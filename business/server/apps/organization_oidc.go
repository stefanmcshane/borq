package apps

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// OrganizationOidc contains provider metadata and the generation-fenced login callback.
type OrganizationOidc struct {
	// AdminEmail is the provider account selected for organization administration.
	AdminEmail string `json:"adminEmail"`
	// IssuerURL is the exact discovery issuer, including an optional tenant path.
	IssuerURL string `json:"issuerUrl"`
	// ClientID identifies this organization's web client at the provider.
	ClientID string `json:"clientId"`
	// BaseURL is the browser-facing Borq origin.
	BaseURL string `json:"borqPublicUrl"`
	// CallbackURL is generated separately for each organization and issuer.
	CallbackURL string `json:"callbackUrl"`
	// CallbackURLs contains the public and configured private redirect URIs to register with the provider.
	CallbackURLs []string `json:"callbackUrls,omitempty"`
	// LogoutCallbackURLs contains the corresponding provider logout return addresses.
	LogoutCallbackURLs []string `json:"logoutCallbackUrls,omitempty"`
	// LogoutCallbackURL is the fixed Borq sign-in page registered for provider logout returns.
	LogoutCallbackURL string `json:"logoutCallbackUrl"`
	// ClientSecretConfigured reports presence without exposing the secret or its vault path.
	ClientSecretConfigured bool `json:"clientSecretConfigured"`
	// Revision guards updates against changes made in another browser.
	Revision int64 `json:"revision"`
	// LastTestedAt is empty until this saved configuration completes a sign-in test.
	LastTestedAt string `json:"lastTestedAt"`
}

// OrganizationOidcInput replaces provider settings while keeping an omitted secret for the same client.
type OrganizationOidcInput struct {
	// AdminEmail selects a verified provider account for organization administration.
	AdminEmail string `json:"adminEmail,omitempty"`
	// IssuerURL identifies the provider, for example a tenant-specific Entra issuer.
	IssuerURL string `json:"issuerUrl"`
	// ClientID is the provider's assigned web client identifier.
	ClientID string `json:"clientId"`
	// ClientSecret is write-only; an empty value keeps credentials for an unchanged issuer and client.
	ClientSecret string `json:"clientSecret,omitempty"`
	// BaseURL is the browser-facing Borq origin, without a path.
	BaseURL string `json:"borqPublicUrl"`
	// Revision is the last read revision, or zero for initial setup.
	Revision int64 `json:"revision"`
}

type organizationOidcRecord struct {
	OrganizationID string           `json:"organizationId"`
	Generation     string           `json:"generation"`
	CredentialsRef string           `json:"credentialsRef"`
	Settings       OrganizationOidc `json:"settings"`
}

func (manager Manager) organizationOidc(ctx context.Context, name string) (organizationOidcRecord, sqlite.Record, error) {
	organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil {
		return organizationOidcRecord{}, sqlite.Record{}, err
	}
	organization, err = manager.ensureOrganizationIdentity(ctx, organization)
	if err != nil {
		return organizationOidcRecord{}, sqlite.Record{}, err
	}
	stored, record, err := loadDocument[organizationOidcRecord](ctx, manager.database, "organization-oidc", organization.ID)
	if errors.Is(err, ErrNotFound) {
		return organizationOidcRecord{OrganizationID: organization.ID}, sqlite.Record{}, nil
	}
	if err == nil && stored.OrganizationID != organization.ID {
		return organizationOidcRecord{}, sqlite.Record{}, errors.New("organization OIDC ownership is invalid")
	}
	stored.Settings.Revision = record.Revision
	if stored.Settings.BaseURL != "" {
		stored.Settings.LogoutCallbackURL = strings.TrimRight(stored.Settings.BaseURL, "/") + "/login"
	}
	if err == nil {
		policy, _, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
		if policyErr == nil {
			stored.Settings.AdminEmail = policy.AdminEmail
		} else if !errors.Is(policyErr, ErrNotFound) {
			return organizationOidcRecord{}, sqlite.Record{}, policyErr
		}
	}
	if err == nil {
		stored.Settings, err = manager.organizationOidcCallbackSettings(ctx, stored.OrganizationID, stored.Settings)
	}
	return stored, record, err
}

// GetOrganizationOidc reads safe settings for one existing organization, including an unconfigured one.
func (manager Manager) GetOrganizationOidc(ctx context.Context, name string) (OrganizationOidc, error) {
	stored, _, err := manager.organizationOidc(ctx, name)
	return stored.Settings, err
}

// PutOrganizationOidc checks discovery and saves a client secret exclusively in management OpenBao.
// Saving a provider invalidates sessions tied to its previous generation;
// AdminEmail selects an administrator without removing existing access.
func (manager Manager) PutOrganizationOidc(ctx context.Context, name string, input OrganizationOidcInput) (OrganizationOidc, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if principal, ok := PrincipalFromContext(ctx); ok && !principal.Admin {
		return OrganizationOidc{}, ErrForbidden
	}
	if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
		return OrganizationOidc{}, organizationOidcError{ErrInvalid, err.Error()}
	}
	if _, _, err := manager.organizationOidc(ctx, name); err != nil {
		return OrganizationOidc{}, err
	}
	organization, record, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil {
		return OrganizationOidc{}, err
	}
	writes, err := manager.stageOrganizationAdminEmail(ctx, organization.ID, input)
	if err != nil {
		return OrganizationOidc{}, err
	}
	policy, _, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
	if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
		return OrganizationOidc{}, policyErr
	}
	if organization.AuthenticationState != "ready" && policy.PocketInstanceID == "" && policy.PocketOrganizationID == "" {
		organization.AuthenticationState = "ready"
		write, err := documentWrite("organization", name, organization, record.Revision)
		if err != nil {
			return OrganizationOidc{}, err
		}
		writes = append(writes, write)
	}
	return manager.putOrganizationOidc(ctx, name, input, writes)
}

// Managed provider setup commits its completion together with the OIDC settings.
// Callers hold runtime.mutex so account enrollment sees a stable provider.
func (manager Manager) putOrganizationOidc(ctx context.Context, name string, input OrganizationOidcInput, accompanying []sqlite.Write) (OrganizationOidc, error) {
	stored, record, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return OrganizationOidc{}, err
	}
	if record.Revision != input.Revision {
		return OrganizationOidc{}, organizationOidcError{ErrConflict, "OIDC settings changed; reload before saving."}
	}
	input.IssuerURL, input.ClientID, input.BaseURL = strings.TrimSpace(input.IssuerURL), strings.TrimSpace(input.ClientID), strings.TrimSpace(input.BaseURL)
	for _, address := range []string{input.IssuerURL, input.BaseURL} {
		if err := oidc.ValidateURL(address); err != nil {
			return OrganizationOidc{}, organizationOidcError{ErrInvalid, err.Error()}
		}
	}
	base, _ := url.Parse(input.BaseURL)
	if (base.Path != "" && base.Path != "/") || base.RawPath != "" {
		return OrganizationOidc{}, organizationOidcError{ErrInvalid, "Enter the Borq address without a path; Borq generates the callback URL."}
	}
	input.BaseURL = strings.TrimRight(input.BaseURL, "/")
	// Distinct redirect URIs identify the expected issuer before a code or PKCE verifier leaves Borq.
	callbackURL := input.BaseURL + "/api/auth/oidc/callback/" + stableId(stored.OrganizationID+"/"+input.IssuerURL)
	if input.ClientID == "" || len(input.ClientID) > 1024 || len(input.ClientSecret) > 8192 || strings.ContainsAny(input.ClientID, "\r\n\x00") {
		return OrganizationOidc{}, organizationOidcError{ErrInvalid, "Enter a valid client ID and client secret."}
	}
	if input.ClientSecret == "" && (stored.CredentialsRef == "" || input.IssuerURL != stored.Settings.IssuerURL || input.ClientID != stored.Settings.ClientID) {
		return OrganizationOidc{}, organizationOidcError{ErrInvalid, "Enter a client secret when setting up or changing the provider or client ID."}
	}
	secret := input.ClientSecret
	if secret == "" {
		secret, err = manager.organizationOidcSecret(ctx, stored)
		if err != nil {
			return OrganizationOidc{}, err
		}
	}
	if _, err := oidc.New(ctx, oidc.Config{Issuer: input.IssuerURL, ClientID: input.ClientID, ClientSecret: secret, RedirectURL: callbackURL}); err != nil {
		return OrganizationOidc{}, organizationOidcError{ErrInvalid, err.Error() + "; saved settings are unchanged."}
	}
	generation, err := newId()
	if err != nil {
		return OrganizationOidc{}, err
	}
	if input.ClientSecret != "" {
		vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return OrganizationOidc{}, errors.New("management secret storage is unavailable; OIDC settings were not saved")
		}
		stored.CredentialsRef = "borq/organization-oidc/" + stored.OrganizationID + "/" + generation
		if _, err := vault.WriteKv(ctx, "secret", stored.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"clientSecret": secret}}); err != nil {
			return OrganizationOidc{}, errors.New("could not store the OIDC client secret; settings were not saved")
		}
	}
	stored.Generation = generation
	stored.Settings = OrganizationOidc{AdminEmail: input.AdminEmail, IssuerURL: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL, CallbackURL: callbackURL, LogoutCallbackURL: input.BaseURL + "/login", ClientSecretConfigured: true}
	stored.Settings, err = manager.organizationOidcCallbackSettings(ctx, stored.OrganizationID, stored.Settings)
	if err != nil {
		return OrganizationOidc{}, err
	}
	write, err := documentWrite("organization-oidc", stored.OrganizationID, stored, record.Revision)
	if err != nil {
		return OrganizationOidc{}, err
	}
	if err := manager.database.Commit(ctx, append(accompanying, write)); err != nil {
		return OrganizationOidc{}, translateStoreError(err)
	}
	stored.Settings.Revision = record.Revision + 1
	return stored.Settings, nil
}

func (manager Manager) organizationOidcSecret(ctx context.Context, stored organizationOidcRecord) (string, error) {
	if !validManagementSecretRef("oidc", stored.OrganizationID, stored.CredentialsRef) {
		return "", errors.New("organization OIDC credential ownership is invalid")
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return "", errors.New("management secret storage is unavailable")
	}
	entry, err := vault.ReadKv(ctx, "secret", stored.CredentialsRef, 1)
	if err != nil || entry.Values["clientSecret"] == "" {
		return "", errors.New("the organization's OIDC client secret is unavailable")
	}
	return entry.Values["clientSecret"], nil
}

// OrganizationOidcTest binds a discovered client to the saved configuration it will verify.
type OrganizationOidcTest struct {
	// Client holds credentials in memory for the duration of the test.
	Client oidc.Client
	// CallbackURL determines the browser return address and cookie security.
	CallbackURL string
	// Generation detects provider changes while the browser visits the IdP.
	Generation string
}

// PrepareOrganizationOidcTest loads the current secret without exposing it over the settings API.
func (manager Manager) PrepareOrganizationOidcTest(ctx context.Context, name, host string) (OrganizationOidcTest, error) {
	stored, _, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return OrganizationOidcTest{}, err
	}
	if stored.Generation == "" {
		return OrganizationOidcTest{}, organizationOidcError{ErrInvalid, "Save this organization's OIDC settings first."}
	}
	callbackURL := stored.Settings.CallbackURL
	if host != "" {
		callbackURL = ""
		for _, candidate := range stored.Settings.CallbackURLs {
			address, err := url.Parse(candidate)
			if err == nil && strings.EqualFold(address.Host, host) {
				callbackURL = candidate
				break
			}
		}
		if callbackURL == "" {
			return OrganizationOidcTest{}, organizationOidcError{ErrInvalid, "Sign-in is unavailable on this address. Open a configured public or private Borq login URL."}
		}
	}
	secret, err := manager.organizationOidcSecret(ctx, stored)
	if err != nil {
		return OrganizationOidcTest{}, err
	}
	client, err := oidc.New(ctx, oidc.Config{Issuer: stored.Settings.IssuerURL, ClientID: stored.Settings.ClientID, ClientSecret: secret, RedirectURL: callbackURL})
	if err != nil {
		return OrganizationOidcTest{}, err
	}
	return OrganizationOidcTest{Client: client, CallbackURL: callbackURL, Generation: stored.Generation}, nil
}

// CompleteOrganizationOidcTest records success only for the same saved provider configuration.
func (manager Manager) CompleteOrganizationOidcTest(ctx context.Context, name, generation string) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	stored, record, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return err
	}
	if generation == "" || stored.Generation != generation {
		return organizationOidcError{ErrConflict, "OIDC settings changed during sign-in; test the saved settings again."}
	}
	stored.Settings.LastTestedAt = time.Now().UTC().Format(time.RFC3339)
	write, err := documentWrite("organization-oidc", stored.OrganizationID, stored, record.Revision)
	if err != nil {
		return err
	}
	return translateStoreError(manager.database.Commit(ctx, []sqlite.Write{write}))
}

// organizationOidcError carries only locally authored, credential-free guidance.
type organizationOidcError struct {
	cause   error
	message string
}

func (err organizationOidcError) Error() string { return err.message }
func (err organizationOidcError) Unwrap() error { return err.cause }

// OrganizationOidcRequestMessage exposes approved setup guidance without underlying provider or vault errors.
func OrganizationOidcRequestMessage(err error) string {
	var problem organizationOidcError
	if errors.As(err, &problem) {
		return problem.message
	}
	return ""
}

// ValidateOrganizationOidcCallback rejects in-flight returns to origins removed from configuration.
func (manager Manager) ValidateOrganizationOidcCallback(ctx context.Context, name, callback string) error {
	stored, _, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return err
	}
	if !slices.Contains(stored.Settings.CallbackURLs, callback) {
		return organizationOidcError{ErrInvalid, "Sign-in addresses changed. Start sign-in again from a configured Borq address."}
	}
	return nil
}
