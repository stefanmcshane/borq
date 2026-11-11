package apps

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func (manager Manager) normalizeManagementIdentity(ctx context.Context, configuration ManagementConfiguration) (ManagementConfiguration, error) {
	configuration.Organizations = slices.Clone(configuration.Organizations)
	publicURL := strings.TrimRight(strings.TrimSpace(configuration.BorqPublicURL), "/")
	if publicURL == "" {
		settings, err := manager.GetPlatformSettings(ctx)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		publicURL = settings.BorqPublicURL
	}
	for index := range configuration.Organizations {
		organization := &configuration.Organizations[index]
		if organization.Oidc != nil && organization.ManagedPocketId != nil {
			return ManagementConfiguration{}, fmt.Errorf("%w: organization %s must declare oidc or managedPocketId, not both", ErrInvalid, organization.Name)
		}
		if organization.Oidc != nil {
			input := *organization.Oidc
			if input.BaseURL == "" {
				input.BaseURL = publicURL
			}
			organization.Oidc = &input
		}
		if organization.ManagedPocketId == nil {
			continue
		}
		addon := *organization.ManagedPocketId
		if addon.BaseURL == "" {
			addon.BaseURL = publicURL
		}
		if addon.CertificateIssuer == "" && strings.HasPrefix(strings.TrimSpace(addon.IssuerURL), "https://") {
			addon.CertificateIssuer = "letsencrypt-cloudflare"
		}
		organization.ManagedPocketId = &addon
	}
	return configuration, nil
}

func (organization ManagementOrganization) oidcInput() ManagementOidc {
	if organization.Oidc != nil {
		return *organization.Oidc
	}
	addon := organization.ManagedPocketId
	adminEmail := addon.OrganizationAdminEmail
	if adminEmail == "" {
		adminEmail = addon.AdminEmail
	}
	return ManagementOidc{AdminEmail: adminEmail, BaseURL: addon.BaseURL, IssuerURL: addon.IssuerURL}
}
