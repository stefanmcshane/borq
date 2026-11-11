package apps

import (
	"errors"
	"testing"

	"github.com/matryer/is"
)

// TestOrganizationMemberCannotClaimBootstrapAdministrator keeps an established
// subject's membership authoritative when the provider changes its email.
func TestOrganizationMemberCannotClaimBootstrapAdministrator(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	administrator := WithPrincipal(t.Context(), Principal{Admin: true})
	adminEmail := identity.Email
	identity.Subject, identity.Email = "existing-member", "member@example.test"
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.True(login.MemberID != "")
	identity.Email = adminEmail
	changed, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.Equal(changed.MemberID, login.MemberID)
	_, _, err = loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", login.OrganizationID)
	assert.True(errors.Is(err, ErrNotFound))
	members, err := manager.ListOrganizationMembers(administrator, "team")
	assert.NoErr(err)
	assert.Equal(len(members), 1)
	_, err = manager.PutOrganizationMember(administrator, "team", members[0].ID, OrganizationMemberInput{RoleIDs: members[0].RoleIDs, Disabled: true, Revision: members[0].Revision})
	assert.NoErr(err)
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.True(err != nil)
}

// TestOrganizationMemberLifecycle owns first-login admission, live assignments,
// durable subject pins, account disablement and irreversible token revocation.
func TestOrganizationMemberLifecycle(t *testing.T) {
	assert := is.New(t)
	manager, configuration, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	administrator := WithPrincipal(t.Context(), Principal{Admin: true})
	identity.Email, identity.Subject = "user@example.test", "normal-user"
	identity.EmailVerified = false
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	memberContext := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{"*"}})
	assert.True(errors.Is(manager.Authorize(memberContext, PermissionAppRead, "team"), ErrForbidden))
	role, err := manager.CreateOrganizationRole(administrator, "team", OrganizationRoleInput{Name: "Reader", Permissions: []Permission{PermissionOrganizationRead, PermissionAppRead, PermissionTokenCreate}})
	assert.NoErr(err)
	members, err := manager.ListOrganizationMembers(administrator, "team")
	assert.NoErr(err)
	assert.Equal(members[0].RoleIDs, []string{})
	assert.True(!members[0].EmailVerified)
	current, err := manager.PutOrganizationMember(administrator, "team", login.MemberID, OrganizationMemberInput{RoleIDs: []string{role.ID}, Revision: members[0].Revision})
	assert.NoErr(err)
	assert.NoErr(manager.Authorize(memberContext, PermissionAppRead, "team"))
	assert.True(errors.Is(manager.Authorize(memberContext, PermissionAppDeploy, "team"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(memberContext, PermissionAppRead, "other"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(memberContext, PermissionManagementRead, "team"), ErrForbidden))
	token, err := manager.CreateApiToken(memberContext, CreateApiTokenInput{Permissions: []Permission{PermissionAppRead}, ExpiresInSeconds: 300})
	assert.NoErr(err)
	identity.Subject = "email-replacement"
	replacement, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.True(replacement.MemberID != login.MemberID)
	replacementPermissions, err := manager.OrganizationLoginPermissions(t.Context(), replacement)
	assert.NoErr(err)
	assert.Equal(replacementPermissions, []Permission{})
	identity.Subject, identity.Email = "normal-user", "changed@example.test"
	pinned, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.Equal(pinned.MemberID, login.MemberID)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	authenticated, err := manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.NoErr(err)
	assert.NoErr(manager.Authorize(WithPrincipal(t.Context(), authenticated), PermissionAppRead, "team"))
	disabled, err := manager.PutOrganizationMember(administrator, "team", current.ID, OrganizationMemberInput{RoleIDs: current.RoleIDs, Disabled: true, Revision: current.Revision})
	assert.NoErr(err)
	assert.Equal(disabled.State, "disabled")
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
	_, err = manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.True(errors.Is(err, ErrUnauthenticated))
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.True(err != nil)
	enabled, err := manager.PutOrganizationMember(administrator, "team", current.ID, OrganizationMemberInput{RoleIDs: current.RoleIDs, Revision: disabled.Revision})
	assert.NoErr(err)
	assert.Equal(enabled.State, "active")
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
	fresh, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.Authorize(WithPrincipal(t.Context(), Principal{Login: fresh, Permissions: []Permission{"*"}}), PermissionAppRead, "team"))
	_, err = manager.PutOrganizationMember(administrator, "team", current.ID, OrganizationMemberInput{RoleIDs: []string{}, Revision: disabled.Revision})
	assert.True(errors.Is(err, ErrConflict))
	token, err = manager.CreateApiToken(WithPrincipal(t.Context(), Principal{Login: fresh, Permissions: []Permission{"*"}}), CreateApiTokenInput{Permissions: []Permission{PermissionAppRead}, ExpiresInSeconds: 300})
	assert.NoErr(err)
	removed, err := manager.PutOrganizationMember(administrator, "team", current.ID, OrganizationMemberInput{RoleIDs: []string{}, Revision: enabled.Revision})
	assert.NoErr(err)
	_, err = manager.PutOrganizationMember(administrator, "team", current.ID, OrganizationMemberInput{RoleIDs: current.RoleIDs, Revision: removed.Revision})
	assert.NoErr(err)
	_, err = manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.True(errors.Is(err, ErrUnauthenticated))
}

// TestInitialOrganizationMemberDisable prevents enabled accounts reviving old sessions.
func TestInitialOrganizationMemberDisable(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	admin := WithPrincipal(t.Context(), Principal{Admin: true})
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	members, err := manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	disabled, err := manager.PutOrganizationMember(admin, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: members[0].RoleIDs, Disabled: true, Revision: members[0].Revision})
	assert.NoErr(err)
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.True(err != nil)
	_, err = manager.PutOrganizationMember(admin, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: members[0].RoleIDs, Revision: disabled.Revision})
	assert.NoErr(err)
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
	fresh, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), fresh))
}

// TestOrganizationMemberWithoutEmail admits a verified provider subject without
// making optional contact claims a second authentication or membership gate.
func TestOrganizationMemberWithoutEmail(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	identity.Email, identity.Subject, identity.EmailVerified = "", "subject-without-email", false
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.True(login.MemberID != "")
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	permissions, err := manager.OrganizationLoginPermissions(t.Context(), login)
	assert.NoErr(err)
	assert.Equal(permissions, []Permission{})
}

// TestOrganizationAdministratorRoleAssignment permits the organization admin to
// delegate the reusable Administrator role while installation access stays denied.
func TestOrganizationAdministratorRoleAssignment(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	initial, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	actor := WithPrincipal(t.Context(), Principal{Login: initial, Permissions: []Permission{"*"}})
	roles, err := manager.ListOrganizationRoles(actor, "team")
	assert.NoErr(err)
	identity.Email, identity.Subject = "another-admin@example.test", "another-admin"
	memberLogin, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	members, err := manager.ListOrganizationMembers(actor, "team")
	assert.NoErr(err)
	var member OrganizationMember
	for _, candidate := range members {
		if candidate.ID == memberLogin.MemberID {
			member = candidate
		}
	}
	_, err = manager.PutOrganizationMember(actor, "team", member.ID, OrganizationMemberInput{RoleIDs: []string{roles[0].ID}, Revision: member.Revision})
	assert.NoErr(err)
	delegated := WithPrincipal(t.Context(), Principal{Login: memberLogin, Permissions: []Permission{"*"}})
	assert.NoErr(manager.Authorize(delegated, PermissionUserManage, "team"))
	assert.True(errors.Is(manager.Authorize(delegated, PermissionManagementConfigure, "team"), ErrForbidden))
}
