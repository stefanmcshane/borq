package apps

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/oidc"
)

// OrganizationLogin binds a browser session to an enrolled account and saved provider.
type OrganizationLogin struct {
	// Organization selects the organization by its current name.
	Organization string
	// OrganizationID prevents a recreated organization from inheriting an old session.
	OrganizationID string
	// Issuer and Subject identify the verified administrator account independently of email.
	Issuer, Subject string
	// Generation invalidates the session when the saved provider configuration changes.
	Generation string
	// ExpiresAt prevents the session from outliving its verified identity.
	ExpiresAt time.Time
	// MemberID identifies an authenticated member; empty retains the initial administrator binding.
	MemberID string
	// MembershipVersion prevents a revoked credential from reviving after membership changes.
	MembershipVersion string
}

type organizationLoginAdmin struct {
	OrganizationID    string       `json:"organizationId"`
	Issuer            string       `json:"issuer"`
	Subject           string       `json:"subject"`
	Email             string       `json:"email,omitempty"`
	Permissions       []Permission `json:"permissions"`
	RoleIDs           []string     `json:"roleIds"`
	EmailVerified     bool         `json:"emailVerified"`
	Disabled          bool         `json:"disabled,omitempty"`
	CredentialVersion string       `json:"credentialVersion,omitempty"`
}

func organizationLoginDenied() error {
	return organizationOidcError{ErrInvalid, "Organization sign-in is unavailable for this organization or account. Use administrator recovery access to review authentication settings."}
}

// ListOrganizationSignInNames is the public login selector's name-only projection.
// It checks saved provider bindings without contacting providers or reading credentials.
func (manager Manager) ListOrganizationSignInNames(ctx context.Context) ([]string, error) {
	organizations, err := manager.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, organization := range organizations {
		if _, _, err := manager.organizationLoginProvider(ctx, organization.Name); err == nil {
			names = append(names, organization.Name)
		}
	}
	return names, nil
}

func (manager Manager) organizationLoginProvider(ctx context.Context, name string) (organizationOidcRecord, organizationLoginPolicy, error) {
	provider, _, err := manager.organizationOidc(ctx, name)
	if err != nil || provider.Generation == "" {
		return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
	}
	policy, _, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", provider.OrganizationID)
	managedProvider := false
	if errors.Is(err, ErrNotFound) {
		pocket, _, pocketErr := manager.readPocketId(ctx, provider.OrganizationID)
		if pocketErr != nil || pocket.Status.State != "ready" || pocket.Organization != name {
			return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
		}
		policy = organizationLoginPolicy{OrganizationID: provider.OrganizationID, AdminEmail: pocket.Status.AdminEmail,
			Issuer: pocket.Status.IssuerURL, ClientID: pocket.ID, BaseURL: pocket.Status.BaseURL}
		managedProvider = true
	} else if err != nil {
		return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
	} else if policy.PocketInstanceID != "" && policy.PocketOrganizationID == "" {
		pocket, _, pocketErr := manager.readPocketId(ctx, provider.OrganizationID)
		if pocketErr != nil || pocket.Status.State != "ready" || pocket.Organization != name || pocket.ID != policy.PocketInstanceID ||
			policy.ClientID != pocket.ID || pocket.Status.IssuerURL != policy.Issuer || pocket.Status.BaseURL != policy.BaseURL {
			return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
		}
		managedProvider = true
	}
	if policy.OrganizationID != provider.OrganizationID || validateOrganizationAdminEmail(policy.AdminEmail) != nil ||
		provider.Settings.IssuerURL != policy.Issuer {
		return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
	}
	if (managedProvider || policy.PocketOrganizationID != "") && (provider.Settings.ClientID != policy.ClientID || provider.Settings.BaseURL != policy.BaseURL) {
		return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
	}
	if policy.PocketOrganizationID != "" {
		source, _, sourceErr := manager.readPocketId(ctx, policy.PocketOrganizationID)
		client, _, clientErr := loadDocument[organizationPocketIdClient](ctx, manager.database, "organization-pocketid-client", provider.OrganizationID)
		if sourceErr != nil || clientErr != nil || source.Status.State != "ready" || source.Organization != policy.PocketOrganization ||
			source.ID != policy.PocketInstanceID || source.Status.IssuerURL != policy.Issuer || source.Status.BaseURL != policy.BaseURL ||
			client.OrganizationID != provider.OrganizationID || client.ClientID != policy.ClientID || client.State != "ready" {
			return organizationOidcRecord{}, organizationLoginPolicy{}, organizationLoginDenied()
		}
	}
	return provider, policy, nil
}

// PrepareOrganizationLogin discovers the provider registered by this organization's administrator policy.
func (manager Manager) PrepareOrganizationLogin(ctx context.Context, name, host string) (OrganizationOidcTest, error) {
	provider, _, err := manager.organizationLoginProvider(ctx, name)
	if err != nil {
		return OrganizationOidcTest{}, err
	}
	prepared, err := manager.PrepareOrganizationOidcTest(ctx, name, host)
	if err != nil {
		if OrganizationOidcRequestMessage(err) != "" {
			return OrganizationOidcTest{}, err
		}
		return OrganizationOidcTest{}, organizationLoginDenied()
	}
	current, _, err := manager.organizationLoginProvider(ctx, name)
	if err != nil || current.OrganizationID != provider.OrganizationID || current.Generation != provider.Generation || prepared.Generation != provider.Generation {
		return OrganizationOidcTest{}, organizationLoginDenied()
	}
	return prepared, nil
}

// AuthorizeOrganizationLogin enrolls the initial administrator once, then authorizes its stable provider identity.
// Identity must come from the OIDC client's verified code exchange.
func (manager Manager) AuthorizeOrganizationLogin(ctx context.Context, name, generation string, identity oidc.Identity) (OrganizationLogin, error) {
	// Management acceptance must not change the selected account between its policy read and first pin.
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	provider, policy, err := manager.organizationLoginProvider(ctx, name)
	if err != nil {
		return OrganizationLogin{}, err
	}
	if generation == "" || generation != provider.Generation || identity.Subject == "" ||
		identity.Issuer != policy.Issuer || !identity.ExpiresAt.After(time.Now()) {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	login := OrganizationLogin{Organization: name, OrganizationID: provider.OrganizationID, Issuer: identity.Issuer,
		Subject: identity.Subject, Generation: generation, ExpiresAt: identity.ExpiresAt}
	admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err == nil && (admin.OrganizationID != provider.OrganizationID || admin.Issuer != policy.Issuer || admin.Subject == "") {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	adminExists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	isAdministrator := adminExists && admin.Issuer == identity.Issuer && admin.Subject == identity.Subject
	if isAdministrator && admin.Disabled {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	members, memberErr := manager.organizationMembers(ctx, provider.OrganizationID)
	if memberErr != nil {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	selected := policy.AdminSelectionState == "pending" || policy.AdminSelectionState == "" && !adminExists
	matchingEmail := strings.EqualFold(strings.TrimSpace(identity.Email), strings.TrimSpace(policy.AdminEmail))
	memberExists := false
	for _, member := range members {
		if member.Member.Issuer == identity.Issuer && member.Member.Subject == identity.Subject {
			memberExists = true
			if member.Member.State != "active" {
				return OrganizationLogin{}, organizationLoginDenied()
			}
		}
	}
	// Existing legacy memberships require an explicit selection change before promotion.
	if selected && matchingEmail && (policy.AdminSelectionState == "pending" || !memberExists) {
		if !identity.EmailVerified && !manager.isProvisionedPocketIdAdministrator(ctx, provider, policy, identity) {
			message := "The identity provider did not verify the selected administrator's email."
			if issuer, err := url.Parse(identity.Issuer); err == nil && issuer.Host == "login.microsoftonline.com" {
				message += " Include email and xms_edov in the Microsoft application's ID-token optional claims, then sign in again."
			}
			return OrganizationLogin{}, organizationOidcError{ErrForbidden, message}
		}
		if !adminExists && !memberExists {
			return manager.enrollSelectedOrganizationAdministrator(ctx, provider, policy, identity, login, true)
		}
		if isAdministrator {
			login.MembershipVersion = admin.CredentialVersion
		} else {
			login, err = manager.authorizeOrganizationMemberLogin(ctx, provider, identity, login, members)
			if err != nil {
				return OrganizationLogin{}, err
			}
		}
		return manager.enrollSelectedOrganizationAdministrator(ctx, provider, policy, identity, login, false)
	}
	if isAdministrator {
		login.MembershipVersion = admin.CredentialVersion
		return login, nil
	}
	return manager.authorizeOrganizationMemberLogin(ctx, provider, identity, login, members)
}

func (manager Manager) isProvisionedPocketIdAdministrator(ctx context.Context, provider organizationOidcRecord, policy organizationLoginPolicy, identity oidc.Identity) bool {
	if policy.PocketOrganizationID != "" {
		return false
	}
	pocket, _, err := manager.readPocketId(ctx, policy.OrganizationID)
	// The operator selected this exact subject when creating the managed provider.
	// Only that subject may claim initial administrator access without a verified email.
	return err == nil && pocket.Status.State == "ready" && (policy.PocketInstanceID == "" || pocket.ID == policy.PocketInstanceID) &&
		policy.ClientID == pocket.ID && policy.Issuer == pocket.Status.IssuerURL && policy.BaseURL == pocket.Status.BaseURL &&
		provider.Settings.ClientID == pocket.ID && provider.Settings.IssuerURL == pocket.Status.IssuerURL && provider.Settings.BaseURL == pocket.Status.BaseURL &&
		identity.Subject == pocket.adminID() && strings.EqualFold(strings.TrimSpace(identity.Email), policy.AdminEmail) &&
		strings.EqualFold(policy.AdminEmail, pocket.Status.AdminEmail)
}

// ValidateOrganizationLogin checks durable authorization without contacting the identity provider.
func (manager Manager) ValidateOrganizationLogin(ctx context.Context, login OrganizationLogin) error {
	if login.OrganizationID == "" || login.Subject == "" || !login.ExpiresAt.After(time.Now()) {
		return organizationLoginDenied()
	}
	provider, _, err := manager.organizationLoginProvider(ctx, login.Organization)
	if err != nil || provider.OrganizationID != login.OrganizationID || provider.Generation != login.Generation || provider.Settings.IssuerURL != login.Issuer {
		return organizationLoginDenied()
	}
	if login.MemberID != "" {
		member, _, err := loadDocument[organizationMemberRecord](ctx, manager.database, "organization-member", login.MemberID)
		if err != nil || member.OrganizationID != login.OrganizationID || member.Member.State != "active" || member.Member.Issuer != login.Issuer || member.Member.Subject != login.Subject || member.CredentialVersion == "" || member.CredentialVersion != login.MembershipVersion {
			return organizationLoginDenied()
		}
		return nil
	}
	admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", login.OrganizationID)
	if err != nil || admin.OrganizationID != login.OrganizationID || admin.Issuer != login.Issuer || admin.Subject != login.Subject || admin.Disabled || admin.CredentialVersion != login.MembershipVersion {
		return organizationLoginDenied()
	}
	return nil
}
