package apps

import (
	"context"
	"errors"
	"strings"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// OrganizationOidcAdministratorInput selects an immutable account at the saved provider.
type OrganizationOidcAdministratorInput struct {
	// Subject is the exact Account ID shown by Borq's successful Test sign-in.
	Subject string `json:"subject"`
	// Revision fences enrollment against a change to the saved provider settings.
	Revision int64 `json:"revision"`
}

// EnrollOrganizationOidcAdministrator explicitly pins the first administrator with
// installation recovery authority when the provider cannot verify email ownership.
// Existing identities and their grants are never replaced or re-enabled.
func (manager Manager) EnrollOrganizationOidcAdministrator(ctx context.Context, organization string, input OrganizationOidcAdministratorInput) (OrganizationPermissions, error) {
	principal, authenticated := PrincipalFromContext(ctx)
	if !authenticated {
		return OrganizationPermissions{}, ErrUnauthenticated
	}
	if !principal.Admin {
		return OrganizationPermissions{}, ErrForbidden
	}
	if validateOrganizationOidcSubject(input.Subject) != nil || input.Revision < 1 {
		return OrganizationPermissions{}, ErrInvalid
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	provider, policy, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	if provider.Settings.Revision != input.Revision {
		return OrganizationPermissions{}, organizationOidcError{ErrConflict, "OIDC settings changed; test sign-in again before enrolling the administrator."}
	}
	writes, err := manager.stageOrganizationOidcAdministrator(ctx, provider.OrganizationID, policy.Issuer, input.Subject, policy.AdminEmail)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	if len(writes) != 0 {
		if err := manager.database.Commit(ctx, writes); err != nil {
			return OrganizationPermissions{}, translateStoreError(err)
		}
	}
	admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	if admin.Disabled {
		return OrganizationPermissions{}, organizationOidcError{ErrConflict, "This administrator is disabled. Re-enable it through organization members."}
	}
	return manager.GetOrganizationPermissions(ctx, organization)
}

// Callers hold runtime.mutex and commit these writes with the selected provider.
func (manager Manager) stageOrganizationOidcAdministrator(ctx context.Context, organizationID, issuer, subject, adminEmail string) ([]sqlite.Write, error) {
	admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organizationID)
	if err == nil {
		if admin.OrganizationID != organizationID || admin.Issuer != issuer || admin.Subject != subject {
			return nil, organizationOidcError{ErrConflict, "This organization already has an enrolled administrator. Manage its access through organization members."}
		}
		return nil, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	members, err := manager.organizationMembers(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		// A disabled or less privileged membership cannot become the initial admin.
		if member.Member.Issuer == issuer && member.Member.Subject == subject {
			return nil, organizationOidcError{ErrConflict, "This account already has a membership. Change its grants through organization members."}
		}
	}
	role, roleWrite, err := initialOrganizationRole(organizationID)
	if err != nil {
		return nil, err
	}
	policy, policyRecord, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	policy.AdminEmail = strings.TrimSpace(adminEmail)
	admin = organizationLoginAdmin{Email: policy.AdminEmail, OrganizationID: organizationID, Issuer: issuer, Subject: subject, RoleIDs: []string{role.ID}}
	write, err := documentWrite("organization-login-admin", organizationID, admin, 0)
	if err != nil {
		return nil, err
	}
	policy.AdminSelectionState = "enrolled"
	policyWrite, err := documentWrite("organization-login-policy", organizationID, policy, policyRecord.Revision)
	if err != nil {
		return nil, err
	}
	writes := []sqlite.Write{write}
	if policyRecord.Revision != 0 {
		writes = append(writes, policyWrite)
	}
	_, _, roleErr := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", role.ID)
	if errors.Is(roleErr, ErrNotFound) {
		writes = append(writes, roleWrite)
	} else if roleErr != nil {
		return nil, roleErr
	}
	return writes, nil
}

func validateOrganizationOidcSubject(subject string) error {
	if subject == "" || len(subject) > 255 || strings.TrimSpace(subject) != subject || strings.ContainsAny(subject, "\r\n\t\x00") {
		return organizationOidcError{ErrInvalid, "Enter the exact Account ID from Borq's successful Test sign-in."}
	}
	return nil
}
