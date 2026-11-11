package appserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestOrganizationOidcPublicPrivateOrigins proves both registered origins finish
// a signed code exchange while browser state cannot cross hosts or trust proxy headers.
func TestOrganizationOidcPublicPrivateOrigins(t *testing.T) {
	assert := is.New(t)
	provider, kubeconfig, exchanges := oidcProviderFixture(t, true)
	directory := t.TempDir()
	public, private := "https://borq.example.test", "https://borq-default.pheasant-rainbow.ts.net"
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory, ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, ManagementURL: private}}})
	assert.NoErr(err)
	defer server.Close()
	_, err = server.manager.OnboardOrganization(t.Context(), "team", apps.OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &apps.OrganizationOidcInput{IssuerURL: provider.URL, BaseURL: public, ClientID: "borq-client", ClientSecret: "private-client-secret"}})
	assert.NoErr(err)
	client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, origin := range []string{public, private} {
		for _, testSignIn := range []bool{false, true} {
			path, body := "/api/auth/oidc/login", url.Values{"organization": {"team"}}.Encode()
			if testSignIn {
				path, body = "/api/auth/oidc/test/team", ""
			}
			request := httptest.NewRequest(http.MethodPost, origin+path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if testSignIn {
				authorizeAdminTestRequest(t, directory, request)
			}
			start := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(start, request)
			if start.Code != http.StatusSeeOther {
				t.Fatalf("%s test=%t: status %d: %s", origin, testSignIn, start.Code, start.Body.String())
			}
			authorize, err := url.Parse(start.Header().Get("Location"))
			assert.NoErr(err)
			assert.True(strings.HasPrefix(authorize.Query().Get("redirect_uri"), origin+"/api/auth/oidc/callback/"))
			cookie := start.Result().Cookies()[0]
			assert.True(cookie.Secure && cookie.HttpOnly && cookie.Domain == "")
			response, err := client.Get(authorize.String())
			assert.NoErr(err)
			response.Body.Close()
			callback := response.Header.Get("Location")
			before := exchanges.Load()
			foreign := strings.Replace(callback, origin, "https://unconfigured.example.test", 1)
			denied := authenticationCall(server.http.Handler, http.MethodGet, foreign, "", "", cookie)
			assert.Equal(denied.Code, http.StatusBadRequest)
			assert.Equal(exchanges.Load(), before)
			completed := authenticationCall(server.http.Handler, http.MethodGet, callback, "", "", cookie)
			if testSignIn {
				assert.Equal(completed.Code, http.StatusOK)
			} else {
				assert.Equal(completed.Code, http.StatusSeeOther)
				var session *http.Cookie
				for _, candidate := range completed.Result().Cookies() {
					if candidate.Name == "borq-session" {
						session = candidate
					}
				}
				assert.True(session != nil)
				assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, origin+"/account", "", "", session).Code, http.StatusOK)
			}
		}
	}
	assert.Equal(exchanges.Load(), int32(4))
	for _, origin := range []string{"https://unconfigured.example.test", private + ":444"} {
		request := httptest.NewRequest(http.MethodPost, origin+"/api/auth/oidc/login", strings.NewReader("organization=team"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("X-Forwarded-Host", strings.TrimPrefix(private, "https://"))
		denied := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(denied, request)
		assert.Equal(denied.Code, http.StatusForbidden)
	}
}
