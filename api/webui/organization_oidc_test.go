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

// TestOrganizationOidcForm owns form conversion and safe page responses; storage is covered by manager tests.
func TestOrganizationOidcForm(t *testing.T) {
	assert := is.New(t)
	settings := appapi.OrganizationOidc{}
	var submitted appapi.OrganizationOidcInput
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team", ID: "team-id"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/organizations/team/pocketid":
			output = appapi.OrganizationPocketID{}
		case "/api/v1/organizations/team/oidc":
			if request.Method == http.MethodPut {
				assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
				settings = appapi.OrganizationOidc{AdminEmail: *submitted.AdminEmail, IssuerURL: submitted.IssuerURL, ClientID: submitted.ClientID, BaseURL: submitted.BaseURL, CallbackURL: "https://borq.test/api/auth/oidc/callback/provider", ClientSecretConfigured: true, Revision: 1}
			}
			output = settings
		default:
			t.Errorf("unexpected API request: %s", request.URL.Path)
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	form := url.Values{"action": {"put-organization-oidc"}, "organization": {"team"}, "view": {"organization-authentication"}, "revision": {"0"}, "adminEmail": {" owner@example.test "}, "issuerUrl": {" https://id.test "}, "clientId": {" client "}, "borqPublicUrl": {" https://borq.test "}, "clientSecret": {"  private-secret  "}}
	request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true}))
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(submitted.IssuerURL, "https://id.test")
	assert.Equal(submitted.ClientID, "client")
	assert.True(submitted.AdminEmail != nil)
	assert.Equal(*submitted.AdminEmail, "owner@example.test")
	assert.Equal(submitted.BaseURL, "https://borq.test")
	assert.True(submitted.ClientSecret != nil)
	assert.Equal(*submitted.ClientSecret, "  private-secret  ")
	assert.Equal(page.Location, "/?organization=team&view=organization-authentication")
	assert.Equal(page.Oidc, settings)
	assert.True(strings.Contains(page.Notice, "open the sign-in page for normal access"))
	assert.True(!strings.Contains(response.Body.String(), "private-secret"))
	form.Set("clientSecret", "")
	form.Set("revision", "1")
	request = httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true}))
	response = httptest.NewRecorder()
	// Reset the decoded fixture so an omitted field is not mistaken for an earlier input.
	submitted = appapi.OrganizationOidcInput{}
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(submitted.ClientSecret == nil)
	assert.True(submitted.AdminEmail != nil)
	assert.Equal(*submitted.AdminEmail, "owner@example.test")
}
