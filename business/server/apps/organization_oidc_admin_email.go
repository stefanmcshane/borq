package apps

import (
	"context"
	"errors"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Callers hold runtime.mutex and commit selection changes with their configuration.
func (manager Manager) stageOrganizationAdminEmail(ctx context.Context, organizationID string, input OrganizationOidcInput) ([]sqlite.Write, error) {
	if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
		return nil, err
	}
	policy, record, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if record.Revision == 0 {
		if _, _, err := manager.readPocketId(ctx, organizationID); err == nil {
			return nil, organizationOidcError{ErrConflict, "This organization already has a managed sign-in provider. Its initial administrator cannot be selected again."}
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if _, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organizationID); err == nil {
			return nil, organizationOidcError{ErrConflict, "This organization already has an enrolled administrator. Its initial administrator cannot be selected again."}
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		policy = organizationLoginPolicy{OrganizationID: organizationID, AdminEmail: strings.TrimSpace(input.AdminEmail), AdminSelectionState: "pending", Issuer: strings.TrimSpace(input.IssuerURL), ClientID: strings.TrimSpace(input.ClientID), BaseURL: strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")}
	} else {
		if policy.OrganizationID != organizationID || policy.Issuer != strings.TrimSpace(input.IssuerURL) {
			return nil, organizationOidcError{ErrConflict, "The administrator's saved issuer cannot be replaced by reapplying settings."}
		}
		if strings.EqualFold(policy.AdminEmail, strings.TrimSpace(input.AdminEmail)) {
			return nil, nil
		}
		if policy.PocketInstanceID != "" || policy.PocketOrganizationID != "" {
			return nil, organizationOidcError{ErrConflict, "Manage this provider's administrator through organization members."}
		}
	}
	writes := []sqlite.Write{}
	admin, adminRecord, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organizationID)
	if err == nil && admin.Email == "" {
		// Legacy pins displayed the policy address, which must survive a new selection.
		admin.Email = policy.AdminEmail
		write, err := documentWrite("organization-login-admin", organizationID, admin, adminRecord.Revision)
		if err != nil {
			return nil, err
		}
		writes = append(writes, write)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	policy.AdminEmail, policy.AdminSelectionState = strings.TrimSpace(input.AdminEmail), "pending"
	write, err := documentWrite("organization-login-policy", organizationID, policy, record.Revision)
	return append(writes, write), err
}

// Consuming the selection and granting access together prevents a later login from
// restoring revoked roles or transferring access to an account that reuses an email.
func (manager Manager) enrollSelectedOrganizationAdministrator(ctx context.Context, provider organizationOidcRecord, policy organizationLoginPolicy, identity oidc.Identity, login OrganizationLogin, initial bool) (OrganizationLogin, error) {
	role, roleWrite, err := initialOrganizationRole(provider.OrganizationID)
	if err != nil {
		return OrganizationLogin{}, err
	}
	writes := []sqlite.Write{}
	savedRole, _, roleErr := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", role.ID)
	if roleErr == nil && !slices.Equal(savedRole.Role.Permissions, []Permission{"*"}) {
		// A customized role must retain its permissions and existing assignments.
		role.ID, err = newId()
		if err != nil {
			return OrganizationLogin{}, err
		}
		roleWrite, err = documentWrite("organization-role", role.ID, organizationRoleRecord{OrganizationID: provider.OrganizationID, Role: role}, 0)
		if err != nil {
			return OrganizationLogin{}, err
		}
		writes = append(writes, roleWrite)
	} else if errors.Is(roleErr, ErrNotFound) {
		writes = append(writes, roleWrite)
	} else if roleErr != nil {
		return OrganizationLogin{}, roleErr
	}
	if initial {
		admin := organizationLoginAdmin{OrganizationID: provider.OrganizationID, Issuer: identity.Issuer, Subject: identity.Subject, Email: policy.AdminEmail, EmailVerified: identity.EmailVerified, RoleIDs: []string{role.ID}}
		write, err := documentWrite("organization-login-admin", provider.OrganizationID, admin, 0)
		if err != nil {
			return OrganizationLogin{}, err
		}
		writes = append(writes, write)
	} else if login.MemberID == "" {
		admin, record, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
		if err != nil {
			return OrganizationLogin{}, err
		}
		if !slices.Contains(admin.RoleIDs, role.ID) {
			admin.RoleIDs = append(admin.RoleIDs, role.ID)
		}
		write, err := documentWrite("organization-login-admin", provider.OrganizationID, admin, record.Revision)
		if err != nil {
			return OrganizationLogin{}, err
		}
		writes = append(writes, write)
	} else {
		member, record, err := loadDocument[organizationMemberRecord](ctx, manager.database, "organization-member", login.MemberID)
		if err != nil {
			return OrganizationLogin{}, err
		}
		if !slices.Contains(member.Member.RoleIDs, role.ID) {
			member.Member.RoleIDs = append(member.Member.RoleIDs, role.ID)
		}
		member.Member.Email, member.Member.EmailVerified = strings.ToLower(strings.TrimSpace(identity.Email)), identity.EmailVerified
		write, err := documentWrite("organization-member", login.MemberID, member, record.Revision)
		if err != nil {
			return OrganizationLogin{}, err
		}
		writes = append(writes, write)
	}
	current, record, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", provider.OrganizationID)
	if errors.Is(err, ErrNotFound) {
		// Legacy managed providers derive their selection from the managed instance.
		if err := manager.database.Commit(ctx, writes); err != nil {
			return OrganizationLogin{}, organizationLoginDenied()
		}
		return login, nil
	} else if err != nil {
		return OrganizationLogin{}, err
	}
	current.AdminSelectionState = "enrolled"
	write, err := documentWrite("organization-login-policy", provider.OrganizationID, current, record.Revision)
	if err != nil {
		return OrganizationLogin{}, err
	}
	if err := manager.database.Commit(ctx, append(writes, write)); err != nil {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	return login, nil
}
