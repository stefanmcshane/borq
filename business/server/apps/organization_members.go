package apps

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// OrganizationMember is a provider-authenticated organization account and its assigned roles.
type OrganizationMember struct {
	// ID is the immutable membership identifier.
	ID string `json:"id"`
	// Email is the optional address reported by the configured provider.
	Email string `json:"email"`
	// EmailVerified records whether the provider verified the displayed email.
	EmailVerified bool `json:"emailVerified"`
	// Issuer identifies the provider whose identity was accepted.
	Issuer string `json:"issuer"`
	// Subject is pinned on the first accepted sign-in and never rebound by email.
	Subject string `json:"subject"`
	// RoleIDs identifies reusable roles belonging to this organization.
	RoleIDs []string `json:"roleIds"`
	// Permissions is the read-only effective permission set derived from roles.
	Permissions []Permission `json:"permissions"`
	// State is active or disabled.
	State string `json:"state"`
	// Revision prevents editors from overwriting concurrent changes.
	Revision int64 `json:"revision"`
}

// OrganizationMemberInput replaces roles and the account's enabled state.
type OrganizationMemberInput struct {
	// RoleIDs replaces the complete set of organization roles; an empty array removes access.
	RoleIDs []string `json:"roleIds"`
	// Disabled prevents current and future sign-ins until re-enabled.
	Disabled bool `json:"disabled"`
	// Revision is the last read membership revision.
	Revision int64 `json:"revision"`
}

type organizationMemberRecord struct {
	Member            OrganizationMember `json:"member"`
	OrganizationID    string             `json:"organizationId"`
	CredentialVersion string             `json:"credentialVersion"`
}

func (manager Manager) organizationMembers(ctx context.Context, organizationID string) ([]organizationMemberRecord, error) {
	records, err := manager.database.List(ctx, "organization-member")
	if err != nil {
		return nil, err
	}
	members := []organizationMemberRecord{}
	for _, record := range records {
		var member organizationMemberRecord
		if err := json.Unmarshal(record.Value, &member); err != nil {
			return nil, err
		}
		if member.OrganizationID != organizationID {
			continue
		}
		member.Member.Revision = record.Revision
		members = append(members, member)
	}
	return members, nil
}

// ListOrganizationMembers lists provider-authenticated accounts and the initial administrator.
func (manager Manager) ListOrganizationMembers(ctx context.Context, organization string) ([]OrganizationMember, error) {
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return nil, err
	}
	provider, policy, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return nil, err
	}
	if err := manager.ensureOrganizationRoles(ctx, provider.OrganizationID); err != nil {
		return nil, err
	}
	records, err := manager.organizationMembers(ctx, provider.OrganizationID)
	if err != nil {
		return nil, err
	}
	members := []OrganizationMember{}
	for _, record := range records {
		record.Member.Permissions, err = manager.organizationRolePermissions(ctx, provider.OrganizationID, record.Member.RoleIDs)
		if err != nil {
			return nil, err
		}
		members = append(members, record.Member)
	}
	admin, record, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err == nil {
		permissions, err := manager.organizationRolePermissions(ctx, provider.OrganizationID, admin.RoleIDs)
		if err != nil {
			return nil, err
		}
		state := "active"
		if admin.Disabled {
			state = "disabled"
		}
		members = append(members, OrganizationMember{ID: "initial-administrator", Email: initialAdministratorEmail(admin, policy), Issuer: admin.Issuer, Subject: admin.Subject, EmailVerified: admin.EmailVerified, RoleIDs: slices.Clone(admin.RoleIDs), Permissions: permissions, State: state, Revision: record.Revision})
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Email < members[j].Email })
	return members, nil
}

func (manager Manager) validateMemberGrants(ctx context.Context, organization string, permissions []Permission) error {
	if permissions == nil || len(permissions) > 64 {
		return ErrInvalid
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	for _, permission := range permissions {
		if permission != "*" && !ValidPermission(permission) {
			return ErrInvalid
		}
		switch permission {
		case PermissionManagementRead, PermissionManagementConfigure, PermissionOrganizationCreate:
			return ErrForbidden
		}
		if permission == "*" {
			if principal.Admin {
				continue
			}
			grants, err := manager.OrganizationLoginPermissions(ctx, principal.Login)
			if err != nil || principal.Login.Organization != organization || !slices.Contains(grants, Permission("*")) || !slices.Contains(principal.Permissions, Permission("*")) {
				return ErrForbidden
			}
			continue
		}
		if err := manager.Authorize(ctx, permission, organization); err != nil {
			return err
		}
	}
	return nil
}

// PutOrganizationMember changes durable grants without adopting or promoting provider accounts.
func (manager Manager) PutOrganizationMember(ctx context.Context, organization, id string, input OrganizationMemberInput) (OrganizationMember, error) {
	if id == "initial-administrator" {
		return manager.putInitialOrganizationMember(ctx, organization, input)
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return OrganizationMember{}, err
	}
	permissions, err := manager.validateMemberRoles(ctx, organization, input.RoleIDs)
	if err != nil {
		return OrganizationMember{}, err
	}
	provider, _, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return OrganizationMember{}, err
	}
	member, record, err := loadDocument[organizationMemberRecord](ctx, manager.database, "organization-member", id)
	if err != nil {
		return OrganizationMember{}, err
	}
	if member.OrganizationID != provider.OrganizationID {
		return OrganizationMember{}, ErrNotFound
	}
	if input.Revision < 1 || input.Revision != record.Revision {
		return OrganizationMember{}, ErrConflict
	}
	rolesChanged := !slices.Equal(member.Member.RoleIDs, input.RoleIDs)
	wasDisabled := member.Member.State == "disabled"
	member.Member.RoleIDs, member.Member.Permissions = slices.Clone(input.RoleIDs), nil
	if input.Disabled {
		member.Member.State = "disabled"
	} else {
		member.Member.State = "active"
	}
	// Disabled credentials remain invalid after the account is enabled again.
	if wasDisabled != input.Disabled {
		member.CredentialVersion = rand.Text()
	}
	write, err := documentWrite("organization-member", id, member, record.Revision)
	if err != nil {
		return OrganizationMember{}, err
	}
	writes := []sqlite.Write{write}
	if rolesChanged {
		tokenWrites, err := manager.memberTokenRevocations(ctx, provider.OrganizationID, id)
		if err != nil {
			return OrganizationMember{}, err
		}
		writes = append(writes, tokenWrites...)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return OrganizationMember{}, translateStoreError(err)
	}
	member.Member.Revision = record.Revision + 1
	member.Member.Permissions = permissions
	return member.Member, nil
}

func (manager Manager) authorizeOrganizationMemberLogin(ctx context.Context, provider organizationOidcRecord, identity oidc.Identity, login OrganizationLogin, members []organizationMemberRecord) (OrganizationLogin, error) {
	for _, member := range members {
		if member.Member.Issuer != identity.Issuer || member.Member.Subject != identity.Subject {
			continue
		}
		if member.Member.State != "active" {
			return OrganizationLogin{}, organizationLoginDenied()
		}
		login.MemberID, login.MembershipVersion = member.Member.ID, member.CredentialVersion
		return login, nil
	}
	// Subjects, rather than reusable email addresses, are the identity boundary.
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(provider.OrganizationID+"\x00"+identity.Issuer+"\x00"+identity.Subject)).String()
	member := organizationMemberRecord{OrganizationID: provider.OrganizationID, CredentialVersion: rand.Text(), Member: OrganizationMember{ID: id, Email: strings.ToLower(strings.TrimSpace(identity.Email)), Issuer: identity.Issuer, Subject: identity.Subject, EmailVerified: identity.EmailVerified, RoleIDs: []string{}, State: "active"}}
	write, err := documentWrite("organization-member", id, member, 0)
	if err != nil || manager.database.Commit(ctx, []sqlite.Write{write}) != nil {
		return OrganizationLogin{}, organizationLoginDenied()
	}
	login.MemberID, login.MembershipVersion = id, member.CredentialVersion
	return login, nil
}

// OrganizationLoginPermissions reads the account's current grants after validating its live membership.
// Transport token scopes remain an independent upper bound enforced by Authorize.
func (manager Manager) OrganizationLoginPermissions(ctx context.Context, login OrganizationLogin) ([]Permission, error) {
	if err := manager.ValidateOrganizationLogin(ctx, login); err != nil {
		return nil, err
	}
	if login.MemberID == "" {
		grants, err := manager.GetOrganizationPermissions(ctx, login.Organization)
		if err != nil {
			return nil, err
		}
		return grants.Permissions, nil
	}
	if err := manager.ensureOrganizationRoles(ctx, login.OrganizationID); err != nil {
		return nil, err
	}
	member, _, err := loadDocument[organizationMemberRecord](ctx, manager.database, "organization-member", login.MemberID)
	if err != nil || member.CredentialVersion != login.MembershipVersion || member.Member.State != "active" {
		return nil, organizationLoginDenied()
	}
	return manager.organizationRolePermissions(ctx, login.OrganizationID, member.Member.RoleIDs)
}

func (manager Manager) putInitialOrganizationMember(ctx context.Context, organization string, input OrganizationMemberInput) (OrganizationMember, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if err := manager.Authorize(ctx, PermissionUserManage, organization); err != nil {
		return OrganizationMember{}, err
	}
	permissions, err := manager.validateMemberRoles(ctx, organization, input.RoleIDs)
	if err != nil {
		return OrganizationMember{}, err
	}
	provider, policy, err := manager.organizationLoginProvider(ctx, organization)
	if err != nil {
		return OrganizationMember{}, err
	}
	admin, record, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", provider.OrganizationID)
	if err != nil {
		return OrganizationMember{}, err
	}
	if input.Revision < 1 || input.Revision != record.Revision {
		return OrganizationMember{}, ErrConflict
	}
	rolesChanged := !slices.Equal(admin.RoleIDs, input.RoleIDs)
	admin.RoleIDs, admin.Permissions = slices.Clone(input.RoleIDs), nil
	if admin.Disabled != input.Disabled {
		admin.CredentialVersion = rand.Text()
	}
	admin.Disabled = input.Disabled
	write, err := documentWrite("organization-login-admin", provider.OrganizationID, admin, record.Revision)
	if err != nil {
		return OrganizationMember{}, err
	}
	writes := []sqlite.Write{write}
	if rolesChanged {
		tokenWrites, err := manager.memberTokenRevocations(ctx, provider.OrganizationID, "")
		if err != nil {
			return OrganizationMember{}, err
		}
		writes = append(writes, tokenWrites...)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return OrganizationMember{}, translateStoreError(err)
	}
	state := "active"
	if admin.Disabled {
		state = "disabled"
	}
	return OrganizationMember{ID: "initial-administrator", Email: initialAdministratorEmail(admin, policy), Issuer: admin.Issuer, Subject: admin.Subject, EmailVerified: admin.EmailVerified, RoleIDs: slices.Clone(admin.RoleIDs), Permissions: permissions, State: state, Revision: record.Revision + 1}, nil
}

func initialAdministratorEmail(admin organizationLoginAdmin, policy organizationLoginPolicy) string {
	if admin.Email != "" {
		return admin.Email
	}
	return policy.AdminEmail
}
