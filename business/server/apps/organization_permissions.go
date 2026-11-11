package apps

import (
	"context"
)

// OrganizationPermissions exposes the initial administrator's effective role permissions.
type OrganizationPermissions struct {
	// Subject identifies the pinned initial administrator account.
	Subject string `json:"subject"`
	// Permissions is the read-only effective grant set derived from assigned roles.
	Permissions []Permission `json:"permissions"`
	// Revision identifies the underlying administrator membership revision.
	Revision int64 `json:"revision"`
}

// GetOrganizationPermissions loads current grants without changing an existing login or token.
// Transport authorization must restrict this settings read to administrators.
func (manager Manager) GetOrganizationPermissions(ctx context.Context, organization string) (OrganizationPermissions, error) {
	provider, _, err := manager.organizationOidc(ctx, organization)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return OrganizationPermissions{}, err
	}
	admin, record, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	if admin.OrganizationID != provider.OrganizationID || admin.Issuer == "" || admin.Subject == "" {
		return OrganizationPermissions{}, organizationLoginDenied()
	}
	permissions, err := manager.organizationRolePermissions(ctx, provider.OrganizationID, admin.RoleIDs)
	if err != nil {
		return OrganizationPermissions{}, err
	}
	return OrganizationPermissions{Subject: admin.Subject, Permissions: permissions, Revision: record.Revision}, nil
}
