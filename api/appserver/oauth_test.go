package appserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanmcshane/borq/business/server/apps"
	"golang.org/x/oauth2"
)

type oauthFixture struct {
	runtime Server
	input   NewInput
	// HTTP test servers and browser cookie jars maintain shared transport state.
	server   *httptest.Server
	browser  *http.Client
	clientID string
}

func newOAuthFixture(t *testing.T) oauthFixture {
	t.Helper()
	assert := is.New(t)
	provider, kubeconfig, _ := oidcProviderFixture(t, true)
	input := NewInput{Port: 7833, DataDir: t.TempDir(), ManagementKubeconfigPath: kubeconfig}
	runtime, err := New(t.Context(), input)
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(runtime.Close()) })
	server := httptest.NewServer(runtime.http.Handler)
	t.Cleanup(server.Close)
	_, err = runtime.manager.OnboardOrganization(t.Context(), "team", apps.OrganizationOnboardingInput{AdminEmail: "owner@example.test",
		Oidc: &apps.OrganizationOidcInput{IssuerURL: provider.URL, BaseURL: server.URL, ClientID: "borq-client", ClientSecret: "private-client-secret"}})
	assert.NoErr(err)
	_, err = runtime.manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	metadata := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(oauthClientMetadata{ID: "https://" + request.Host + "/client.json", Name: "Example MCP client",
			RedirectURIs: []string{"https://client.example/callback"}, AuthMethod: "none", GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"}})
	}))
	t.Cleanup(metadata.Close)
	// Isolate the HTTPS fixture's certificate and network. Production SSRF guards
	// are exercised independently using the unmodified metadata client.
	runtime.authentication.oauth.client.Transport = metadata.Client().Transport
	jar, err := cookiejar.New(nil)
	assert.NoErr(err)
	browser := &http.Client{Jar: jar, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return oauthFixture{runtime: runtime, input: input, server: server, browser: browser, clientID: metadata.URL + "/client.json"}
}

func oauthGet(t *testing.T, client *http.Client, address string) (int, string, string) {
	t.Helper()
	assert := is.New(t)
	response, err := client.Get(address)
	assert.NoErr(err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	assert.NoErr(err)
	return response.StatusCode, response.Header.Get("Location"), string(body)
}

func (fixture oauthFixture) consent(t *testing.T, address string) string {
	t.Helper()
	assert := is.New(t)
	status, location, body := oauthGet(t, fixture.browser, address)
	assert.Equal(status, http.StatusSeeOther)
	assert.Equal(location, "/api/auth/oauth/consent")
	status, location, body = oauthGet(t, fixture.browser, fixture.server.URL+location)
	if status == http.StatusSeeOther {
		assert.Equal(location, "/login")
		response, err := fixture.browser.PostForm(fixture.server.URL+"/api/auth/oidc/login", url.Values{"organization": {"team"}})
		assert.NoErr(err)
		assert.Equal(response.StatusCode, http.StatusSeeOther)
		authorize := response.Header.Get("Location")
		response.Body.Close()
		var callback string
		status, callback, _ = oauthGet(t, fixture.browser, authorize)
		assert.Equal(status, http.StatusFound)
		status, location, _ = oauthGet(t, fixture.browser, callback)
		assert.Equal(status, http.StatusSeeOther)
		assert.Equal(location, "/api/auth/oauth/consent")
		status, _, body = oauthGet(t, fixture.browser, fixture.server.URL+location)
	}
	assert.Equal(status, http.StatusOK)
	assert.True(strings.Contains(body, "Example MCP client"))
	assert.True(strings.Contains(body, "team"))
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	assert.Equal(len(match), 2)
	return match[1]
}

func (fixture oauthFixture) approve(t *testing.T, csrf, decision string) *url.URL {
	t.Helper()
	assert := is.New(t)
	response, err := fixture.browser.PostForm(fixture.server.URL+"/api/auth/oauth/consent", url.Values{"csrf": {csrf}, "decision": {decision}})
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.StatusCode, http.StatusSeeOther)
	redirect, err := url.Parse(response.Header.Get("Location"))
	assert.NoErr(err)
	assert.Equal(redirect.Scheme+"://"+redirect.Host+redirect.Path, "https://client.example/callback")
	assert.Equal(redirect.Query().Get("iss"), fixture.server.URL)
	return redirect
}

func (fixture oauthFixture) grant(t *testing.T) (url.Values, string) {
	t.Helper()
	address, verifier := fixture.authorization("organization:read")
	csrf := fixture.consent(t, address)
	redirect := fixture.approve(t, csrf, "allow")
	is.New(t).Equal(redirect.Query().Get("state"), "client-state")
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {fixture.clientID}, "redirect_uri": {"https://client.example/callback"},
		"resource": {fixture.server.URL + "/api/mcp"}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}}, csrf
}

func (fixture oauthFixture) authorization(scope string) (string, string) {
	verifier := oauth2.GenerateVerifier()
	values := url.Values{"response_type": {"code"}, "client_id": {fixture.clientID}, "redirect_uri": {"https://client.example/callback"},
		"resource": {fixture.server.URL + "/api/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)},
		"scope": {scope}, "state": {"client-state"}}
	return fixture.server.URL + "/api/auth/oauth/authorize?" + values.Encode(), verifier
}

func (fixture oauthFixture) exchange(t *testing.T, values url.Values) (int, map[string]any) {
	t.Helper()
	assert := is.New(t)
	response, err := fixture.server.Client().PostForm(fixture.server.URL+"/api/auth/oauth/token", values)
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.Header.Get("Cache-Control"), "no-store")
	var result map[string]any
	assert.NoErr(json.NewDecoder(response.Body).Decode(&result))
	return response.StatusCode, result
}

// TestMcpOAuthCimdWorkflow connects an actual MCP SDK client through discovery,
// HTTPS client metadata, signed organization login, consent and PKCE exchange.
func TestMcpOAuthCimdWorkflow(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: fixture.clientID}, RedirectURL: "https://client.example/callback",
		Client: fixture.server.Client(), AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			csrf := fixture.consent(t, args.URL)
			redirect := fixture.approve(t, csrf, "allow")
			return &auth.AuthorizationResult{Code: redirect.Query().Get("code"), State: redirect.Query().Get("state"), Iss: redirect.Query().Get("iss")}, nil
		},
	})
	assert.NoErr(err)
	client := mcp.NewClient(&mcp.Implementation{Name: "cimd-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: fixture.server.URL + "/api/mcp", HTTPClient: fixture.server.Client(), OAuthHandler: handler}, nil)
	assert.NoErr(err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ListOrganizations"})
	assert.NoErr(err)
	assert.True(!result.IsError)
	assert.True(strings.Contains(result.Content[0].(*mcp.TextContent).Text, "team"))
	assert.True(!strings.Contains(result.Content[0].(*mcp.TextContent).Text, "other"))
	denied, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "PutOrganization", Arguments: map[string]any{"path": map[string]any{"organization": "another"}}})
	assert.NoErr(err)
	assert.True(denied.IsError)
	assert.Equal(denied.StructuredContent.(map[string]any)["status"], float64(http.StatusForbidden))
}

// TestMcpOAuthDiscovery owns public discovery, PKCE/CIMD advertisement and host
// trust; forwarded headers cannot choose the issuer or resource audience.
func TestMcpOAuthDiscovery(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/api/mcp", "/.well-known/oauth-authorization-server"} {
		response, err := fixture.server.Client().Get(fixture.server.URL + path)
		assert.NoErr(err)
		assert.Equal(response.StatusCode, http.StatusOK)
		assert.Equal(response.Header.Get("Cache-Control"), "no-store")
		var metadata map[string]any
		assert.NoErr(json.NewDecoder(response.Body).Decode(&metadata))
		response.Body.Close()
		if strings.Contains(path, "authorization-server") {
			assert.Equal(metadata["issuer"], fixture.server.URL)
			assert.Equal(metadata["client_id_metadata_document_supported"], true)
			assert.Equal(metadata["code_challenge_methods_supported"], []any{"S256"})
		} else {
			assert.Equal(metadata["resource"], fixture.server.URL+"/api/mcp")
			assert.Equal(metadata["authorization_servers"], []any{fixture.server.URL})
		}
		request := httptest.NewRequest(http.MethodGet, "https://untrusted.example"+path, nil)
		request.Header.Set("X-Forwarded-Host", strings.TrimPrefix(fixture.server.URL, "http://"))
		responseRecorder := httptest.NewRecorder()
		fixture.runtime.http.Handler.ServeHTTP(responseRecorder, request)
		assert.Equal(responseRecorder.Code, http.StatusBadRequest)
	}
	response, err := fixture.server.Client().Get(fixture.server.URL + "/api/mcp")
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.StatusCode, http.StatusUnauthorized)
	assert.True(strings.Contains(response.Header.Get("WWW-Authenticate"), `resource_metadata="`+fixture.server.URL+`/.well-known/oauth-protected-resource/api/mcp"`))
}

// TestMcpOAuthGrantGuards rejects mismatched token exchanges at their named
// bindings, consumes failed codes, and makes each approved code single-use.
func TestMcpOAuthGrantGuards(t *testing.T) {
	fixture := newOAuthFixture(t)
	for _, test := range []struct{ name, field, value string }{
		{"PKCE", "code_verifier", strings.Repeat("x", 43)},
		{"client", "client_id", "https://other.example/client.json"},
		{"redirect", "redirect_uri", "https://client.example/callback/other"},
		{"resource", "resource", fixture.server.URL},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			values, _ := fixture.grant(t)
			original := values.Get(test.field)
			values.Set(test.field, test.value)
			status, result := fixture.exchange(t, values)
			assert.Equal(status, http.StatusBadRequest)
			assert.Equal(result["error"], "invalid_grant")
			values.Set(test.field, original)
			status, result = fixture.exchange(t, values)
			assert.Equal(status, http.StatusBadRequest)
			assert.Equal(result["error"], "invalid_grant")
		})
	}
	t.Run("concurrent redemption", func(t *testing.T) {
		assert := is.New(t)
		values, _ := fixture.grant(t)
		var group sync.WaitGroup
		statuses := make(chan int, 2)
		for range 2 {
			group.Go(func() { status, _ := fixture.exchange(t, values); statuses <- status })
		}
		group.Wait()
		close(statuses)
		counts := map[int]int{}
		for status := range statuses {
			counts[status]++
		}
		assert.Equal(counts, map[int]int{http.StatusOK: 1, http.StatusBadRequest: 1})
	})
	t.Run("expiry", func(t *testing.T) {
		assert := is.New(t)
		values, _ := fixture.grant(t)
		digest := sha256.Sum256([]byte(values.Get("code")))
		fixture.runtime.authentication.oauth.mutex.Lock()
		code := fixture.runtime.authentication.oauth.codes[digest]
		code.expires = time.Now().Add(-time.Second)
		fixture.runtime.authentication.oauth.codes[digest] = code
		fixture.runtime.authentication.oauth.mutex.Unlock()
		status, result := fixture.exchange(t, values)
		assert.Equal(status, http.StatusBadRequest)
		assert.Equal(result["error"], "invalid_grant")
	})
}

// TestMcpOAuthAccessIsolation proves consent cannot become API, administrator,
// cross-organization or cross-origin access, and expired tokens are rejected.
func TestMcpOAuthAccessIsolation(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	values, _ := fixture.grant(t)
	status, result := fixture.exchange(t, values)
	assert.Equal(status, http.StatusOK)
	token := result["access_token"].(string)
	assert.Equal(result["scope"], "organization:read")
	assert.True(result["expires_in"].(float64) > 0 && result["expires_in"].(float64) <= 60)
	assert.Equal(fixture.mcpStatus(token), http.StatusOK)
	for _, path := range []string{"/api/v1/organizations", "/api/auth/session", "/api/auth/tokens"} {
		response := authenticationCall(fixture.runtime.http.Handler, http.MethodGet, fixture.server.URL+path, "", token)
		assert.Equal(response.Code, http.StatusUnauthorized)
	}
	request := httptest.NewRequest(http.MethodPost, "https://other.example/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	fixture.runtime.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusUnauthorized)
	digest := sha256.Sum256([]byte(token))
	fixture.runtime.authentication.oauth.mutex.Lock()
	access := fixture.runtime.authentication.oauth.tokens[digest]
	access.expires = time.Now().Add(-time.Second)
	fixture.runtime.authentication.oauth.tokens[digest] = access
	fixture.runtime.authentication.oauth.mutex.Unlock()
	assert.Equal(authenticationCall(fixture.runtime.http.Handler, http.MethodGet, fixture.server.URL+"/api/mcp", "", token).Code, http.StatusUnauthorized)
}

func (fixture oauthFixture) mcpStatus(token string) int {
	request := httptest.NewRequest(http.MethodPost, fixture.server.URL+"/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	fixture.runtime.http.Handler.ServeHTTP(response, request)
	return response.Code
}

// TestMcpOAuthAuthorizationGuards reaches each request guard using an otherwise
// valid HTTPS metadata client, so missing client configuration cannot mask it.
func TestMcpOAuthAuthorizationGuards(t *testing.T) {
	fixture := newOAuthFixture(t)
	for _, test := range []struct{ name, field, value, code string }{
		{"PKCE method", "code_challenge_method", "plain", "invalid_request"},
		{"PKCE challenge", "code_challenge", "short", "invalid_request"},
		{"resource", "resource", fixture.server.URL, "invalid_request"},
		{"response type", "response_type", "token", "invalid_request"},
		{"callback", "redirect_uri", "https://client.example/callback/other", "invalid_client"},
		{"wildcard scope", "scope", "*", "invalid_scope"},
		{"installation scope", "scope", "management:configure", "invalid_scope"},
		{"token delegation", "scope", "token:create", "invalid_scope"},
		{"duplicate scope", "scope", "organization:read organization:read", "invalid_scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			address, _ := fixture.authorization("organization:read")
			parsed, err := url.Parse(address)
			assert.NoErr(err)
			query := parsed.Query()
			query.Set(test.field, test.value)
			parsed.RawQuery = query.Encode()
			status, location, body := oauthGet(t, fixture.browser, parsed.String())
			assert.Equal(status, http.StatusBadRequest)
			assert.Equal(location, "")
			var problem map[string]string
			assert.NoErr(json.Unmarshal([]byte(body), &problem))
			assert.Equal(problem["error"], test.code)
		})
	}
	address, _ := fixture.authorization("organization:read")
	status, _, body := oauthGet(t, fixture.browser, address+"&client_id="+url.QueryEscape(fixture.clientID))
	assert := is.New(t)
	assert.Equal(status, http.StatusBadRequest)
	assert.True(strings.Contains(body, `"error":"invalid_request"`))
}

// TestMcpOAuthConsentGuards requires the displayed form, same-origin submission,
// an unchanged browser account and approval; denial never releases a code.
func TestMcpOAuthConsentGuards(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	address, _ := fixture.authorization("organization:read")
	csrf := fixture.consent(t, address)
	response, err := fixture.browser.PostForm(fixture.server.URL+"/api/auth/oauth/consent", url.Values{"decision": {"allow"}})
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusBadRequest)
	response.Body.Close()
	request, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/api/auth/oauth/consent", strings.NewReader(url.Values{"csrf": {csrf}, "decision": {"allow"}}.Encode()))
	assert.NoErr(err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://untrusted.example")
	response, err = fixture.browser.Do(request)
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusForbidden)
	response.Body.Close()
	denied := fixture.approve(t, csrf, "deny")
	assert.Equal(denied.Query().Get("error"), "access_denied")
	assert.Equal(denied.Query().Get("code"), "")
	csrf = fixture.consent(t, address)
	response, err = fixture.browser.PostForm(fixture.server.URL+"/api/auth/logout", url.Values{})
	assert.NoErr(err)
	response.Body.Close()
	response, err = fixture.browser.PostForm(fixture.server.URL+"/api/auth/oauth/consent", url.Values{"csrf": {csrf}, "decision": {"allow"}})
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusForbidden)
	response.Body.Close()
}

// TestMcpOAuthMemberRevocation keeps OAuth handles subject to the same durable
// credential revocation as user tokens, even if the old role is assigned again.
func TestMcpOAuthMemberRevocation(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	values, _ := fixture.grant(t)
	status, result := fixture.exchange(t, values)
	assert.Equal(status, http.StatusOK)
	token := result["access_token"].(string)
	assert.Equal(fixture.mcpStatus(token), http.StatusOK)
	admin := apps.WithPrincipal(t.Context(), apps.Principal{Admin: true})
	members, err := fixture.runtime.manager.ListOrganizationMembers(admin, "team")
	assert.NoErr(err)
	assert.Equal(len(members), 1)
	original := members[0]
	role, err := fixture.runtime.manager.CreateOrganizationRole(admin, "team", apps.OrganizationRoleInput{Name: "OAuth readers", Permissions: []apps.Permission{apps.PermissionOrganizationRead, apps.PermissionTokenCreate}})
	assert.NoErr(err)
	member, err := fixture.runtime.manager.PutOrganizationMember(admin, "team", original.ID, apps.OrganizationMemberInput{RoleIDs: []string{role.ID}, Revision: original.Revision})
	assert.NoErr(err)
	assert.Equal(fixture.mcpStatus(token), http.StatusUnauthorized)
	_, err = fixture.runtime.manager.PutOrganizationMember(admin, "team", original.ID, apps.OrganizationMemberInput{RoleIDs: original.RoleIDs, Revision: member.Revision})
	assert.NoErr(err)
	assert.Equal(fixture.mcpStatus(token), http.StatusUnauthorized)
}

// TestMcpOAuthRestartRequiresSignIn reopens the same durable organization state
// while proving a previous process's MCP handle cannot authorize a new server.
func TestMcpOAuthRestartRequiresSignIn(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	values, _ := fixture.grant(t)
	status, result := fixture.exchange(t, values)
	assert.Equal(status, http.StatusOK)
	token := result["access_token"].(string)
	assert.Equal(fixture.mcpStatus(token), http.StatusOK)
	fixture.server.Close()
	assert.NoErr(fixture.runtime.Close())
	reopened, err := New(t.Context(), fixture.input)
	assert.NoErr(err)
	defer reopened.Close()
	metadata := authenticationCall(reopened.http.Handler, http.MethodGet, fixture.server.URL+"/.well-known/oauth-protected-resource", "", "")
	assert.Equal(metadata.Code, http.StatusOK)
	assert.Equal(authenticationCall(reopened.http.Handler, http.MethodGet, fixture.server.URL+"/api/mcp", "", token).Code, http.StatusUnauthorized)
}

// TestMcpOAuthConsentAuthority denies bootstrap administrator sessions and
// organization sessions whose current grants cannot create the requested token.
func TestMcpOAuthConsentAuthority(t *testing.T) {
	for _, kind := range []string{"administrator", "missing token permission"} {
		t.Run(kind, func(t *testing.T) {
			assert := is.New(t)
			fixture := newOAuthFixture(t)
			address, _ := fixture.authorization("organization:read")
			fixture.consent(t, address)
			if kind == "administrator" {
				token, err := os.ReadFile(filepath.Join(fixture.input.DataDir, "admin-token"))
				assert.NoErr(err)
				response, err := fixture.browser.PostForm(fixture.server.URL+"/api/auth/login", url.Values{"token": {strings.TrimSpace(string(token))}})
				assert.NoErr(err)
				assert.Equal(response.StatusCode, http.StatusSeeOther)
				assert.Equal(response.Header.Get("Location"), "/api/auth/oauth/consent")
				response.Body.Close()
			} else {
				admin := apps.WithPrincipal(t.Context(), apps.Principal{Admin: true})
				members, err := fixture.runtime.manager.ListOrganizationMembers(admin, "team")
				assert.NoErr(err)
				role, err := fixture.runtime.manager.CreateOrganizationRole(admin, "team", apps.OrganizationRoleInput{Name: "Readers", Permissions: []apps.Permission{apps.PermissionOrganizationRead}})
				assert.NoErr(err)
				_, err = fixture.runtime.manager.PutOrganizationMember(admin, "team", members[0].ID, apps.OrganizationMemberInput{RoleIDs: []string{role.ID}, Revision: members[0].Revision})
				assert.NoErr(err)
			}
			status, location, body := oauthGet(t, fixture.browser, fixture.server.URL+"/api/auth/oauth/consent")
			assert.Equal(status, http.StatusForbidden)
			assert.Equal(location, "")
			assert.True(strings.Contains(body, `"error":"access_denied"`))
		})
	}
}

// TestMcpOAuthPendingAdmission keeps unsigned requests from locking out new
// sign-ins while preserving a signed-in user's already displayed consent.
func TestMcpOAuthPendingAdmission(t *testing.T) {
	assert := is.New(t)
	fixture := newOAuthFixture(t)
	address, _ := fixture.authorization("organization:read")
	csrf := fixture.consent(t, address)
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for range 65 {
		status, _, _ := oauthGet(t, client, address)
		assert.Equal(status, http.StatusSeeOther)
	}
	redirect := fixture.approve(t, csrf, "allow")
	assert.True(redirect.Query().Get("code") != "")
}
