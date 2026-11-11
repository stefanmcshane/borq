package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func TestUserPermissionsAndDurableApiToken(t *testing.T) {
	assert := is.New(t)
	manager, input, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.True(errors.Is(manager.Authorize(t.Context(), PermissionClusterLink, "team"), ErrUnauthenticated))
	admin := WithPrincipal(t.Context(), Principal{Admin: true})
	assert.NoErr(manager.Authorize(admin, PermissionManagementConfigure, ""))
	assert.True(errors.Is(manager.Authorize(admin, Permission("unknown"), "team"), ErrForbidden))
	user := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{PermissionClusterLink, PermissionTokenCreate}})
	assert.NoErr(manager.Authorize(user, PermissionClusterLink, "team"))
	assert.True(errors.Is(manager.Authorize(user, PermissionClusterLink, "another"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(user, PermissionClusterCredentials, "team"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(user, PermissionManagementConfigure, ""), ErrForbidden))
	_, err = manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{PermissionClusterCredentials}, ExpiresInSeconds: 3600})
	assert.True(errors.Is(err, ErrForbidden))
	_, err = manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{"*"}, ExpiresInSeconds: 3600})
	assert.True(errors.Is(err, ErrForbidden))
	token, err := manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{PermissionClusterLink}, ExpiresInSeconds: 3600})
	assert.NoErr(err)
	assert.True(!token.ExpiresAt.After(identity.ExpiresAt))
	records, err := manager.database.List(t.Context(), "api-token")
	assert.NoErr(err)
	encoded, err := json.Marshal(records)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), token.Token))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	principal, err := manager.AuthenticateApiToken(t.Context(), token.Token)
	assert.NoErr(err)
	authenticated := WithPrincipal(t.Context(), principal)
	assert.NoErr(manager.Authorize(authenticated, PermissionClusterLink, "team"))
	assert.True(errors.Is(manager.Authorize(authenticated, PermissionTokenCreate, "team"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(authenticated, PermissionClusterLink, "another"), ErrForbidden))
	_, err = manager.AuthenticateApiToken(t.Context(), token.Token+"x")
	assert.True(errors.Is(err, ErrUnauthenticated))
	parent, err := manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{PermissionClusterLink, PermissionTokenCreate}, ExpiresInSeconds: 60})
	assert.NoErr(err)
	delegator, err := manager.AuthenticateApiToken(t.Context(), parent.Token)
	assert.NoErr(err)
	child, err := manager.CreateApiToken(WithPrincipal(t.Context(), delegator), CreateApiTokenInput{Permissions: []Permission{PermissionClusterLink}, ExpiresInSeconds: 3600})
	assert.NoErr(err)
	assert.True(!child.ExpiresAt.After(parent.ExpiresAt))
}

func TestScopedClusterCredentialsAndRendering(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	ctx := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{PermissionClusterLink, PermissionAppDeploy}})
	for _, input := range []ClusterInput{
		{InCluster: true}, {KubeconfigPath: "/server/management.yaml"}, {},
		{Kubeconfig: "apiVersion: v1\nkind: Config\n"},
		{Kubeconfig: "apiVersion: v1\nkind: Config\ncurrent-context: target\ncontexts:\n- name: target\n  context:\n    cluster: target\nclusters:\n- name: target\n  cluster:\n    server: http://localhost:8080\n"},
		{Kubeconfig: "apiVersion: v1\nkind: Config\nusers:\n- name: user\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1beta1\n      command: /bin/false\n"},
		{Kubeconfig: "apiVersion: v1\nkind: Config\nusers:\n- name: user\n  user:\n    tokenFile: /server/private-token\n"},
		{Kubeconfig: "apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    certificate-authority: /server/private-ca\n"},
		{Kubeconfig: "apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    proxy-url: http://localhost:8200\n"},
	} {
		input.Organization, input.Name = "team", "example"
		_, err := manager.RegisterCluster(ctx, input)
		assert.True(errors.Is(err, ErrForbidden))
	}
	assert.NoErr(validateClusterCredentialAuthority(ctx, ClusterInput{Kubeconfig: "apiVersion: v1\nkind: Config\ncurrent-context: target\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: user\nclusters:\n- name: target\n  cluster:\n    server: https://target.example.test\nusers:\n- name: user\n  user:\n    token: provided-static-credential\n"}))
	_, err = manager.RenderApps(ctx, Configuration{}, "preview", "preview", "preview", "", ResourceScope{Organization: "other", Space: "dev", Cluster: "target"})
	assert.True(errors.Is(err, ErrForbidden))
	plan := planRecord{Plan: Plan{ID: "other-plan", Space: Space{ID: "other-space", Namespace: "other", Organization: "other", Name: "dev", Cluster: "target"}}}
	write, err := documentWrite("plan", "other-plan", plan, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.RenderApps(ctx, Configuration{}, "other", "other-space", "other-plan", "", ResourceScope{})
	assert.True(errors.Is(err, ErrForbidden))
	_, err = manager.RenderApps(WithPrincipal(t.Context(), Principal{Admin: true}), Configuration{}, "other", "other-space", "other-plan", "", ResourceScope{})
	assert.NoErr(err)
}

// TestOrganizationWildcardCannotAdministerInstallation keeps legacy wildcard
// organization grants and derived tokens within their organization authority.
func TestOrganizationWildcardCannotAdministerInstallation(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	grants, err := manager.GetOrganizationPermissions(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(grants.Permissions, []Permission{"*"})
	installation := WithPrincipal(t.Context(), Principal{Admin: true})
	for _, scope := range [][]Permission{{"*"}, {PermissionManagementRead, PermissionManagementConfigure, PermissionOrganizationCreate, PermissionTokenCreate}} {
		user := WithPrincipal(t.Context(), Principal{Login: login, Permissions: scope})
		for _, permission := range []Permission{PermissionManagementRead, PermissionManagementConfigure, PermissionOrganizationCreate} {
			for _, organization := range []string{"", "team"} {
				assert.True(errors.Is(manager.Authorize(user, permission, organization), ErrForbidden))
				assert.NoErr(manager.Authorize(installation, permission, organization))
			}
			_, err := manager.CreateApiToken(user, CreateApiTokenInput{Permissions: []Permission{permission}, ExpiresInSeconds: 60})
			assert.True(errors.Is(err, ErrForbidden))
		}
	}
	user := WithPrincipal(t.Context(), Principal{Login: login, Permissions: []Permission{"*"}})
	for _, permission := range []Permission{PermissionOrganizationRead, PermissionOrganizationConfigure, PermissionUserManage, PermissionClusterLink} {
		assert.NoErr(manager.Authorize(user, permission, "team"))
		assert.True(errors.Is(manager.Authorize(user, permission, "other"), ErrForbidden))
	}
}
