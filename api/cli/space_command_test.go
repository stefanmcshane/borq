package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestSpaceCreateFile owns schema-backed space input and explicit flag precedence over the file and profile defaults.
func TestSpaceCreateFile(t *testing.T) {
	assert := is.New(t)
	wantedCluster := "file-cluster"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/preview")
		var input appapi.SpaceInput
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.Equal(input.Cluster, wantedCluster)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"preview"}`))
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "space.yaml")
	assert.NoErr(os.WriteFile(file, []byte("cluster: file-cluster\n"), 0o600))
	arguments := []string{"space", "create", "preview", "--organization", "team", "--server", server.URL, "--file", file}
	_, _, err := runApplicationCommand(t, arguments...)
	assert.NoErr(err)
	wantedCluster = "flag-cluster"
	_, _, err = runApplicationCommand(t, append(arguments, "--cluster", wantedCluster)...)
	assert.NoErr(err)
}

// TestSpaceCommandsUseProfileDefaults verifies organization-scoped listings,
// explicit new names, and independent organization and cluster flag overrides.
func TestSpaceCommandsUseProfileDefaults(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var requests, clusters []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`[{"name":"existing"}]`))
			return
		}
		var input appapi.SpaceInput
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.True(input.Cluster != "")
		clusters = append(clusters, input.Cluster)
		_, _ = writer.Write([]byte(`{"name":"new"}`))
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization", "team", "--cluster", "local", "--space", "existing")
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("config", "use", "work")
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"space", "list"},
		{"space", "list", "--organization", "other"},
		{"space", "create", "new"},
		{"space", "create", "new", "--cluster", "target"},
		{"space", "create", "new", "--organization", "other"},
		{"space", "create", "new", "--organization", "other", "--cluster", "target"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	assert.Equal(requests, []string{
		"GET /api/v1/organizations/team/spaces", "GET /api/v1/organizations/other/spaces",
		"PUT /api/v1/organizations/team/spaces/new", "PUT /api/v1/organizations/team/spaces/new",
		"PUT /api/v1/organizations/other/spaces/new", "PUT /api/v1/organizations/other/spaces/new",
	})
	assert.Equal(clusters, []string{"local", "target", "local", "target"})
	for _, arguments := range [][]string{{"space", "create"}, {"space", "create", ""}, {"space", "create", "new", "--cluster="}, {"space", "create", "new", "--organization="}, {"space", "create", "new", "--namespace", "custom"}, {"space", "create", "new", "--certificate-issuer", "custom"}} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.True(err != nil)
	}
	assert.Equal(len(requests), 6)
	settings := readSelectedProfile(t)
	assert.Equal(settings.Organization, "team")
	assert.Equal(settings.Cluster, "local")
	assert.Equal(settings.Space, "existing")
}
