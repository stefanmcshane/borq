package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
)

// TestProfileClusterTargets scopes component inspection to the selected
// organization and cluster, while an explicit cluster argument takes priority.
func TestProfileClusterTargets(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"clusterId":"target","status":"ready"}`))
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization", "team", "--cluster", "staging")
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"cluster", "components", "--profile", "work"},
		{"cluster", "components", "production", "--profile", "work"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	assert.Equal(paths, []string{
		"/api/v1/organizations/team/clusters/staging/components",
		"/api/v1/organizations/team/clusters/production/components",
	})
}
