package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// TestAppsListUsesProfileAndFormats prints reusable configuration or a Kubernetes
// document stream while explicit organization and space flags override profile defaults.
func TestAppsListUsesProfileAndFormats(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := appapi.ApplicationState{
		Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api", Image: "example/api:v1", Replicas: 1, Env: appapi.EnvReference{Name: "settings", Version: "3"}}}},
		Manifests: []appapi.Manifest{
			{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "api"}},
			{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "api"}},
		},
	}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		requests = append(requests, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(state)
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization", "team", "--space", "dev")
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("config", "use", "work")
	assert.NoErr(err)
	output, _, err := runConfiguredApplicationCommand("apps", "list")
	assert.NoErr(err)
	var configuration appapi.Configuration
	assert.NoErr(yaml.Unmarshal([]byte(output), &configuration))
	assert.Equal(configuration, state.Configuration)
	assert.True(!strings.Contains(output, "kind: Deployment"))
	output, _, err = runConfiguredApplicationCommand("apps", "list", "--manifest", "--organization", "other", "--space", "preview")
	assert.NoErr(err)
	decoder := yamlreader.NewDecoder(strings.NewReader(output))
	for _, expected := range state.Manifests {
		var manifest appapi.Manifest
		assert.NoErr(decoder.Decode(&manifest))
		actualJson, err := json.Marshal(manifest)
		assert.NoErr(err)
		expectedJson, err := json.Marshal(expected)
		assert.NoErr(err)
		assert.Equal(string(actualJson), string(expectedJson))
	}
	var extra appapi.Manifest
	assert.Equal(decoder.Decode(&extra), io.EOF)
	assert.True(!strings.Contains(output, "apps:"))
	assert.Equal(requests, []string{"/api/v1/organizations/team/spaces/dev/apps", "/api/v1/organizations/other/spaces/preview/apps"})
	settings := readSelectedProfile(t)
	assert.Equal(settings.Organization, "team")
	assert.Equal(settings.Space, "dev")
}

// TestAppsListEmptyAndMissing distinguishes empty spaces from missing or inaccessible targets.
func TestAppsListEmptyAndMissing(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "/missing/") {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "space not found"})
			return
		}
		_ = json.NewEncoder(writer).Encode(appapi.ApplicationState{Configuration: appapi.Configuration{Apps: []appapi.App{}}, Manifests: []appapi.Manifest{}})
	}))
	defer server.Close()
	flags := []string{"--api-url", server.URL, "--organization", "team", "--space", "dev"}
	output, _, err := runApplicationCommand(t, append([]string{"apps", "list"}, flags...)...)
	assert.NoErr(err)
	assert.Equal(output, "apps: []\n")
	output, _, err = runApplicationCommand(t, append([]string{"apps", "list", "--manifest"}, flags...)...)
	assert.NoErr(err)
	assert.Equal(output, "")
	_, _, err = runApplicationCommand(t, "apps", "list", "--api-url", server.URL, "--organization", "team", "--space", "missing")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "space not found"))
	_, _, err = runApplicationCommand(t, "apps", "list")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "organization"))
}
