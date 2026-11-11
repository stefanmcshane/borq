package appapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationRoleDocumentSchema owns strict HTTP decoding: member writes
// accept role assignments, and obsolete direct permission edits are rejected.
func TestOrganizationRoleDocumentSchema(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, scenario := range []struct {
		schema, body string
		valid        bool
	}{
		{"OrganizationMemberInput", `{"roleIds":[],"disabled":false,"revision":1}`, true},
		{"OrganizationMemberInput", `{"roleIds":["reader"],"disabled":false,"revision":1}`, true},
		{"OrganizationMemberInput", `{"permissions":["app:read"],"disabled":false,"revision":1}`, false},
		{"OrganizationMemberInput", `{"roleIds":["reader","reader"],"disabled":false,"revision":1}`, false},
		{"OrganizationRoleInput", `{"name":"Reader","permissions":["app:read"],"revision":0}`, true},
		{"OrganizationRoleInput", `{"name":"Reader","permissions":[],"revision":3}`, true},
		{"OrganizationRoleInput", `{"name":"Reader","permissions":["*"],"revision":0}`, false},
		{"OrganizationRoleInput", `{"name":"Reader","permissions":["app:read"],"revision":-1}`, false},
	} {
		var output any
		response := httptest.NewRecorder()
		accepted := server.decodeBody(response, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(scenario.body)), scenario.schema, &output)
		if accepted != scenario.valid {
			t.Fatalf("%s accepted=%v want=%v: %s", scenario.body, accepted, scenario.valid, response.Body.String())
		}
	}
}

// TestOrganizationRoleRoutesRequireAuthentication protects every role operation
// and verifies removed invitation and direct-grant routes cannot dispatch.
func TestOrganizationRoleRoutesRequireAuthentication(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "roles.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	dispatched := false
	routes, err := NewAuthorizationHandler(manager, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dispatched = true }))
	assert.NoErr(err)
	for _, scenario := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/organizations/team/roles", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/organizations/team/roles", http.StatusUnauthorized},
		{http.MethodPut, "/api/v1/organizations/team/roles/reader", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/organizations/team/roles/reader?revision=1", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/organizations/team/members", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/organizations/team/members/member/enrollment", http.StatusNotFound},
		{http.MethodPut, "/api/v1/organizations/team/permissions", http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(scenario.method, scenario.path, nil))
		assert.Equal(response.Code, scenario.status)
	}
	assert.True(!dispatched)
}

// TestOrganizationRoleHttpLifecycle owns role response encoding and HTTP status
// translation, including required deletion revisions and stale-update conflicts.
func TestOrganizationRoleHttpLifecycle(t *testing.T) {
	assert := is.New(t)
	path := filepath.Join(t.TempDir(), "roles.sqlite")
	database, err := sqlite.New(t.Context(), path)
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{
		{Collection: "organization", Key: "team", Value: json.RawMessage(`{"id":"organization-id","name":"team"}`)},
		{Collection: "organization-oidc", Key: "organization-id", Value: json.RawMessage(`{"organizationId":"organization-id","generation":"provider-generation","settings":{"issuerUrl":"https://id.example.test","clientId":"borq"}}`)},
		{Collection: "organization-login-policy", Key: "organization-id", Value: json.RawMessage(`{"organizationId":"organization-id","adminEmail":"owner@example.test","issuer":"https://id.example.test","clientId":"borq"}`)},
	}))
	assert.NoErr(database.Close())
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: path})
	assert.NoErr(err)
	defer manager.Close()
	routes, err := NewHandler(manager)
	assert.NoErr(err)
	requestRole := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true}))
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, request)
		return response
	}
	base := "/api/v1/organizations/team/roles"
	created := requestRole(http.MethodPost, base, `{"name":"Reader","permissions":["app:read"],"revision":0}`)
	assert.Equal(created.Code, http.StatusCreated)
	var role OrganizationRole
	assert.NoErr(json.Unmarshal(created.Body.Bytes(), &role))
	assert.Equal(role.Name, "Reader")
	assert.Equal(role.Permissions, []GrantedPermission{GrantedPermission("app:read")})
	assert.Equal(role.Revision, int64(1))
	listed := requestRole(http.MethodGet, base, "")
	assert.Equal(listed.Code, http.StatusOK)
	var roles []OrganizationRole
	assert.NoErr(json.Unmarshal(listed.Body.Bytes(), &roles))
	assert.Equal(roles, []OrganizationRole{role})
	updated := requestRole(http.MethodPut, base+"/"+role.ID, `{"name":"Reader","permissions":["app:read","organization:read"],"revision":1}`)
	assert.Equal(updated.Code, http.StatusOK)
	assert.NoErr(json.Unmarshal(updated.Body.Bytes(), &role))
	assert.Equal(role.Revision, int64(2))
	stale := requestRole(http.MethodPut, base+"/"+role.ID, `{"name":"Reader","permissions":[],"revision":1}`)
	assert.Equal(stale.Code, http.StatusConflict)
	for _, query := range []string{"", "?revision=0"} {
		invalid := requestRole(http.MethodDelete, base+"/"+role.ID+query, "")
		assert.Equal(invalid.Code, http.StatusBadRequest)
	}
	deleted := requestRole(http.MethodDelete, base+"/"+role.ID+"?revision=2", "")
	assert.Equal(deleted.Code, http.StatusOK)
	var result DeletedOrganizationRole
	assert.NoErr(json.Unmarshal(deleted.Body.Bytes(), &result))
	assert.Equal(result.ID, role.ID)
}
