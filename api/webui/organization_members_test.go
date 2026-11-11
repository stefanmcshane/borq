package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestOrganizationMembersUi owns role form conversion, empty assignments,
// revision-checked deletion, and API rejection propagation. Manager tests own grants.
func TestOrganizationMembersUi(t *testing.T) {
	assert := is.New(t)
	var submitted map[string]any
	denied := false
	member := appapi.OrganizationMember{ID: "member-1", Email: "viewer@example.test", Issuer: "https://id.test", State: "active", Revision: 1, RoleIds: []string{}}
	role := appapi.OrganizationRole{ID: "role-1", Name: "Viewer", Permissions: []appapi.GrantedPermission{"app:read"}, Revision: 2}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team", ID: "team-id"}}
		case "/api/v1/organizations/team/members":
			assert.Equal(request.Method, http.MethodGet)
			output = []appapi.OrganizationMember{member}
		case "/api/v1/organizations/team/roles":
			if request.Method == http.MethodPost {
				assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
				output = role
			} else {
				output = []appapi.OrganizationRole{role}
			}
		case "/api/v1/organizations/team/roles/role-1":
			if request.Method == http.MethodDelete {
				assert.Equal(request.URL.Query().Get("revision"), "2")
				assert.NoErr(json.NewEncoder(writer).Encode(map[string]string{"id": "role-1"}))
				return
			}
			assert.Equal(request.Method, http.MethodPut)
			assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
			output = role
		case "/api/v1/organizations/team/members/member-1":
			assert.Equal(request.Method, http.MethodPut)
			assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
			if denied {
				writer.WriteHeader(http.StatusForbidden)
				output = appapi.Problem{Message: "Permission denied."}
			} else {
				output = member
			}
		default:
			t.Errorf("members page requested unrelated API resource: %s", request.URL.Path)
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	perform := func(form url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Permissions: []apps.Permission{apps.PermissionUserManage}}))
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		return response
	}
	form := url.Values{"action": {"put-organization-member"}, "organization": {"team"}, "member": {"member-1"}, "revision": {"7"}, "disabled": {"true"}, "roleIds": {"role-1"}}
	response := perform(form)
	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(submitted["roleIds"], []any{"role-1"})
	assert.Equal(submitted["revision"], float64(7))
	assert.Equal(submitted["disabled"], true)
	_, directGrants := submitted["permissions"]
	assert.True(!directGrants)
	assert.True(page.CanManageUsers)
	assert.True(!page.CanConfigureOrganization)
	assert.Equal(page.Roles, []appapi.OrganizationRole{role})
	assert.True(!strings.Contains(strings.Join(page.MemberPermissions, ","), "management:"))
	assert.True(strings.Contains(","+strings.Join(page.MemberPermissions, ",")+",", ",user:manage,"))
	form.Del("roleIds")
	response = perform(form)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(submitted["roleIds"], []any{})
	denied = true
	response = perform(form)
	assert.Equal(response.Code, http.StatusForbidden)
	assert.True(strings.Contains(response.Body.String(), "Permission denied."))
	denied = false
	for _, action := range []string{"create-organization-role", "put-organization-role"} {
		form = url.Values{"action": {action}, "organization": {"team"}, "role": {"role-1"}, "name": {"Viewer"}, "revision": {"2"}, "permissions": {"organization:read", "app:read"}}
		response = perform(form)
		assert.Equal(response.Code, http.StatusOK)
		assert.Equal(submitted["name"], "Viewer")
		assert.Equal(submitted["permissions"], []any{"organization:read", "app:read"})
	}
	form.Set("action", "delete-organization-role")
	response = perform(form)
	assert.Equal(response.Code, http.StatusOK)
}

// TestOrganizationAwaitingRole shows successful authentication without querying
// organization data that the newly signed-in identity cannot yet access.
func TestOrganizationAwaitingRole(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unassigned members must not request organization data")
	}))
	assert.NoErr(err)
	request := httptest.NewRequest(http.MethodGet, "/api/ui/page?organization=other", nil)
	request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Login: apps.OrganizationLogin{Organization: "team"}}))
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	page := decodeView[pageData](t, response, "workspace")
	assert.True(page.AwaitingRole)
	assert.Equal(page.Organization, "team")
	assert.True(!page.CanManageUsers)
	assert.Equal(len(page.Organizations), 0)
}
