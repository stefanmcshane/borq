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
)

// TestRegistrySettingsActions owns browser-to-API scope routing and ensures only
// the submitted registry credentials are changed, without accepting vault references.
func TestRegistrySettingsActions(t *testing.T) {
	for _, scope := range []string{"space", "management"} {
		t.Run(scope, func(t *testing.T) {
			assert := is.New(t)
			calls := 0
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				assert.Equal(request.Method, http.MethodPut)
				var body map[string]json.RawMessage
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				assert.Equal(len(body), 1)
				var registries appapi.Registries
				assert.NoErr(json.Unmarshal(body["registries"], &registries))
				assert.Equal(*registries["ghcr.io"].Username, "github-user")
				assert.Equal(*registries["ghcr.io"].Token, "private-test-token")
				assert.True(registries["ghcr.io"].CredentialsRef == nil)
				path := "/api/v1/management/configuration"
				if scope == "space" {
					path = "/api/v1/organizations/team/spaces/production/configuration"
				}
				assert.Equal(request.URL.Path, path)
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "registry-change", Status: "queued"})
			})}
			request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
			request.PostForm = url.Values{"action": {"put-" + scope + "-registry"}, "organization": {"team"}, "space": {"production"}, "githubUsername": {" github-user "}, "githubToken": {"private-test-token"}, "credentialsRef": {"another-spaces-reference"}}
			result, err := web.execute(request)
			assert.NoErr(err)
			assert.Equal(calls, 1)
			assert.Equal(result.selection.Get("managementOperation"), "registry-change")
			assert.True(!strings.Contains(result.selection.Encode()+result.notice, "private-test-token"))
			if scope == "space" {
				assert.Equal(result.selection.Get("view"), "secrets")
				assert.Equal(result.selection.Get("organization"), "team")
				assert.Equal(result.selection.Get("space"), "production")
			} else {
				assert.Equal(result.selection.Get("view"), "infrastructure")
				assert.Equal(result.selection.Get("node"), "management")
				assert.Equal(result.selection.Get("tab"), "settings")
				assert.Equal(result.selection.Get("space"), "")
			}
		})
	}
}

// TestRegistrySettingsInvalidInput rejects partial credentials or missing space
// scope before calling the API, and never includes submitted secrets in errors.
func TestRegistrySettingsInvalidInput(t *testing.T) {
	for _, change := range []func(url.Values){
		func(form url.Values) { form.Del("githubUsername") },
		func(form url.Values) { form.Del("githubToken") },
		func(form url.Values) { form.Del("organization") },
		func(form url.Values) { form.Del("space") },
	} {
		assert := is.New(t)
		web := handler{api: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid registry form reached the API") })}
		request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
		request.PostForm = url.Values{"action": {"put-space-registry"}, "organization": {"team"}, "space": {"production"}, "githubUsername": {"github-user"}, "githubToken": {"never-echo-this-token"}}
		change(request.PostForm)
		_, err := web.execute(request)
		assert.True(err != nil)
		problem, ok := err.(apiProblem)
		assert.True(ok)
		assert.Equal(problem.status, http.StatusBadRequest)
		assert.True(!strings.Contains(problem.message, "never-echo"))
	}
}

// TestManagementRegistrySettingsReadback exposes only configuration status,
// even if an API response accidentally contains inline credentials.
func TestManagementRegistrySettingsReadback(t *testing.T) {
	assert := is.New(t)
	reference, username, token := "saved-vault-reference", "private-user", "never-render-this-token"
	registries := &appapi.Registries{"ghcr.io": {CredentialsRef: &reference, Username: &username, Token: &token}}
	organizations := []appapi.ManagementOrganization{
		{Name: "other", Spaces: &[]appapi.ManagementSpace{{Name: "production", Registries: registries}}},
		{Name: "team", Spaces: &[]appapi.ManagementSpace{{Name: "production", Registries: registries}, {Name: "development"}}},
	}
	web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		if request.URL.Path == "/api/v1/management/s3" {
			_ = json.NewEncoder(writer).Encode(appapi.ManagementS3{})
			return
		}
		assert.Equal(request.URL.Path, "/api/v1/management/configuration")
		_ = json.NewEncoder(writer).Encode(appapi.ManagementConfiguration{Registries: registries, Organizations: &organizations})
	})}
	management, err := web.loadManagementSettings(httptest.NewRequest(http.MethodGet, "/", nil), pageData{Infrastructure: infrastructureSelection{Node: managementNode, Tab: "settings"}})
	assert.NoErr(err)
	assert.True(management.Registry.Configured)
	encoded, err := json.Marshal(management)
	assert.NoErr(err)
	for _, value := range []string{reference, username, token} {
		assert.True(!strings.Contains(string(encoded), value))
	}
}
