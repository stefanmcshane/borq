package appserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestOidcAdministratorWithoutEmailVerification exercises real signed tokens with
// no email_verified claim, management apply, browser login and provider revocation.
func TestOidcAdministratorWithoutEmailVerification(t *testing.T) {
	assert := is.New(t)
	provider, kubeconfig, _ := oidcProviderFixture(t, false)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory, ManagementKubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer server.Close()
	base := "https://borq.example.test"
	_, err = server.manager.OnboardOrganization(t.Context(), "team", apps.OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &apps.OrganizationOidcInput{IssuerURL: provider.URL, BaseURL: base, ClientID: "borq-client", ClientSecret: "private-client-secret"}})
	assert.NoErr(err)
	login := func(testSignIn bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, base+"/api/auth/oidc/login", strings.NewReader(url.Values{"organization": {"team"}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if testSignIn {
			request = httptest.NewRequest(http.MethodPost, base+"/api/auth/oidc/test/team", nil)
			authorizeAdminTestRequest(t, directory, request)
		}
		start := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(start, request)
		assert.Equal(start.Code, http.StatusSeeOther)
		client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		authorization, err := client.Get(start.Header().Get("Location"))
		assert.NoErr(err)
		defer authorization.Body.Close()
		return authenticationCall(server.http.Handler, http.MethodGet, authorization.Header.Get("Location"), "", "", start.Result().Cookies()[0])
	}
	denied := login(false)
	assert.Equal(denied.Code, http.StatusForbidden)
	assert.True(strings.Contains(denied.Body.String(), "did not verify"))
	proof := login(true)
	assert.Equal(proof.Code, http.StatusOK)
	assert.True(strings.Contains(proof.Body.String(), "account-123"))
	settings, err := server.manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	configuration := apps.ManagementConfiguration{Organizations: []apps.ManagementOrganization{{Name: "team", Oidc: &apps.ManagementOidc{IssuerURL: provider.URL, ClientID: "borq-client", BaseURL: base, AdminEmail: "owner@example.test", AdminSubject: "account-123"}}}}
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	request := httptest.NewRequest(http.MethodPut, base+"/api/v1/management/configuration", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	authorizeAdminTestRequest(t, directory, request)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusAccepted)
	var operation apps.Operation
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &operation))
	deadline := time.Now().Add(10 * time.Second)
	for operation.Status != "succeeded" && time.Now().Before(deadline) {
		operation, err = server.manager.GetManagementOperation(t.Context(), operation.ID)
		assert.NoErr(err)
		if operation.Status == "failed" {
			t.Fatalf("management apply failed: %s", operation.Message)
		}
		time.Sleep(time.Millisecond)
	}
	assert.Equal(operation.Status, "succeeded")
	response = login(false)
	assert.Equal(response.Code, http.StatusSeeOther)
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "borq-session" {
			session = cookie
		}
	}
	assert.True(session != nil)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/account", "", "", session).Code, http.StatusOK)
	_, err = server.manager.PutOrganizationOidc(t.Context(), "team", apps.OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: provider.URL, BaseURL: base, ClientID: "borq-client", Revision: settings.Revision})
	assert.NoErr(err)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/account", "", "", session).Code, http.StatusSeeOther)
}
