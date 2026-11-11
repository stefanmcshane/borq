package appmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPermissionTransportParity drives the real MCP SDK and HTTP authorization
// against durable provider bindings and grants, including an authenticated user
// with no permissions, partial grants and a different organization's resource.
func TestPermissionTransportParity(t *testing.T) {
	assert := is.New(t)
	file := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := sqlite.New(t.Context(), file)
	assert.NoErr(err)
	organizationID := strings.Repeat("a", 32)
	grants := []apps.Permission{apps.PermissionOrganizationRead, apps.PermissionClusterRead, apps.PermissionTokenCreate, apps.PermissionUserManage, apps.PermissionAddonRead, apps.PermissionAddonConfigure, apps.PermissionAddonDelete, apps.PermissionSpaceConfigure}
	documents := []struct {
		collection, key string
		value           any
	}{
		{"organization", "team", apps.Organization{ID: organizationID, Name: "team"}},
		{"space", "team/dev", map[string]any{"space": apps.Space{ID: "own-space", Organization: "team", Name: "dev", Namespace: "team-dev", Cluster: "local", Configuration: apps.Configuration{Apps: []apps.App{}}}}},
		{"pocketid-operation", "own-operation", map[string]any{"id": "own-operation", "spaceId": "own-space", "status": "succeeded", "phase": "pocketid", "message": "ready", "planId": "", "resources": []any{}, "startedAt": time.Now(), "updatedAt": time.Now()}},
		{"pocketid-operation", "foreign-operation", map[string]any{"id": "foreign-operation", "spaceId": "foreign-space", "status": "succeeded", "phase": "pocketid", "message": "private", "planId": "", "resources": []any{}, "startedAt": time.Now(), "updatedAt": time.Now()}},
		{"organization-oidc", organizationID, map[string]any{"organizationId": organizationID, "generation": "generation", "settings": apps.OrganizationOidc{IssuerURL: "https://identity.example.test", ClientID: "client", BaseURL: "https://borq.example.test"}}},
		{"organization-login-policy", organizationID, map[string]any{"organizationId": organizationID, "adminEmail": "owner@example.test", "issuer": "https://identity.example.test", "clientId": "client", "baseUrl": "https://borq.example.test"}},
		{"organization-login-admin", organizationID, map[string]any{"organizationId": organizationID, "issuer": "https://identity.example.test", "subject": "owner", "permissions": grants}},
	}
	var writes []sqlite.Write
	for _, document := range documents {
		value, err := json.Marshal(document.value)
		assert.NoErr(err)
		writes = append(writes, sqlite.Write{Collection: document.collection, Key: document.key, Value: value})
	}
	assert.NoErr(database.Commit(t.Context(), writes))
	assert.NoErr(database.Close())
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: file})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	login := apps.OrganizationLogin{Organization: "team", OrganizationID: organizationID, Issuer: "https://identity.example.test", Subject: "owner", Generation: "generation", ExpiresAt: time.Now().Add(time.Hour)}
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	api, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	authorized, err := appapi.NewAuthorizationHandler(manager, api)
	assert.NoErr(err)
	for _, test := range []struct {
		name, organization string
		permissions        []apps.Permission
		anonymous          bool
		status             int
	}{
		{"anonymous", "team", nil, true, http.StatusUnauthorized},
		{"zero grants", "team", []apps.Permission{}, false, http.StatusForbidden},
		{"partial grant", "team", []apps.Permission{apps.PermissionOrganizationRead}, false, http.StatusForbidden},
		{"allowed", "team", []apps.Permission{apps.PermissionClusterRead}, false, http.StatusOK},
		{"cross organization", "other", []apps.Permission{apps.PermissionClusterRead}, false, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			dispatch := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !test.anonymous {
					request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Login: login, Permissions: test.permissions}))
				}
				authorized.ServeHTTP(writer, request)
			})
			response := httptest.NewRecorder()
			dispatch.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/"+test.organization+"/clusters", nil))
			assert.Equal(response.Code, test.status)
			ctx, session := connectTools(t, dispatch)
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ListClusters", Arguments: map[string]any{"path": map[string]any{"organization": test.organization}}})
			assert.NoErr(err)
			assert.Equal(result.IsError, test.status >= 400)
			assert.Equal(result.Content[0].(*mcp.TextContent).Text, response.Body.String())
			tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
			assert.NoErr(err)
			found := false
			for _, tool := range tools.Tools {
				if tool.Name == "ListClusters" {
					found = true
					assert.True(strings.Contains(tool.Description, "Required permission: cluster:read."))
				}
			}
			assert.True(found)
		})
	}
	t.Run("managed Pocket ID authorization", func(t *testing.T) {
		for _, test := range []struct {
			name, method, path, tool, arguments string
			permission                          apps.Permission
			status                              int
		}{
			{"read", http.MethodGet, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid", "ListPocketID", `{"path":{"organization":"team","space":"dev"}}`, apps.PermissionAddonRead, http.StatusOK},
			{"read without grant", http.MethodGet, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid", "ListPocketID", `{"path":{"organization":"team","space":"dev"}}`, apps.PermissionAppRead, http.StatusForbidden},
			{"cross organization", http.MethodGet, "/api/v1/organizations/other/spaces/dev/managed-addons/pocketid", "ListPocketID", `{"path":{"organization":"other","space":"dev"}}`, apps.PermissionAddonRead, http.StatusForbidden},
			{"enrollment requires configure", http.MethodPost, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/identity/enrollment", "CreatePocketIDEnrollment", `{"path":{"organization":"team","space":"dev","pocketid":"identity"}}`, apps.PermissionAddonRead, http.StatusForbidden},
			{"enrollment configure", http.MethodPost, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/identity/enrollment", "CreatePocketIDEnrollment", `{"path":{"organization":"team","space":"dev","pocketid":"identity"}}`, apps.PermissionAddonConfigure, http.StatusNotFound},
			{"delete requires delete", http.MethodDelete, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/identity", "DeletePocketID", `{"path":{"organization":"team","space":"dev","pocketid":"identity"}}`, apps.PermissionAddonConfigure, http.StatusForbidden},
			{"own operation", http.MethodGet, "/api/v1/pocketid-operations/own-operation", "GetPocketIDOperation", `{"path":{"operation":"own-operation"}}`, apps.PermissionAddonRead, http.StatusOK},
			{"aggregate management operation stays private", http.MethodGet, "/api/v1/management/operations/aggregate", "GetManagementOperation", `{"path":{"operation":"aggregate"}}`, apps.PermissionAddonRead, http.StatusForbidden},
			{"foreign operation", http.MethodGet, "/api/v1/pocketid-operations/foreign-operation", "GetPocketIDOperation", `{"path":{"operation":"foreign-operation"}}`, apps.PermissionAddonRead, http.StatusForbidden},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert := is.New(t)
				dispatch := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Login: login, Permissions: []apps.Permission{test.permission}}))
					authorized.ServeHTTP(writer, request)
				})
				response := httptest.NewRecorder()
				dispatch.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
				assert.Equal(response.Code, test.status)
				ctx, session := connectTools(t, dispatch)
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: test.tool, Arguments: json.RawMessage(test.arguments)})
				assert.NoErr(err)
				assert.Equal(result.IsError, test.status >= 400)
				assert.Equal(result.Content[0].(*mcp.TextContent).Text, response.Body.String())
				if test.name == "enrollment configure" {
					assert.Equal(response.Header().Get("Cache-Control"), "no-store, private")
					assert.Equal(response.Header().Get("Referrer-Policy"), "no-referrer")
				}
			})
		}
	})
	t.Run("managed Pocket ID aggregate authorization", func(t *testing.T) {
		body := `{"managedAddons":{"pocketid":[{"name":"identity","issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}]}}`
		for _, test := range []struct {
			name        string
			permissions []apps.Permission
			status      int
		}{
			{"space grant alone", []apps.Permission{apps.PermissionSpaceConfigure}, http.StatusForbidden},
			{"space and addon grants accept work", []apps.Permission{apps.PermissionSpaceConfigure, apps.PermissionAddonConfigure}, http.StatusAccepted},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert := is.New(t)
				dispatch := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Login: login, Permissions: test.permissions}))
					authorized.ServeHTTP(writer, request)
				})
				response := httptest.NewRecorder()
				dispatch.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/organizations/team/spaces/dev/configuration", strings.NewReader(body)))
				assert.Equal(response.Code, test.status)
				ctx, session := connectTools(t, dispatch)
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ApplySpaceConfiguration", Arguments: json.RawMessage(`{"path":{"organization":"team","space":"dev"},"body":` + body + `}`)})
				assert.NoErr(err)
				assert.Equal(result.IsError, test.status >= 400)
				if test.status >= 400 {
					assert.Equal(result.Content[0].(*mcp.TextContent).Text, response.Body.String())
					return
				}
				var httpOperation, toolOperation appapi.Operation
				assert.NoErr(json.Unmarshal(response.Body.Bytes(), &httpOperation))
				assert.NoErr(json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &toolOperation))
				assert.True(httpOperation.ID != "" && toolOperation.ID != "")
				assert.Equal(httpOperation.Status, appapi.Queued)
				assert.Equal(toolOperation.Status, appapi.Queued)
			})
		}
	})
	t.Run("member and token contract", func(t *testing.T) {
		assert := is.New(t)
		dispatch := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Login: login, Permissions: grants}))
			authorized.ServeHTTP(writer, request)
		})
		call := func(method, path, body string) *httptest.ResponseRecorder {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			dispatch.ServeHTTP(response, request)
			return response
		}
		invalid := call(http.MethodPost, "/api/v1/organizations/team/roles", `{"name":"Reader","permissions":["made:up"],"revision":0}`)
		assert.Equal(invalid.Code, http.StatusBadRequest)
		elevated := call(http.MethodPost, "/api/v1/organizations/team/roles", `{"name":"Reader","permissions":["cluster:credentials"],"revision":0}`)
		assert.Equal(elevated.Code, http.StatusForbidden)
		response := call(http.MethodPost, "/api/v1/organizations/team/roles", `{"name":"Reader","permissions":["cluster:read"],"revision":0}`)
		assert.Equal(response.Code, http.StatusCreated)
		var role appapi.OrganizationRole
		assert.NoErr(json.Unmarshal(response.Body.Bytes(), &role))
		assert.Equal(role.Name, "Reader")
		memberLogin, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", "generation", oidc.Identity{Issuer: login.Issuer, Subject: "member", Email: "member@example.test", EmailVerified: true, ExpiresAt: time.Now().Add(time.Hour)})
		assert.NoErr(err)
		listed := call(http.MethodGet, "/api/v1/organizations/team/members", "")
		assert.Equal(listed.Code, http.StatusOK)
		assert.True(!strings.Contains(listed.Body.String(), "enrollmentUrl"))
		assert.True(!strings.Contains(listed.Body.String(), "credentialsRef"))
		updated := call(http.MethodPut, "/api/v1/organizations/team/members/"+memberLogin.MemberID, `{"roleIds":["`+role.ID+`"],"disabled":true,"revision":1}`)
		assert.Equal(updated.Code, http.StatusOK)
		var member appapi.OrganizationMember
		assert.NoErr(json.Unmarshal(updated.Body.Bytes(), &member))
		assert.Equal(member.State, appapi.Disabled)
		assert.Equal(member.Revision, int64(2))
		stale := call(http.MethodPut, "/api/v1/organizations/team/members/"+memberLogin.MemberID, `{"roleIds":[],"disabled":false,"revision":1}`)
		assert.Equal(stale.Code, http.StatusConflict)
		tokenResponse := call(http.MethodPost, "/api/auth/tokens", `{"permissions":["cluster:read"],"expiresInSeconds":60}`)
		assert.Equal(tokenResponse.Code, http.StatusOK)
		assert.True(strings.Contains(tokenResponse.Header().Get("Cache-Control"), "no-store"))
		var token appapi.APIToken
		assert.NoErr(json.Unmarshal(tokenResponse.Body.Bytes(), &token))
		tokenPrincipal, err := manager.AuthenticateApiToken(t.Context(), token.Token)
		assert.NoErr(err)
		assert.Equal(tokenPrincipal.Permissions, []apps.Permission{apps.PermissionClusterRead})
		ctx, session := connectTools(t, dispatch)
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "CreateAPIToken", Arguments: map[string]any{"body": map[string]any{"permissions": []string{"cluster:read"}, "expiresInSeconds": 60}}})
		assert.NoErr(err)
		assert.True(!result.IsError)
		assert.NoErr(json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &token))
		_, err = manager.AuthenticateApiToken(t.Context(), token.Token)
		assert.NoErr(err)
	})

}
