package appserver

import (
	"bytes"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

func authenticationCall(handler http.Handler, method, address, body, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, address, strings.NewReader(body))
	if body != "" {
		if strings.HasPrefix(body, "{") {
			request.Header.Set("Content-Type", "application/json")
		} else {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestServerAuthenticationDiagnostics makes failed MCP sign-in discoverable in
// server logs without exposing credentials or request query parameters.
func TestServerAuthenticationDiagnostics(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(server.Close()) })
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, test := range []struct {
		name, path, header, credential, event, reason string
		status                                        int
	}{
		{"missing token", "/api/mcp", "", "", "authentication_rejected", "missing_credentials", http.StatusUnauthorized},
		{"invalid token", "/api/mcp", "Authorization", "Bearer private-token", "authentication_rejected", "invalid_credentials", http.StatusUnauthorized},
		{"invalid proxy token", "/api/mcp", "X-Borq-Admin-Token", "private-proxy-token", "authentication_rejected", "invalid_credentials", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			output.Reset()
			request := httptest.NewRequest(http.MethodGet, "https://borq.test"+test.path+"?token=private-query", nil)
			if test.header != "" {
				request.Header.Set(test.header, test.credential)
			}
			request.AddCookie(&http.Cookie{Name: "borq-session", Value: "private-cookie"})
			request.Header.Set("User-Agent", "private-user-agent")
			response := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(response, request)
			assert.Equal(response.Code, test.status)
			var record map[string]any
			assert.NoErr(json.Unmarshal(output.Bytes(), &record))
			assert.Equal(record["level"], "WARN")
			assert.Equal(record["msg"], test.event)
			assert.Equal(record["method"], http.MethodGet)
			assert.Equal(record["path"], test.path)
			assert.Equal(record["status"], float64(test.status))
			assert.Equal(record["reason"], test.reason)
			assert.True(!strings.Contains(output.String(), "private-"))
		})
	}
	output.Reset()
	request := httptest.NewRequest(http.MethodGet, "https://borq.test/api/auth/session", nil)
	authorizeAdminTestRequest(t, directory, request)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(output.Len(), 0)
}

// TestOrganizationLoginSelector exposes only usable organization names without
// requiring credentials or contacting identity providers to render the page.
func TestOrganizationLoginSelector(t *testing.T) {
	assert := is.New(t)
	provider, kubeconfig, _ := oidcProviderFixture(t, true)
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: t.TempDir(), ManagementKubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer server.Close()
	base := "https://borq.example.test"
	response := authenticationCall(server.http.Handler, http.MethodGet, base+"/login", "", "")
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "No organizations are ready for sign-in"))
	assert.True(strings.Contains(response.Body.String(), `href="/login/admin"`))
	for _, name := range []string{"zulu", "alpha"} {
		_, err := server.manager.OnboardOrganization(t.Context(), name, apps.OrganizationOnboardingInput{
			AdminEmail: "owner@example.test", Oidc: &apps.OrganizationOidcInput{IssuerURL: provider.URL,
				BaseURL: base, ClientID: "borq-client", ClientSecret: "private-client-secret"},
		})
		assert.NoErr(err)
		response = authenticationCall(server.http.Handler, http.MethodGet, base+"/login", "", "")
		if name == "zulu" {
			assert.True(strings.Contains(response.Body.String(), `<option value="zulu" selected>zulu</option>`))
		}
	}
	_, err = server.manager.CreateOrganization(t.Context(), "unconfigured")
	assert.NoErr(err)
	provider.Close()
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/login", "", "")
	assert.Equal(response.Code, http.StatusOK)
	page := response.Body.String()
	assert.True(strings.Contains(page, `<select class="input" name="organization" required id="organization">`))
	alpha, zulu := strings.Index(page, `<option value="alpha"`), strings.Index(page, `<option value="zulu"`)
	assert.True(alpha >= 0 && zulu > alpha)
	for _, private := range []string{"unconfigured", "owner@example.test", "private-client-secret", "borq-client", provider.URL} {
		assert.True(!strings.Contains(page, private))
	}
	assert.True(!strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action"))
	invitationPage := authenticationCall(server.http.Handler, http.MethodGet, base+"/login?organization=zulu", "", "")
	assert.True(strings.Contains(invitationPage.Body.String(), `<option value="zulu" selected>zulu</option>`))
	unknownPage := authenticationCall(server.http.Handler, http.MethodGet, base+"/login?organization=unconfigured", "", "")
	assert.True(!strings.Contains(unknownPage.Body.String(), `<option value="unconfigured"`))
	// A public selector does not make the organization API public.
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations", "", "").Code, http.StatusUnauthorized)
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/login/admin", "", "")
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), `name="token"`))
	assert.True(!strings.Contains(response.Body.String(), `<select`))
	assert.True(strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self'"))
	assert.NoErr(server.manager.Close())
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/login", "", "").Code, http.StatusServiceUnavailable)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/login/admin", "", "").Code, http.StatusOK)
}

func TestServerAuthenticationAndRecovery(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	defer func() { _ = server.Close() }()
	path := filepath.Join(directory, "admin-token")
	credential, err := os.ReadFile(path)
	assert.NoErr(err)
	stat, err := os.Stat(path)
	assert.NoErr(err)
	assert.Equal(stat.Mode().Perm(), os.FileMode(0600))
	token := strings.TrimSpace(string(credential))
	assert.True(len(token) >= 32)
	base := "https://borq.test"
	for _, path := range []string{"/api/v1/organizations", "/api/ui/page", "/api/mcp", "/api/auth/session", "/api/openapi.yaml", "/api/auth/pocketid/admin/team"} {
		response := authenticationCall(server.http.Handler, http.MethodGet, base+path, "", "")
		assert.Equal(response.Code, http.StatusUnauthorized)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	}
	response := authenticationCall(server.http.Handler, http.MethodGet, base+"/", "", "")
	assert.Equal(response.Code, http.StatusSeeOther)
	assert.Equal(response.Header().Get("Location"), "/login")
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/livez", "", "").Code, http.StatusOK)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/auth/session", "", "wrong").Code, http.StatusUnauthorized)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations", "", token).Code, http.StatusOK)
	checkPath := base + "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/restore-check"
	response = authenticationCall(server.http.Handler, http.MethodPost, checkPath, "{}", "")
	assert.Equal(response.Code, http.StatusUnauthorized)
	assert.True(strings.Contains(response.Body.String(), "projected restore-check token"))
	response = authenticationCall(server.http.Handler, http.MethodPost, checkPath, "{}", "projected-token")
	assert.Equal(response.Code, http.StatusBadRequest)
	assert.True(strings.Contains(response.Body.String(), "restore-check jobUid"))
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations", "", "projected-token").Code, http.StatusUnauthorized)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/login", url.Values{"token": {token}}.Encode(), "")
	assert.Equal(response.Code, http.StatusSeeOther)
	cookie := response.Result().Cookies()[0]
	assert.True(cookie.HttpOnly && cookie.Secure)
	assert.Equal(cookie.SameSite, http.SameSiteLaxMode)
	assert.True(cookie.Value != token)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page", "", "", cookie).Code, http.StatusOK)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations", "", "", cookie).Code, http.StatusUnauthorized)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/logout", "", "", cookie).Code, http.StatusSeeOther)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page", "", "", cookie).Code, http.StatusUnauthorized)
	request := httptest.NewRequest(http.MethodPost, base+"/api/auth/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://foreign.test")
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusForbidden)
	assert.NoErr(server.Close())
	server, err = New(t.Context(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	again, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.Equal(string(again), string(credential))
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/auth/session", "", token).Code, http.StatusOK)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page", "", "", cookie).Code, http.StatusUnauthorized)
}

// The provider signs a real ID token and validates PKCE and client credentials.
// Only its discovery and private secret storage are supplied by the fixture.
func TestOrganizationLoginAcrossUiApiAndMcp(t *testing.T) {
	assert := is.New(t)
	provider, kubeconfig, _ := oidcProviderFixture(t, true)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory, ManagementKubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer server.Close()
	base := "https://borq.example.test"
	_, err = server.manager.OnboardOrganization(t.Context(), "team", apps.OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &apps.OrganizationOidcInput{IssuerURL: provider.URL, BaseURL: base, ClientID: "borq-client", ClientSecret: "private-client-secret"}})
	assert.NoErr(err)
	_, err = server.manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	response := authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/oidc/login", url.Values{"organization": {"team"}}.Encode(), "")
	assert.Equal(response.Code, http.StatusSeeOther)
	attemptCookie := response.Result().Cookies()[0]
	client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorization, err := client.Get(response.Header().Get("Location"))
	assert.NoErr(err)
	defer authorization.Body.Close()
	response = authenticationCall(server.http.Handler, http.MethodGet, authorization.Header.Get("Location"), "", "", attemptCookie)
	assert.Equal(response.Code, http.StatusSeeOther)
	var sessionCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "borq-session" {
			sessionCookie = cookie
		}
	}
	assert.True(sessionCookie != nil)
	account := authenticationCall(server.http.Handler, http.MethodGet, base+"/account", "", "", sessionCookie)
	assert.Equal(account.Code, http.StatusOK)
	assert.True(strings.Contains(account.Body.String(), `value="organization:read,cluster:read,space:read,app:read"`))
	assert.True(!strings.Contains(account.Body.String(), "cluster:link"))
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page?organization=team", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), `"name":"other"`))
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page?organization=other", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusForbidden)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/tokens", `{"permissions":["organization:read","organization:configure","user:manage","cluster:link","database:read"],"expiresInSeconds":3600}`, "", sessionCookie)
	assert.Equal(response.Code, http.StatusOK)
	var token apps.ApiToken
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &token))
	assert.True(token.Token != "")
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations", "", token.Token)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), "other"))
	// This is the pinned initial organization administrator, whose durable
	// wildcard grants and browser session must not grant provider administration.
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/pocketid/admin/team", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusForbidden)
	assert.Equal(response.Header().Get("Location"), "")
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/tokens", `{"permissions":["management:configure"],"expiresInSeconds":60}`, "", sessionCookie)
	assert.Equal(response.Code, http.StatusForbidden)
	for _, path := range []string{"/pocketid", "/oidc", "/permissions"} {
		response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations/team"+path, "", token.Token)
		assert.Equal(response.Code, http.StatusOK)
	}
	response = authenticationCall(server.http.Handler, http.MethodPut, base+"/api/v1/organizations/team/oidc", `{"adminEmail":"owner@example.test","issuerUrl":"https://id.example.test","clientId":"client","borqPublicUrl":"https://borq.example.test","revision":0}`, token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	providerInput := `{"issuerUrl":"https://id.example.test","borqPublicUrl":"https://borq.example.test","certificateIssuer":"cloudflare","adminEmail":"owner@example.test","oidcRevision":0}`
	for _, test := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/organizations/team/pocketid/configuration", ""},
		{http.MethodPost, "/api/v1/organizations/team/pocketid", providerInput},
		{http.MethodPost, "/api/auth/pocketid/admin/team", ""},
		{http.MethodPost, "/api/v1/organizations/team/pocketid/admin-enrollment", ""},
		{http.MethodGet, "/api/v1/management/configuration", ""},
		{http.MethodGet, "/api/v1/management/destroy", ""},
		{http.MethodPost, "/api/v1/management/destroy", ""},
		{http.MethodGet, "/api/v1/management/destroy/operations/unknown", ""},
	} {
		response = authenticationCall(server.http.Handler, test.method, base+test.path, test.body, token.Token)
		assert.Equal(response.Code, http.StatusForbidden)
	}
	// UI cookies cannot authenticate management API calls; ordinary UI accounts
	// must still pass the API's installation-administrator permission check.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response = authenticationCall(server.http.Handler, method, base+"/api/v1/management/destroy", "", "", sessionCookie)
		assert.Equal(response.Code, http.StatusUnauthorized)
	}
	// Invalid input reaches API validation only with the named permission.
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/v1/organizations/team/clusters", `{}`, token.Token)
	assert.Equal(response.Code, http.StatusBadRequest)
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations/team/clusters/example/kubeconfig", "", token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/env", "", token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/v1/organizations/other/clusters", `{}`, token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/tokens", `{"permissions":["token:create"],"expiresInSeconds":3600}`, token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	for _, test := range []struct {
		tool      string
		arguments string
		forbidden bool
	}{
		{"LinkCluster", `{"path":{"organization":"team"},"body":{"name":"example","connection":{"kubeconfig":"invalid: ["}}}`, false},
		{"LinkCluster", `{"path":{"organization":"other"},"body":{"name":"example","connection":{"kubeconfig":"invalid: ["}}}`, true},
		{"GetOrganizationPocketIDConfiguration", `{"path":{"organization":"team"}}`, true},
		{"SetupOrganizationPocketID", `{"path":{"organization":"team"},"body":` + providerInput + `}`, true},
		{"CreateOrganizationPocketIDAdminEnrollment", `{"path":{"organization":"team"}}`, true},
		{"GetOrganizationOidc", `{"path":{"organization":"team"}}`, false},
		{"PlanManagementDestroy", `{}`, true},
		{"DestroyManagement", `{}`, true},
		{"GetManagementDestroy", `{"path":{"operation":"unknown"}}`, true},
	} {
		request := httptest.NewRequest(http.MethodPost, base+"/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+test.tool+`","arguments":`+test.arguments+`}}`))
		request.Header.Set("Authorization", "Bearer "+token.Token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response = httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		assert.Equal(response.Code, http.StatusOK)
		if strings.Contains(response.Body.String(), "permission denied") != test.forbidden {
			t.Errorf("MCP %s returned an unexpected permission result", test.tool)
		}
	}
	adminToken, err := os.ReadFile(filepath.Join(directory, "admin-token"))
	assert.NoErr(err)
	for _, path := range []string{"/pocketid", "/pocketid/configuration"} {
		response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations/team"+path, "", strings.TrimSpace(string(adminToken)))
		assert.Equal(response.Code, http.StatusOK)
	}
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/v1/organizations/team/roles", "", strings.TrimSpace(string(adminToken)))
	assert.Equal(response.Code, http.StatusOK)
	var roles []apps.OrganizationRole
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &roles))
	assert.Equal(len(roles), 1)
	replacement, err := json.Marshal(apps.OrganizationRoleInput{Name: roles[0].Name, Revision: roles[0].Revision, Permissions: []apps.Permission{apps.PermissionOrganizationRead}})
	assert.NoErr(err)
	response = authenticationCall(server.http.Handler, http.MethodPut, base+"/api/v1/organizations/team/roles/"+roles[0].ID, string(replacement), strings.TrimSpace(string(adminToken)))
	assert.Equal(response.Code, http.StatusOK)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/v1/organizations/team/clusters", `{}`, token.Token)
	assert.Equal(response.Code, http.StatusForbidden)
	response = authenticationCall(server.http.Handler, http.MethodGet, base+"/api/ui/page?organization=team", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusForbidden)
	account = authenticationCall(server.http.Handler, http.MethodGet, base+"/account", "", "", sessionCookie)
	assert.Equal(account.Code, http.StatusOK)
	assert.True(!strings.Contains(account.Body.String(), `action="/account/tokens"`))
	assert.True(strings.Contains(account.Body.String(), "token:create permission"))
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/logout", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.Equal(response.Header().Get("Referrer-Policy"), "no-referrer")
	assert.True(strings.Contains(response.Header().Get("Content-Security-Policy"), "script-src 'nonce-"))
	_, link, found := strings.Cut(response.Body.String(), `<a href="`)
	assert.True(found)
	link, _, found = strings.Cut(link, `"`)
	assert.True(found)
	logout, err := client.Get(html.UnescapeString(link))
	assert.NoErr(err)
	defer logout.Body.Close()
	assert.Equal(logout.StatusCode, http.StatusFound)
	assert.Equal(logout.Header.Get("Location"), base+"/login")
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/account", "", "", sessionCookie).Code, http.StatusSeeOther)
	response = authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/logout", "", "", sessionCookie)
	assert.Equal(response.Code, http.StatusSeeOther)
	assert.Equal(response.Header().Get("Location"), "/login")
}
