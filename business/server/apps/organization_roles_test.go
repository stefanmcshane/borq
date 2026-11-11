package apps

import (
	"errors"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationRoleRevocation owns live shared grants, durable revisions and
// irreversible narrowing of tokens even if the role later restores a permission.
func TestOrganizationRoleRevocation(t *testing.T) {
	assert := is.New(t)
	manager, configuration, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	user := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{"*"}})
	admin := WithPrincipal(t.Context(), Principal{Admin: true})
	roles, err := manager.ListOrganizationRoles(admin, "team")
	assert.NoErr(err)
	assert.Equal(len(roles), 1)
	role := roles[0]
	support, err := manager.CreateOrganizationRole(admin, "team", OrganizationRoleInput{Name: "Cluster linker", Permissions: []Permission{PermissionClusterLink}})
	assert.NoErr(err)
	members, err := manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	_, err = manager.PutOrganizationMember(admin, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: []string{role.ID, support.ID}, Revision: members[0].Revision})
	assert.NoErr(err)
	token, err := manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{PermissionOrganizationRead, PermissionClusterLink}, ExpiresInSeconds: 300})
	assert.NoErr(err)
	principal, err := manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.NoErr(err)
	tokenContext := WithPrincipal(t.Context(), principal)
	role, err = manager.PutOrganizationRole(admin, "team", role.ID, OrganizationRoleInput{Name: role.Name, Revision: role.Revision, Permissions: []Permission{PermissionOrganizationRead}})
	assert.NoErr(err)
	assert.NoErr(manager.Authorize(tokenContext, PermissionOrganizationRead, "team"))
	assert.NoErr(manager.Authorize(user, PermissionClusterLink, "team"))
	assert.NoErr(manager.Authorize(tokenContext, PermissionClusterLink, "team"))
	_, err = manager.PutOrganizationRole(admin, "team", support.ID, OrganizationRoleInput{Name: support.Name, Revision: support.Revision, Permissions: []Permission{}})
	assert.NoErr(err)
	assert.True(errors.Is(manager.Authorize(tokenContext, PermissionClusterLink, "team"), ErrForbidden))
	_, err = manager.PutOrganizationRole(admin, "team", role.ID, OrganizationRoleInput{Name: role.Name, Revision: role.Revision - 1, Permissions: []Permission{}})
	assert.True(errors.Is(err, ErrConflict))
	role, err = manager.PutOrganizationRole(admin, "team", role.ID, OrganizationRoleInput{Name: role.Name, Revision: role.Revision, Permissions: []Permission{PermissionOrganizationRead, PermissionClusterLink}})
	assert.NoErr(err)
	assert.NoErr(manager.Authorize(user, PermissionClusterLink, "team"))
	assert.True(errors.Is(manager.Authorize(tokenContext, PermissionClusterLink, "team"), ErrForbidden))
	assert.True(errors.Is(manager.DeleteOrganizationRole(admin, "team", role.ID, role.Revision), ErrConflict))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	principal, err = manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.NoErr(err)
	assert.True(errors.Is(manager.Authorize(WithPrincipal(t.Context(), principal), PermissionClusterLink, "team"), ErrForbidden))
	roles, err = manager.ListOrganizationRoles(admin, "team")
	assert.NoErr(err)
	assert.Equal(roles[0], role)
	unused, err := manager.CreateOrganizationRole(admin, "team", OrganizationRoleInput{Name: "Unused", Permissions: []Permission{}})
	assert.NoErr(err)
	assert.True(errors.Is(manager.DeleteOrganizationRole(admin, "team", unused.ID, unused.Revision+1), ErrConflict))
	assert.NoErr(manager.DeleteOrganizationRole(admin, "team", unused.ID, unused.Revision))
}

// TestOrganizationRoleAuthority prevents role creation or assignment from granting
// permissions beyond the actor's authority or crossing organization boundaries.
func TestOrganizationRoleAuthority(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	admin := WithPrincipal(t.Context(), Principal{Admin: true})
	roles, err := manager.ListOrganizationRoles(admin, "team")
	assert.NoErr(err)
	_, err = manager.PutOrganizationRole(admin, "team", roles[0].ID, OrganizationRoleInput{Name: "User manager", Revision: roles[0].Revision, Permissions: []Permission{PermissionUserManage, PermissionOrganizationRead}})
	assert.NoErr(err)
	actor := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{"*"}})
	for _, permission := range []Permission{"*", PermissionClusterLink, PermissionManagementRead, PermissionManagementConfigure, PermissionOrganizationCreate} {
		_, err := manager.CreateOrganizationRole(actor, "team", OrganizationRoleInput{Name: "Escalation", Permissions: []Permission{permission}})
		assert.True(errors.Is(err, ErrForbidden))
	}
	for _, input := range []OrganizationRoleInput{{Name: "Missing grants"}, {Name: "", Permissions: []Permission{}}, {Name: "Unknown", Permissions: []Permission{"unknown:grant"}}, {Name: "Revision", Revision: 1, Permissions: []Permission{}}} {
		_, err := manager.CreateOrganizationRole(admin, "team", input)
		assert.True(errors.Is(err, ErrInvalid))
	}
	powerful, err := manager.CreateOrganizationRole(admin, "team", OrganizationRoleInput{Name: "Deployer", Permissions: []Permission{PermissionAppDeploy}})
	assert.NoErr(err)
	members, err := manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	_, err = manager.PutOrganizationMember(actor, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: []string{powerful.ID}, Revision: members[0].Revision})
	assert.True(errors.Is(err, ErrForbidden))
	_, err = manager.PutOrganizationMember(admin, "team", "initial-administrator", OrganizationMemberInput{Revision: members[0].Revision})
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.ListOrganizationRoles(t.Context(), "team")
	assert.True(errors.Is(err, ErrUnauthenticated))
	foreign := organizationRoleRecord{OrganizationID: "other-organization", Role: OrganizationRole{ID: "foreign", Name: "Foreign", Permissions: []Permission{PermissionOrganizationRead}}}
	write, err := documentWrite("organization-role", "foreign", foreign, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.PutOrganizationMember(admin, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: []string{"foreign"}, Revision: members[0].Revision})
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.PutOrganizationRole(admin, "team", "foreign", OrganizationRoleInput{Name: "Hijacked", Permissions: []Permission{}, Revision: 1})
	assert.True(errors.Is(err, ErrNotFound))
	assert.True(errors.Is(manager.DeleteOrganizationRole(admin, "team", "foreign", 1), ErrNotFound))
}

// TestOrganizationRoleMigration exercises atomic persistent conversion of legacy
// individual grants, including default administrator access and explicit revocation.
func TestOrganizationRoleMigration(t *testing.T) {
	assert := is.New(t)
	manager, configuration, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	legacy, record, err := loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", login.OrganizationID)
	assert.NoErr(err)
	legacy.RoleIDs, legacy.Permissions = nil, nil
	adminWrite, err := documentWrite("organization-login-admin", login.OrganizationID, legacy, record.Revision)
	assert.NoErr(err)
	pendingWrite, err := documentWrite("organization-member", "unclaimed-invitation", organizationMemberRecord{OrganizationID: login.OrganizationID, Member: OrganizationMember{ID: "unclaimed-invitation", Email: "pending@example.test", Permissions: []Permission{"*"}, State: "invited"}}, 0)
	assert.NoErr(err)
	writes := []sqlite.Write{adminWrite, pendingWrite}
	for _, member := range []OrganizationMember{{ID: "reader-one", Permissions: []Permission{PermissionAppRead}}, {ID: "reader-two", Permissions: []Permission{PermissionAppRead}}, {ID: "installation-grant", Permissions: []Permission{PermissionAppRead, PermissionManagementRead}}, {ID: "revoked", Permissions: []Permission{}}} {
		member.Subject = member.ID
		write, err := documentWrite("organization-member", member.ID, organizationMemberRecord{OrganizationID: login.OrganizationID, Member: member}, 0)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	assert.NoErr(manager.database.Commit(t.Context(), writes))
	admin := WithPrincipal(t.Context(), Principal{Admin: true})
	members, err := manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	assert.Equal(len(members), 5)
	_, _, err = loadDocument[organizationMemberRecord](t.Context(), manager.database, "organization-member", "unclaimed-invitation")
	assert.True(errors.Is(err, ErrNotFound))
	roles, err := manager.ListOrganizationRoles(admin, "team")
	assert.NoErr(err)
	assert.Equal(len(roles), 3)
	for _, member := range members {
		assert.Equal(len(member.RoleIDs), 1)
		if member.ID == "revoked" {
			assert.Equal(member.Permissions, []Permission{})
		}
		if member.ID == "initial-administrator" {
			assert.Equal(member.Permissions, []Permission{"*"})
		}
		if member.ID == "reader-one" || member.ID == "reader-two" || member.ID == "installation-grant" {
			assert.Equal(member.Permissions, []Permission{PermissionAppRead})
		}
	}
	legacy, record, err = loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", login.OrganizationID)
	assert.NoErr(err)
	legacy.RoleIDs, legacy.Permissions = nil, []Permission{}
	adminWrite, err = documentWrite("organization-login-admin", login.OrganizationID, legacy, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{adminWrite}))
	members, err = manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	for _, member := range members {
		if member.ID == "initial-administrator" {
			assert.Equal(member.Permissions, []Permission{})
		}
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	reopened, err := manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	assert.Equal(reopened, members)
	stored, _, err := loadDocument[organizationMemberRecord](t.Context(), manager.database, "organization-member", "reader-one")
	assert.NoErr(err)
	assert.True(stored.Member.Permissions == nil)
	assert.Equal(len(stored.Member.RoleIDs), 1)
}
