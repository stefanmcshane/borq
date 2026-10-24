package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestClientSettingsCreatesPrivateYaml verifies first use creates a portable
// config directory and usable defaults without requiring a server connection.
func TestClientSettingsCreatesPrivateYaml(t *testing.T) {
	assert := is.New(t)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	output, _, err := runConfiguredApplicationCommand("config", "show")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "space: default"))
	assert.True(strings.HasPrefix(output, "default:\n"))
	profiles, _, err := runConfiguredApplicationCommand("config", "list")
	assert.NoErr(err)
	assert.True(strings.Contains(profiles, "default"))
	current, _, err := runConfiguredApplicationCommand("config", "current")
	assert.NoErr(err)
	assert.Equal(current, "default\n")
	_, _, err = runConfiguredApplicationCommand("config", "use", "default")
	assert.NoErr(err)
	settings, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(path, filepath.Join(base, "borq", "config.yaml"))
	assert.Equal(settings, clientSettings{Server: "http://127.0.0.1:7833", Space: "default"})
	file, err := os.Stat(path)
	assert.NoErr(err)
	assert.Equal(file.Mode().Perm(), os.FileMode(0o600))
	directory, err := os.Stat(filepath.Dir(path))
	assert.NoErr(err)
	assert.Equal(directory.Mode().Perm(), os.FileMode(0o700))
	_, _, err = runConfiguredApplicationCommand("cluster", "list")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "borq config set --organization"))
}

// TestClientApiUrlAliasUsesSavedDefaultsAndTemporaryOverrides keeps the primary
// flag compatible with server-keyed YAML and the existing --server alias.
func TestClientApiUrlAliasUsesSavedDefaultsAndTemporaryOverrides(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var savedRequests, overrideRequests atomic.Int32
	saved := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		savedRequests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("[]"))
	}))
	defer saved.Close()
	override := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		overrideRequests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("[]"))
	}))
	defer override.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--api-url", saved.URL)
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"org", "list"},
		{"org", "list", "--api-url", override.URL},
		{"org", "list", "--server", override.URL},
		{"org", "list", "--api-url", override.URL, "--server", override.URL + "/"},
		{"org", "list"},
	} {
		_, _, err = runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	assert.Equal(savedRequests.Load(), int32(2))
	assert.Equal(overrideRequests.Load(), int32(3))
	settings, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(settings.Server, saved.URL)
	data, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.True(strings.Contains(string(data), "server:"))
	assert.True(!strings.Contains(string(data), "api-url:"))
	output, _, err := runConfiguredApplicationCommand("org", "list", "--help")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "--api-url"))
	assert.True(!strings.Contains(output, "--server"))
}

// TestConflictingApiUrlAliasesFailBeforeRequests prevents argument ordering from
// silently selecting another server or changing the saved API selection.
func TestConflictingApiUrlAliasesFailBeforeRequests(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("[]"))
	}))
	defer server.Close()
	settings, _, err := loadClientSettings()
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"org", "list", "--api-url", server.URL, "--server", "http://unused.invalid"},
		{"org", "list", "--server", "http://unused.invalid", "--api-url", server.URL},
		{"config", "set", "--api-url", server.URL, "--server", "http://unused.invalid"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "different API URLs"))
	}
	assert.Equal(requests.Load(), int32(0))
	unchanged, _, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(unchanged, settings)
}

// TestClientDefaultsRouteCommandsAndPreserveSavedPlans checks defaults are
// shared by discovery, environment groups, plan and apply, while flags remain temporary.
func TestClientDefaultsRouteCommandsAndPreserveSavedPlans(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/v1/operations/saved-plan":
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "operation not found"})
		case strings.HasSuffix(request.URL.Path, "/plans"):
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "saved-plan", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api", Image: "example/api:v1", Replicas: 1}}}})
		case request.URL.Path == "/api/v1/plans/saved-plan/apply":
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "saved-operation", PlanID: "saved-plan", Status: appapi.Pending})
		default:
			_, _ = writer.Write([]byte("[]"))
		}
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--server", server.URL, "--organization", "team", "--space", "dev")
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "apps.yaml")
	assert.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  image: example/api:v1\n"), 0o600))
	for _, arguments := range [][]string{
		{"org", "list"}, {"cluster", "list"}, {"space", "list"}, {"environment-group", "list"},
		{"plan", "-f", file}, {"apply", "-f", file},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	assert.Equal(requests, []string{
		"/api/v1/organizations", "/api/v1/organizations/team/clusters", "/api/v1/organizations/team/spaces",
		"/api/v1/organizations/team/spaces/dev/environment-groups", "/api/v1/organizations/team/spaces/dev/plans",
		"/api/v1/operations/saved-plan", "/api/v1/plans/saved-plan/apply",
	})
	override := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/other/spaces/preview/environment-groups")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("[]"))
	}))
	defer override.Close()
	_, _, err = runConfiguredApplicationCommand("environment-group", "list", "--server", override.URL, "--organization", "other", "--space", "preview")
	assert.NoErr(err)
	settings, _, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(settings, clientSettings{Server: server.URL, Organization: "team", Space: "dev"})
	_, _, err = runConfiguredApplicationCommand("config", "set", "--space", "staging")
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("apply", "-f", file, "--plan-file", file+".borq-plan.json")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "changed since planning"))
	assert.Equal(len(requests), 7)
}

// TestClientSettingsUpdateAndValidate prevents partial changes and invalid URLs
// from silently discarding the existing selection or breaking later commands.
func TestClientSettingsUpdateAndValidate(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, err := runConfiguredApplicationCommand("config", "set", "--organization", "team", "--space", "production")
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("config", "set", "--organization", "another")
	assert.NoErr(err)
	settings, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(settings.Space, "default")
	assert.Equal(settings.Organization, "another")
	for _, arguments := range [][]string{
		{"config", "set"}, {"config", "set", "--server", "not-a-url"}, {"config", "set", "--server", "ftp://example.com"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.True(err != nil)
		unchanged, _, err := loadClientSettings()
		assert.NoErr(err)
		assert.Equal(unchanged, settings)
	}
	// First-use initialization must never replace a file another process saved.
	assert.NoErr(saveClientSettings(path, clientSettings{Server: "https://unused.example", Space: "unused"}, true))
	unchanged, _, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(unchanged, settings)
	_, _, err = runConfiguredApplicationCommand("config", "set", "--organization=", "--space=")
	assert.NoErr(err)
	cleared, _, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(cleared.Organization, "")
	assert.Equal(cleared.Space, "")
}

// TestInvalidClientSettingsFailBeforeRequests makes malformed or misspelled
// user settings actionable instead of silently choosing a deployment target.
func TestInvalidClientSettingsFailBeforeRequests(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, path, err := loadClientSettings()
	assert.NoErr(err)
	for _, source := range []string{"space: [invalid\n", "organisation: wrong\n", "server: invalid\n", "space: first\nspace: second\n"} {
		assert.NoErr(os.WriteFile(path, []byte(source), 0o600))
		_, _, err := runConfiguredApplicationCommand("org", "list", "--server", "http://127.0.0.1:1")
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), path))
		unchanged, err := os.ReadFile(path)
		assert.NoErr(err)
		assert.Equal(string(unchanged), source)
	}
}
