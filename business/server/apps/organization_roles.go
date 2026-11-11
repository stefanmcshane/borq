package apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// OrganizationRole is a reusable set of permissions within one organization.
type OrganizationRole struct {
	// ID is the immutable role identifier.
	ID string `json:"id"`
	// Name is the administrator-selected display name.
	Name string `json:"name"`
	// Permissions applies to every account assigned this role.
	Permissions []Permission `json:"permissions"`
	// Revision prevents overwriting concurrent role edits.
	Revision int64 `json:"revision"`
}

// OrganizationRoleInput creates or replaces a reusable role.
type OrganizationRoleInput struct {
	// Name is the role's display name.
	Name string `json:"name"`
	// Permissions is required; an empty array grants no access.
	Permissions []Permission `json:"permissions"`
	// Revision is zero for creation and the last read revision for updates.
	Revision int64 `json:"revision"`
}

type organizationRoleRecord struct {
	OrganizationID string           `json:"organizationId"`
	Role           OrganizationRole `json:"role"`
}

func migrationOrganizationRole(organizationID string, permissions []Permission) (OrganizationRole, sqlite.Write, error) {
	permissions = slices.DeleteFunc(slices.Clone(permissions), func(permission Permission) bool {
		// Installation grants never conferred organization access, including in legacy records.
		return permission == PermissionManagementRead || permission == PermissionManagementConfigure || permission == PermissionOrganizationCreate
	})
	if permissions == nil {
		permissions = []Permission{}
	}
	slices.Sort(permissions)
	permissions = slices.Compact(permissions)
	encoded, err := json.Marshal(permissions)
	if err != nil {
		return OrganizationRole{}, sqlite.Write{}, err
	}
	digest := sha256.Sum256(encoded)
	suffix := hex.EncodeToString(digest[:])
	role := OrganizationRole{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(organizationID+"\x00"+suffix)).String(), Name: "Migrated access " + suffix[:12], Permissions: permissions}
	if slices.Equal(permissions, []Permission{"*"}) {
		role.Name = "Administrator"
	}
	write, err := documentWrite("organization-role", role.ID, organizationRoleRecord{OrganizationID: organizationID, Role: role}, 0)
	return role, write, err
}

func initialOrganizationRole(organizationID string) (OrganizationRole, sqlite.Write, error) {
	return migrationOrganizationRole(organizationID, []Permission{"*"})
}

// ensureOrganizationRoles atomically preserves legacy access while replacing individual grants.
// Nil role IDs identify old records; explicit empty assignments must never receive defaults.
func (manager Manager) ensureOrganizationRoles(ctx context.Context, organizationID string) error {
	for range 5 {
		writes := []sqlite.Write{}
		staged := map[string]bool{}
		addRole := func(permissions []Permission) ([]string, error) {
			role, write, err := migrationOrganizationRole(organizationID, permissions)
			if err != nil {
				return nil, err
			}
			_, _, readErr := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", role.ID)
			if errors.Is(readErr, ErrNotFound) && !staged[role.ID] {
				writes = append(writes, write)
				staged[role.ID] = true
			} else if readErr != nil && !errors.Is(readErr, ErrNotFound) {
				return nil, readErr
			}
			return []string{role.ID}, nil
		}
		members, err := manager.organizationMembers(ctx, organizationID)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.Member.Subject == "" {
				writes = append(writes, sqlite.Write{Collection: "organization-member", Key: member.Member.ID, ExpectedRevision: member.Member.Revision, Delete: true})
				continue
			}
			if member.Member.RoleIDs != nil {
				continue
			}
			member.Member.RoleIDs, err = addRole(member.Member.Permissions)
			if err != nil {
				return err
			}
			member.Member.Permissions = nil
			write, err := documentWrite("organization-member", member.Member.ID, member, member.Member.Revision)
			if err != nil {
				return err
			}
			writes = append(writes, write)
		}
		admin, record, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organizationID)
		if err == nil && admin.RoleIDs == nil {
			permissions := admin.Permissions
			if permissions == nil {
				permissions = []Permission{"*"}
			}
			admin.RoleIDs, err = addRole(permissions)
			if err != nil {
				return err
			}
			admin.Permissions = nil
			write, err := documentWrite("organization-login-admin", organizationID, admin, record.Revision)
			if err != nil {
				return err
			}
			writes = append(writes, write)
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if len(writes) == 0 {
			return nil
		}
		if err := manager.database.Commit(ctx, writes); errors.Is(err, sqlite.ErrConflict) {
			continue
		} else {
			return err
		}
	}
	return ErrConflict
}

func (manager Manager) organizationRolePermissions(ctx context.Context, organizationID string, ids []string) ([]Permission, error) {
	permissions := []Permission{}
	for _, id := range ids {
		stored, _, err := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", id)
		if err != nil {
			return nil, err
		}
		if stored.OrganizationID != organizationID {
			return nil, ErrNotFound
		}
		permissions = append(permissions, stored.Role.Permissions...)
	}
	slices.Sort(permissions)
	return slices.Compact(permissions), nil
}

func (manager Manager) validateMemberRoles(ctx context.Context, organization string, ids []string) ([]Permission, error) {
	if ids == nil || len(ids) > 64 {
		return nil, ErrInvalid
	}
	provider, _, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return nil, err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return nil, err
	}
	permissions, err := manager.organizationRolePermissions(ctx, provider.OrganizationID, ids)
	if err != nil {
		return nil, err
	}
	if err := manager.validateMemberGrants(ctx, organization, permissions); err != nil {
		return nil, err
	}
	return permissions, nil
}

// ListOrganizationRoles lists the reusable permission sets owned by the organization.
func (manager Manager) ListOrganizationRoles(ctx context.Context, organization string) ([]OrganizationRole, error) {
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return nil, err
	}
	provider, _, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return nil, err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "organization-role")
	if err != nil {
		return nil, err
	}
	roles := []OrganizationRole{}
	for _, record := range records {
		var stored organizationRoleRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.OrganizationID != provider.OrganizationID {
			continue
		}
		stored.Role.Revision = record.Revision
		roles = append(roles, stored.Role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return roles, nil
}

// CreateOrganizationRole creates an organization-owned reusable permission set.
func (manager Manager) CreateOrganizationRole(ctx context.Context, organization string, input OrganizationRoleInput) (OrganizationRole, error) {
	return manager.writeOrganizationRole(ctx, organization, uuid.NewString(), input, true)
}

// PutOrganizationRole replaces a role and immediately changes every assigned account's access.
func (manager Manager) PutOrganizationRole(ctx context.Context, organization, id string, input OrganizationRoleInput) (OrganizationRole, error) {
	return manager.writeOrganizationRole(ctx, organization, id, input, false)
}

func (manager Manager) writeOrganizationRole(ctx context.Context, organization, id string, input OrganizationRoleInput, create bool) (OrganizationRole, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return OrganizationRole{}, err
	}
	if err := manager.validateMemberGrants(ctx, organization, input.Permissions); err != nil {
		return OrganizationRole{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 128 || (create && input.Revision != 0) || (!create && input.Revision < 1) {
		return OrganizationRole{}, ErrInvalid
	}
	provider, _, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return OrganizationRole{}, err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return OrganizationRole{}, err
	}
	if !create {
		stored, record, err := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", id)
		if err != nil {
			return OrganizationRole{}, err
		}
		if stored.OrganizationID != provider.OrganizationID {
			return OrganizationRole{}, ErrNotFound
		}
		if input.Revision != record.Revision {
			return OrganizationRole{}, ErrConflict
		}
	}
	role := OrganizationRole{ID: id, Name: name, Permissions: slices.Clone(input.Permissions), Revision: input.Revision + 1}
	write, err := documentWrite("organization-role", id, organizationRoleRecord{OrganizationID: provider.OrganizationID, Role: role}, input.Revision)
	if err != nil {
		return OrganizationRole{}, err
	}
	writes := []sqlite.Write{write}
	if !create {
		tokenWrites, err := manager.organizationRoleTokenWrites(ctx, provider.OrganizationID, role)
		if err != nil {
			return OrganizationRole{}, err
		}
		writes = append(writes, tokenWrites...)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return OrganizationRole{}, translateStoreError(err)
	}
	return role, nil
}

// DeleteOrganizationRole removes an unassigned role, rejecting stale revisions and active assignments.
func (manager Manager) DeleteOrganizationRole(ctx context.Context, organization, id string, revision int64) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return err
	}
	provider, _, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return err
	}
	stored, record, err := loadDocument[organizationRoleRecord](ctx, manager.database, "organization-role", id)
	if err != nil {
		return err
	}
	if stored.OrganizationID != provider.OrganizationID {
		return ErrNotFound
	}
	if revision < 1 || revision != record.Revision {
		return ErrConflict
	}
	members, err := manager.organizationMembers(ctx, provider.OrganizationID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if slices.Contains(member.Member.RoleIDs, id) {
			return ErrConflict
		}
	}
	admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err == nil && slices.Contains(admin.RoleIDs, id) {
		return ErrConflict
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return translateStoreError(manager.database.Commit(ctx, []sqlite.Write{{Collection: "organization-role", Key: id, ExpectedRevision: revision, Delete: true}}))
}

func (manager Manager) organizationRoleTokenWrites(ctx context.Context, organizationID string, changed OrganizationRole) ([]sqlite.Write, error) {
	records, err := manager.database.List(ctx, "api-token")
	if err != nil {
		return nil, err
	}
	writes := []sqlite.Write{}
	for _, record := range records {
		var token apiTokenRecord
		if err := json.Unmarshal(record.Value, &token); err != nil {
			return nil, err
		}
		if token.Login.OrganizationID != organizationID {
			continue
		}
		var ids []string
		if token.Login.MemberID == "" {
			admin, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organizationID)
			if err != nil {
				return nil, err
			}
			ids = admin.RoleIDs
		} else {
			member, _, err := loadDocument[organizationMemberRecord](ctx, manager.database, "organization-member", token.Login.MemberID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			ids = member.Member.RoleIDs
		}
		if !slices.Contains(ids, changed.ID) {
			continue
		}
		remaining := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == changed.ID })
		permissions, err := manager.organizationRolePermissions(ctx, organizationID, remaining)
		if err != nil {
			return nil, err
		}
		permissions = append(permissions, changed.Permissions...)
		reduced := slices.DeleteFunc(slices.Clone(token.Permissions), func(permission Permission) bool {
			return !slices.Contains(permissions, permission) && !slices.Contains(permissions, Permission("*"))
		})
		if slices.Equal(reduced, token.Permissions) {
			continue
		}
		token.Permissions = reduced
		write, err := documentWrite("api-token", record.Key, token, record.Revision)
		if err != nil {
			return nil, err
		}
		writes = append(writes, write)
	}
	return writes, nil
}

func (manager Manager) memberTokenRevocations(ctx context.Context, organizationID, memberID string) ([]sqlite.Write, error) {
	records, err := manager.database.List(ctx, "api-token")
	if err != nil {
		return nil, err
	}
	writes := []sqlite.Write{}
	for _, record := range records {
		var token apiTokenRecord
		if err := json.Unmarshal(record.Value, &token); err != nil {
			return nil, err
		}
		if token.Login.OrganizationID == organizationID && token.Login.MemberID == memberID {
			writes = append(writes, sqlite.Write{Collection: "api-token", Key: record.Key, ExpectedRevision: record.Revision, Delete: true})
		}
	}
	return writes, nil
}
