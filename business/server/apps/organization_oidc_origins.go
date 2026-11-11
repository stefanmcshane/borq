package apps

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/oidc"
)

// Only configured origins can receive authorization codes; request headers never add origins.
func (manager Manager) organizationOidcOrigins(ctx context.Context, public string) ([]string, error) {
	origins := []string{strings.TrimRight(public, "/")}
	network := manager.runtime.platformSettings.Network
	configuration, _, err := loadDocument[managementNetworkConfiguration](ctx, manager.database, "management-network-settings", "shared")
	if err == nil {
		network = configuration.Network
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if network.Tailscale == nil {
		return origins, nil
	}
	state, _, err := loadDocument[platform.NetworkState](ctx, manager.database, "management-network-state", "shared")
	if err == nil {
		network.ManagementURL = state.ManagementURL
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	private := strings.TrimRight(network.ManagementURL, "/")
	if private == "" || strings.EqualFold(private, origins[0]) {
		return origins, nil
	}
	address, err := url.Parse(private)
	if err != nil || oidc.ValidateURL(private) != nil || address.Scheme != "https" || address.Path != "" || address.RawPath != "" {
		return nil, organizationOidcError{ErrInvalid, "The private management HTTPS address is invalid. Check management network settings."}
	}
	return append(origins, private), nil
}

func (manager Manager) organizationOidcCallbackSettings(ctx context.Context, organizationID string, settings OrganizationOidc) (OrganizationOidc, error) {
	settings.CallbackURLs, settings.LogoutCallbackURLs = []string{}, []string{}
	if settings.CallbackURL == "" {
		return settings, nil
	}
	origins := []string{strings.TrimRight(settings.BaseURL, "/")}
	policy, _, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organizationID)
	if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
		return OrganizationOidc{}, policyErr
	}
	managed := policy.PocketInstanceID != "" || policy.PocketOrganizationID != ""
	if !managed {
		_, _, pocketErr := manager.readPocketId(ctx, organizationID)
		if pocketErr == nil {
			managed = true
		} else if !errors.Is(pocketErr, ErrNotFound) {
			return OrganizationOidc{}, pocketErr
		}
	}
	// Automatically managed clients currently register their canonical address only.
	if !managed {
		var err error
		origins, err = manager.organizationOidcOrigins(ctx, settings.BaseURL)
		if err != nil {
			return OrganizationOidc{}, err
		}
	}
	callback, err := url.Parse(settings.CallbackURL)
	if err != nil {
		return OrganizationOidc{}, err
	}
	for _, origin := range origins {
		settings.CallbackURLs = append(settings.CallbackURLs, origin+callback.EscapedPath())
		settings.LogoutCallbackURLs = append(settings.LogoutCallbackURLs, origin+"/login")
	}
	return settings, nil
}
