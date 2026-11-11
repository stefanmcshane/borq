package apps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// OrganizationOnboardingInput optionally selects an identity provider during organization creation.
type OrganizationOnboardingInput struct {
	// AdminEmail selects the existing provider account allowed to administer Borq, without changing provider roles.
	AdminEmail string `json:"adminEmail,omitempty"`
	// Oidc supplies a fully configured external provider instead of managed Pocket ID.
	Oidc *OrganizationOidcInput `json:"oidc,omitempty"`
	// PocketId supplies explicit settings for this organization's new managed instance.
	PocketId *OrganizationPocketIdInput `json:"pocketId,omitempty"`
	// PocketIdOrganization explicitly reuses another organization's managed installation.
	PocketIdOrganization string `json:"pocketIdOrganization,omitempty"`
}

type organizationLoginPolicy struct {
	OrganizationID       string `json:"organizationId"`
	AdminEmail           string `json:"adminEmail"`
	AdminSelectionState  string `json:"adminSelectionState,omitempty"`
	Issuer               string `json:"issuer"`
	ClientID             string `json:"clientId"`
	BaseURL              string `json:"baseUrl"`
	PocketOrganization   string `json:"pocketOrganization,omitempty"`
	PocketOrganizationID string `json:"pocketOrganizationId,omitempty"`
	PocketInstanceID     string `json:"pocketInstanceId,omitempty"`
}

type organizationPocketIdClient struct {
	OrganizationID        string `json:"organizationId"`
	ClientID              string `json:"clientId"`
	CredentialsRef        string `json:"credentialsRef"`
	State                 string `json:"state"`
	ClientLinksConfigured bool   `json:"clientLinksConfigured,omitempty"`
}

// OnboardOrganization creates an organization, optionally configuring or queuing its identity provider.
func (manager Manager) OnboardOrganization(ctx context.Context, name string, input OrganizationOnboardingInput) (Organization, error) {
	if err := validateNames(name); err != nil {
		return Organization{}, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if existing, _, err := loadDocument[Organization](ctx, manager.database, "organization", name); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Organization{}, err
	}
	id, err := newId()
	if err != nil {
		return Organization{}, err
	}
	organization := Organization{Name: name, ID: id, ManagedAddons: true}
	if input.Oidc != nil {
		organization.AuthenticationState = "ready"
	} else if input.PocketId != nil || input.PocketIdOrganization != "" {
		organization.AuthenticationState = "queued"
	}
	writes, err := manager.organizationOnboardingWrites(ctx, organization, input)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return Organization{}, organizationOidcError{ErrInvalid, err.Error()}
		}
		return Organization{}, err
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		if errors.Is(err, sqlite.ErrConflict) {
			return Organization{}, organizationOidcError{ErrConflict, "Organization setup changed or the Pocket ID hostname is already reserved. Reload and choose an unused Pocket ID address."}
		}
		return Organization{}, translateStoreError(err)
	}
	select {
	case manager.runtime.pocketIdWake <- struct{}{}:
	default:
	}
	return organization, nil
}

func (manager Manager) organizationOnboardingWrites(ctx context.Context, organization Organization, input OrganizationOnboardingInput) ([]sqlite.Write, error) {
	if input.Oidc != nil && (input.PocketId != nil || input.PocketIdOrganization != "") || input.PocketId != nil && input.PocketIdOrganization != "" {
		return nil, fmt.Errorf("%w: choose one organization identity provider", ErrInvalid)
	}
	organizationWrite, err := documentWrite("organization", organization.Name, organization, 0)
	if err != nil {
		return nil, err
	}
	writes := []sqlite.Write{organizationWrite}
	if input.Oidc != nil {
		if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
			return nil, err
		}
		provider, err := manager.stageOrganizationProvider(ctx, organization, *input.Oidc)
		if err != nil {
			return nil, err
		}
		policy := organizationLoginPolicy{OrganizationID: organization.ID, AdminEmail: strings.TrimSpace(input.AdminEmail), AdminSelectionState: "pending", Issuer: provider.Settings.IssuerURL, ClientID: provider.Settings.ClientID, BaseURL: provider.Settings.BaseURL}
		providerWrite, err := documentWrite("organization-oidc", organization.ID, provider, 0)
		if err != nil {
			return nil, err
		}
		policyWrite, err := documentWrite("organization-login-policy", organization.ID, policy, 0)
		return append(writes, providerWrite, policyWrite), err
	}
	if input.PocketId != nil {
		return manager.newOrganizationPocketIdWrites(ctx, organization, *input.PocketId, input.AdminEmail, writes)
	}
	if input.PocketIdOrganization == "" {
		if input.AdminEmail != "" {
			return nil, fmt.Errorf("%w: adminEmail requires an identity provider; supply oidc, pocketId or pocketIdOrganization", ErrInvalid)
		}
		return writes, nil
	}
	source, err := manager.organizationPocketIdSource(ctx, input.PocketIdOrganization)
	if err != nil {
		return nil, err
	}
	adminEmail := strings.TrimSpace(input.AdminEmail)
	if adminEmail == "" {
		adminEmail = source.Status.AdminEmail
	}
	if err := validateOrganizationAdminEmail(adminEmail); err != nil {
		return nil, err
	}
	clientID, err := newId()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	credentialsRef, err := manager.stageManagementSecret(ctx, "oidc", organization.ID, "clientSecret", base64.RawURLEncoding.EncodeToString(secret))
	if err != nil {
		return nil, err
	}
	policy := organizationLoginPolicy{OrganizationID: organization.ID, AdminEmail: adminEmail, Issuer: source.Status.IssuerURL,
		ClientID: clientID, BaseURL: source.Status.BaseURL, PocketOrganization: source.Organization, PocketOrganizationID: source.OrganizationID, PocketInstanceID: source.ID}
	policyWrite, err := documentWrite("organization-login-policy", organization.ID, policy, 0)
	if err != nil {
		return nil, err
	}
	clientWrite, err := documentWrite("organization-pocketid-client", organization.ID, organizationPocketIdClient{OrganizationID: organization.ID, ClientID: clientID, CredentialsRef: credentialsRef, State: "queued"}, 0)
	return append(writes, policyWrite, clientWrite), err
}

func validateOrganizationAdminEmail(email string) error {
	email = strings.TrimSpace(email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 {
		return fmt.Errorf("%w: enter the initial administrator's email address", ErrInvalid)
	}
	return nil
}

func (manager Manager) stageOrganizationProvider(ctx context.Context, organization Organization, input OrganizationOidcInput) (organizationOidcRecord, error) {
	input.IssuerURL, input.ClientID, input.BaseURL = strings.TrimSpace(input.IssuerURL), strings.TrimSpace(input.ClientID), strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	base, err := url.Parse(input.BaseURL)
	if err != nil || base.Path != "" || base.RawPath != "" || len(input.ClientID) > 1024 || len(input.ClientSecret) > 8192 || strings.ContainsAny(input.ClientID, "\r\n\x00") {
		return organizationOidcRecord{}, fmt.Errorf("%w: enter a Borq origin without a path and valid OIDC credentials", ErrInvalid)
	}
	callback := input.BaseURL + "/api/auth/oidc/callback/" + stableId(organization.ID+"/"+input.IssuerURL)
	if _, err := oidc.New(ctx, oidc.Config{Issuer: input.IssuerURL, ClientID: input.ClientID, ClientSecret: input.ClientSecret, RedirectURL: callback}); err != nil {
		return organizationOidcRecord{}, organizationOidcError{ErrInvalid, "Could not verify the organization's OIDC provider. Check its issuer, client credentials and public Borq address."}
	}
	generation, err := newId()
	if err != nil {
		return organizationOidcRecord{}, err
	}
	ref, err := manager.stageManagementSecret(ctx, "oidc", organization.ID, "clientSecret", input.ClientSecret)
	if err != nil {
		return organizationOidcRecord{}, err
	}
	return organizationOidcRecord{OrganizationID: organization.ID, Generation: generation, CredentialsRef: ref,
		Settings: OrganizationOidc{IssuerURL: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL, CallbackURL: callback, ClientSecretConfigured: true}}, nil
}

func (manager Manager) organizationPocketIdSource(ctx context.Context, name string) (organizationPocketIdRecord, error) {
	if err := validateNames(name); err != nil {
		return organizationPocketIdRecord{}, fmt.Errorf("%w: explicitly select the organization owning the Pocket ID instance to reuse", ErrInvalid)
	}
	organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil {
		return organizationPocketIdRecord{}, fmt.Errorf("%w: the selected Pocket ID organization is unavailable", ErrInvalid)
	}
	stored, _, err := manager.readPocketId(ctx, organization.ID)
	if err != nil || stored.Status.State != "ready" {
		return organizationPocketIdRecord{}, fmt.Errorf("%w: the selected managed Pocket ID instance is not ready", ErrInvalid)
	}
	return stored, nil
}

func (manager Manager) newOrganizationPocketIdWrites(ctx context.Context, organization Organization, input OrganizationPocketIdInput, adminEmail string, writes []sqlite.Write) ([]sqlite.Write, error) {
	if !manager.pocketIdAvailable() {
		return nil, fmt.Errorf("%w: managed Pocket ID requires a management cluster", ErrInvalid)
	}
	input.IssuerURL, input.BaseURL = strings.TrimRight(strings.TrimSpace(input.IssuerURL), "/"), strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.AdminUsername, input.AdminEmail = pocketIdAdminUsername(input.AdminUsername, input.AdminEmail), strings.TrimSpace(input.AdminEmail)
	if err := validatePocketIdInput(input); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	adminEmail = strings.TrimSpace(adminEmail)
	if adminEmail == "" {
		adminEmail = input.AdminEmail
	}
	if err := validateOrganizationAdminEmail(adminEmail); err != nil {
		return nil, err
	}
	if input.Configuration != nil || input.Env != nil || input.OidcRevision != 0 {
		return nil, fmt.Errorf("%w: configure Pocket ID environment after accepting the new organization's initial setup", ErrInvalid)
	}
	if err := manager.validateNetworkPocketIdName(input.IssuerURL); err != nil {
		return nil, err
	}
	id, err := newId()
	if err != nil {
		return nil, err
	}
	pocket := organizationPocketIdRecord{ID: id, Organization: organization.Name, OrganizationID: organization.ID,
		Status: OrganizationPocketId{Available: true, State: "queued", Message: "Pocket ID setup is queued.", IssuerURL: input.IssuerURL,
			BaseURL: input.BaseURL, CertificateIssuer: input.CertificateIssuer, AdminUsername: input.AdminUsername, AdminEmail: input.AdminEmail}}
	pocketWrite, err := documentWrite("organization-pocketid", organization.ID, pocket, 0)
	if err != nil {
		return nil, err
	}
	policy := organizationLoginPolicy{OrganizationID: organization.ID, AdminEmail: adminEmail, Issuer: input.IssuerURL,
		ClientID: id, BaseURL: input.BaseURL, PocketInstanceID: id}
	policyWrite, err := documentWrite("organization-login-policy", organization.ID, policy, 0)
	if err != nil {
		return nil, err
	}
	issuer, _ := url.Parse(input.IssuerURL)
	reservation, err := documentWrite("organization-pocketid-host", issuer.Hostname(), map[string]string{"organizationId": organization.ID, "instanceId": id}, 0)
	return append(writes, pocketWrite, policyWrite, reservation), err
}

func (manager Manager) provisionOrganizationPocketIdClients(ctx context.Context) {
	records, err := manager.database.List(ctx, "organization-pocketid-client")
	if err != nil {
		return
	}
	for _, record := range records {
		var pending organizationPocketIdClient
		if json.Unmarshal(record.Value, &pending) != nil || pending.OrganizationID != record.Key ||
			(pending.State != "queued" && (pending.State != "ready" || pending.ClientLinksConfigured)) {
			continue
		}
		_ = manager.provisionOrganizationPocketIdClient(ctx, pending, record.Revision)
	}
}

func (manager Manager) provisionOrganizationPocketIdClient(ctx context.Context, pending organizationPocketIdClient, revision int64) error {
	policy, _, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", pending.OrganizationID)
	if err != nil || policy.OrganizationID != pending.OrganizationID || policy.ClientID != pending.ClientID {
		return organizationLoginDenied()
	}
	source, _, err := manager.readPocketId(ctx, policy.PocketOrganizationID)
	if err != nil || source.Organization != policy.PocketOrganization || source.ID != policy.PocketInstanceID || source.Status.State != "ready" ||
		source.Status.IssuerURL != policy.Issuer || source.Status.BaseURL != policy.BaseURL {
		return organizationLoginDenied()
	}
	organizations, err := manager.ListOrganizations(ctx)
	if err != nil {
		return err
	}
	name := ""
	for _, organization := range organizations {
		if organization.ID == pending.OrganizationID {
			name = organization.Name
		}
	}
	if name == "" {
		return organizationLoginDenied()
	}
	credentials, err := manager.pocketIdCredentials(ctx, source)
	if err != nil {
		return err
	}
	client, err := manager.pocketIdClient(source, credentials["apiKey"])
	if err != nil {
		return err
	}
	clientName := "Borq " + name
	if len(clientName) > 50 {
		clientName = clientName[:50]
	}
	callback := policy.BaseURL + "/api/auth/oidc/callback/" + stableId(pending.OrganizationID+"/"+policy.Issuer)
	input := pocketid.OidcClient{ID: pending.ClientID, Name: clientName, CallbackURL: callback,
		LogoutCallbackURL: policy.BaseURL + "/login", LaunchURL: policy.BaseURL + "/"}
	if pending.State == "ready" {
		if err := client.EnsureClientLinks(ctx, input); err != nil {
			return err
		}
		pending.ClientLinksConfigured = true
		write, err := documentWrite("organization-pocketid-client", pending.OrganizationID, pending, revision)
		if err != nil {
			return err
		}
		return manager.database.Commit(ctx, []sqlite.Write{write})
	}
	secret, err := manager.readManagementSecret(ctx, "oidc", pending.OrganizationID, pending.CredentialsRef, "clientSecret")
	if err != nil {
		return err
	}
	input.Secret = secret
	if err := client.EnsureClient(ctx, input); err != nil {
		return err
	}
	pending.State = "ready"
	pending.ClientLinksConfigured = true
	write, err := documentWrite("organization-pocketid-client", pending.OrganizationID, pending, revision)
	if err != nil {
		return err
	}
	organization, organizationRecord, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil || organization.ID != pending.OrganizationID {
		return organizationLoginDenied()
	}
	organization.AuthenticationState = "ready"
	organizationWrite, err := documentWrite("organization", name, organization, organizationRecord.Revision)
	if err != nil {
		return err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	_, err = manager.putOrganizationOidc(ctx, name, OrganizationOidcInput{IssuerURL: policy.Issuer, ClientID: policy.ClientID,
		ClientSecret: secret, BaseURL: policy.BaseURL}, []sqlite.Write{write, organizationWrite})
	return err
}
