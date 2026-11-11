package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
)

func managementPocketIdInput(addon ManagementPocketId, oidc ManagementOidc, revision int64) OrganizationPocketIdInput {
	return OrganizationPocketIdInput{IssuerURL: strings.TrimRight(strings.TrimSpace(addon.IssuerURL), "/"), BaseURL: strings.TrimRight(strings.TrimSpace(oidc.BaseURL), "/"), CertificateIssuer: strings.TrimSpace(addon.CertificateIssuer), AdminUsername: pocketIdAdminUsername(addon.AdminUsername, addon.AdminEmail), AdminEmail: strings.TrimSpace(addon.AdminEmail), OidcRevision: revision}
}

func matchManagementPocketId(stored organizationPocketIdRecord, addon ManagementPocketId, oidc ManagementOidc) error {
	input := managementPocketIdInput(addon, oidc, 0)

	if stored.Status.IssuerURL != input.IssuerURL {
		return organizationOidcError{ErrConflict, "This organization already has managed Pocket ID with a different issuerUrl. Its address is fixed after installation; use the existing address or create a new organization for a new instance."}
	}
	if stored.Status.BaseURL != input.BaseURL {
		return organizationOidcError{ErrConflict, "This organization's managed Pocket ID already has a different borqPublicUrl. Keep its existing Borq address or create a new organization for a new instance."}
	}
	if stored.Status.AdminUsername != input.AdminUsername || stored.Status.AdminEmail != input.AdminEmail {
		return organizationOidcError{ErrConflict, "This organization's managed Pocket ID already has a different initial administrator. Keep the existing administrator settings when reapplying."}
	}
	if stored.Status.CertificateIssuer != input.CertificateIssuer && stored.Status.State != "failed" {
		return organizationOidcError{ErrConflict, "Managed Pocket ID certificate issuer changes require a failed setup. Keep the existing certificateIssuer for this running instance."}
	}
	return nil
}

func (manager Manager) readPocketIdValues(ctx context.Context, organizationID, ref string) (map[string]any, error) {
	if ref == "" {
		return nil, nil
	}
	encoded, err := manager.readManagementSecret(ctx, "pocketid-values", organizationID, ref, "values")
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if json.Unmarshal([]byte(encoded), &values) != nil || platform.ValidatePocketIdValues(values) != nil {
		return nil, fmt.Errorf("%w: stored managed Pocket ID workload values are invalid", ErrInvalid)
	}
	return values, nil
}

func (manager Manager) applyManagementPocketId(ctx context.Context, action managementAction, accepted managementAuthentication, environment pocketIdEnvironment) error {
	if _, _, err := manager.managementOidcOwner(ctx, action.organization.Name, accepted); err != nil {
		return err
	}
	input := managementPocketIdInput(action.pocketId, action.organization.oidcInput(), accepted.Revision)
	if _, err := manager.setupOrganizationPocketId(ctx, action.organization.Name, input, &action.pocketId, environment); err != nil {
		return err
	}
	ticker := time.NewTicker(manager.runtime.pollInterval)
	defer ticker.Stop()
	for {
		stored, _, err := manager.readPocketId(ctx, accepted.OrganizationID)
		if err != nil {
			return err
		}
		if err := matchManagementPocketId(stored, action.pocketId, action.organization.oidcInput()); err != nil {
			return err
		}
		if stored.ValuesRef != action.pocketId.ValuesRef {
			return fmt.Errorf("%w: managed Pocket ID values changed during apply", ErrConflict)
		}
		if environment.Name != "" && stored.Environment != environment {
			return fmt.Errorf("%w: managed Pocket ID environment changed during apply", ErrConflict)
		}
		if stored.Status.State == "failed" {
			return errors.New("managed Pocket ID setup failed; inspect its status in Admin and reapply after correcting the cause")
		}
		if stored.Status.State == "ready" && stored.BackupConfigured && stored.ConfigurationApplied && stored.AppliedValuesRef == stored.ValuesRef && stored.AppliedEnvironmentRef == stored.Environment.CredentialsRef {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager Manager) matchImplicitManagementPocketId(ctx context.Context, name string, accepted managementAuthentication, pocket organizationPocketIdRecord) error {
	provider, record, err := manager.managementOidcOwner(ctx, name, accepted)
	if err != nil {
		return err
	}
	policy, _, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", accepted.OrganizationID)
	if err != nil {
		return err
	}
	if pocket.ID != accepted.ImplicitPocketID || pocket.OrganizationID != accepted.OrganizationID ||
		policy.OrganizationID != accepted.OrganizationID || policy.PocketOrganizationID != "" ||
		policy.PocketInstanceID != pocket.ID || policy.ClientID != pocket.ID ||
		policy.Issuer != pocket.Status.IssuerURL || policy.BaseURL != pocket.Status.BaseURL {
		return fmt.Errorf("%w: organization identity provider changed during management apply", ErrConflict)
	}
	completed := pocket.Status.State == "ready" && provider.Settings.ClientID == pocket.ID &&
		provider.Settings.IssuerURL == pocket.Status.IssuerURL && provider.Settings.BaseURL == pocket.Status.BaseURL
	if !completed && (record.Revision != accepted.Revision || pocket.OidcRevision != accepted.Revision) {
		return fmt.Errorf("%w: organization OIDC settings changed during management apply", ErrConflict)
	}
	return nil
}

func (manager Manager) waitImplicitManagementPocketId(ctx context.Context, name string, accepted managementAuthentication) error {
	ticker := time.NewTicker(manager.runtime.pollInterval)
	defer ticker.Stop()
	if accepted.ImplicitPocketID == "" {
		shared := false
		for {
			organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", name)
			if err != nil {
				return err
			}
			if accepted.OrganizationID != "" && organization.ID != accepted.OrganizationID {
				return fmt.Errorf("%w: organization identity changed during management apply", ErrConflict)
			}
			if organization.AuthenticationState != "queued" {
				if shared {
					_, _, err := manager.organizationLoginProvider(ctx, name)
					return err
				}
				return nil
			}
			client, _, err := loadDocument[organizationPocketIdClient](ctx, manager.database, "organization-pocketid-client", organization.ID)
			if accepted.OrganizationID == "" || errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: reapply management configuration to finish organization authentication setup", ErrComponentsNotReady)
			}
			if err != nil {
				return err
			}
			if client.State == "failed" {
				return fmt.Errorf("%w: shared Pocket ID client setup failed", ErrComponentsNotReady)
			}
			shared = true
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	for {
		pocket, _, err := manager.readPocketId(ctx, accepted.OrganizationID)
		if err != nil {
			return err
		}
		if err := manager.matchImplicitManagementPocketId(ctx, name, accepted, pocket); err != nil {
			return err
		}
		if pocket.Status.State == "failed" {
			return fmt.Errorf("%w: managed Pocket ID setup failed; correct its setup and reapply management configuration", ErrComponentsNotReady)
		}
		if pocket.Status.State == "ready" && pocket.BackupConfigured && pocket.ConfigurationApplied &&
			pocket.AppliedValuesRef == pocket.ValuesRef && pocket.AppliedEnvironmentRef == pocket.Environment.CredentialsRef {
			_, _, err := manager.organizationLoginProvider(ctx, name)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager Manager) exportManagementAuthentication(ctx context.Context, organization Organization, entry *ManagementOrganization) error {
	stored, _, err := manager.organizationOidc(ctx, organization.Name)
	if err != nil {
		return err
	}
	if stored.Settings.IssuerURL != "" {
		entry.Oidc = &ManagementOidc{IssuerURL: stored.Settings.IssuerURL, ClientID: stored.Settings.ClientID, BaseURL: stored.Settings.BaseURL, ClientSecretRef: stored.CredentialsRef}
		policy, _, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
		if err == nil && policy.OrganizationID == organization.ID {
			entry.Oidc.AdminEmail = policy.AdminEmail
			admin, _, adminErr := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organization.ID)
			if adminErr == nil && admin.OrganizationID == organization.ID && admin.Issuer == stored.Settings.IssuerURL && policy.AdminSelectionState != "pending" && (admin.Email == "" || strings.EqualFold(admin.Email, policy.AdminEmail)) {
				entry.Oidc.AdminSubject = admin.Subject
			} else if adminErr != nil && !errors.Is(adminErr, ErrNotFound) {
				return adminErr
			}
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	pocket, _, err := manager.readPocketId(ctx, stored.OrganizationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// A failed initial setup still needs a complete declarative retry document.
	if entry.Oidc == nil {
		entry.Oidc = &ManagementOidc{BaseURL: pocket.Status.BaseURL}
	}
	// If Admin selected another provider, export the current OIDC independently;
	// an omitted addon preserves its instance rather than reattaching that provider.
	if entry.Oidc.IssuerURL != "" && (entry.Oidc.IssuerURL != pocket.Status.IssuerURL || entry.Oidc.ClientID != pocket.ID) {
		return nil
	}
	entry.ManagedPocketId = &ManagementPocketId{IssuerURL: pocket.Status.IssuerURL, BaseURL: pocket.Status.BaseURL, CertificateIssuer: pocket.Status.CertificateIssuer, AdminUsername: pocket.Status.AdminUsername, AdminEmail: pocket.Status.AdminEmail, ValuesRef: pocket.ValuesRef, Env: pocket.Environment.reference(false)}
	if entry.Oidc.AdminEmail != "" && !strings.EqualFold(entry.Oidc.AdminEmail, pocket.Status.AdminEmail) {
		entry.ManagedPocketId.OrganizationAdminEmail = entry.Oidc.AdminEmail
	}
	entry.Oidc = nil
	return nil
}
