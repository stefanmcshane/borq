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
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

// TestClientProfilesInheritBaseAndKeepSelectionSeparate checks partial presets,
// active and temporary selection, private storage, and edits to inherited defaults.
func TestClientProfilesInheritBaseAndKeepSelectionSeparate(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, arguments := range [][]string{
		{"config", "set", "--api-url", "https://base.example.test", "--organization", "team", "--space", "production", "--cluster", "primary"},
		{"config", "set", "--profile", "dev", "--api-url", "https://dev.example.test"},
		{"config", "set", "--profile", "prod", "--space", "release"},
		{"config", "use", "dev"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	base, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(base.Server, "https://base.example.test")
	assert.Equal(base.Space, "production")
	assert.Equal(base.CurrentProfile, "dev")
	profile, err := readClientProfile(path, "dev")
	assert.NoErr(err)
	assert.True(profile.Server != nil)
	assert.True(profile.Organization == nil && profile.Space == nil && profile.Cluster == nil)
	profilePath, err := clientProfilePath(path, "dev")
	assert.NoErr(err)
	file, err := os.Stat(profilePath)
	assert.NoErr(err)
	assert.Equal(file.Mode().Perm(), os.FileMode(0600))
	directory, err := os.Stat(filepath.Dir(profilePath))
	assert.NoErr(err)
	assert.Equal(directory.Mode().Perm(), os.FileMode(0700))
	selected := readSelectedProfile(t)
	assert.Equal(selected.Server, "https://dev.example.test")
	assert.Equal(selected.Organization, "team")
	assert.Equal(selected.Space, "production")
	assert.Equal(selected.Cluster, "primary")
	temporary := readSelectedProfile(t, "--profile", "prod")
	assert.Equal(temporary.Server, base.Server)
	assert.Equal(temporary.Space, "release")
	assert.Equal(temporary.CurrentProfile, "prod")
	assert.Equal(readSelectedProfile(t).CurrentProfile, "dev")
	_, _, err = runConfiguredApplicationCommand("config", "set", "--profile=", "--space", "qa")
	assert.NoErr(err)
	assert.Equal(readSelectedProfile(t).Space, "qa")
	_, _, err = runConfiguredApplicationCommand("config", "set", "--space", "active-space")
	assert.NoErr(err)
	assert.Equal(readSelectedProfile(t).Space, "active-space")
	assert.Equal(readSelectedProfile(t, "--profile=").Space, "qa")
	_, _, err = runConfiguredApplicationCommand("config", "list")
	assert.NoErr(err)
	profiles, err := listClientProfiles(path)
	assert.NoErr(err)
	assert.Equal(profiles, []string{"default", "dev", "prod"})
	_, _, err = runConfiguredApplicationCommand("config", "use", "")
	assert.NoErr(err)
	assert.Equal(readSelectedProfile(t).CurrentProfile, "")
	assert.Equal(readSelectedProfile(t).Server, base.Server)
}

// TestClientProfileEmptyOverridesAndOrganizationSafety keeps intentional clears,
// command overrides, and resets dependent selections when changing a saved organization.
func TestClientProfileEmptyOverridesAndOrganizationSafety(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, arguments := range [][]string{
		{"config", "set", "--organization", "team", "--space", "production", "--cluster", "prod-cluster"},
		{"config", "set", "--profile", "work", "--space", "review", "--cluster", "review-cluster"},
		{"config", "use", "work"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	command := &cobra.Command{}
	command.Flags().String("organization", "", "")
	command.Flags().String("space", "", "")
	command.Flags().String("cluster", "", "")
	assert.NoErr(command.ParseFlags([]string{"--organization", "other"}))
	selected, err := selectedClientSettings(command)
	assert.NoErr(err)
	assert.Equal(selected.Organization, "other")
	assert.Equal(selected.Space, "default")
	assert.Equal(selected.Cluster, "review-cluster")
	_, _, err = runConfiguredApplicationCommand("config", "set", "--organization", "other")
	assert.NoErr(err)
	selected = readSelectedProfile(t)
	assert.Equal(selected.Space, "default")
	assert.Equal(selected.Cluster, "")
	base, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(base.Organization, "team")
	assert.Equal(base.Cluster, "prod-cluster")
	_, _, err = runConfiguredApplicationCommand("config", "set", "--organization=", "--space=", "--cluster=")
	assert.NoErr(err)
	selected = readSelectedProfile(t)
	assert.Equal(selected.Organization, "")
	assert.Equal(selected.Space, "")
	assert.Equal(selected.Cluster, "")
	profile, err := readClientProfile(path, "work")
	assert.NoErr(err)
	assert.True(profile.Organization != nil && *profile.Organization == "")
	assert.True(profile.Space != nil && *profile.Space == "")
	assert.True(profile.Cluster != nil && *profile.Cluster == "")
	_, _, err = runConfiguredApplicationCommand("config", "set", "--organization", "new", "--space", "custom", "--cluster", "custom-cluster")
	assert.NoErr(err)
	selected = readSelectedProfile(t)
	assert.Equal(selected.Space, "custom")
	assert.Equal(selected.Cluster, "custom-cluster")
}

// TestClientProfilesRouteAndProtectSavedPlans uses real CLI request conversion
// to prove profile changes cannot silently apply a plan reviewed in another space.
func TestClientProfilesRouteAndProtectSavedPlans(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var plans, applies atomic.Int32
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/v1/operations/profile-plan":
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "operation not found"})
		case strings.HasSuffix(request.URL.Path, "/plans"):
			plans.Add(1)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "profile-plan"})
		case strings.HasSuffix(request.URL.Path, "/apply"):
			applies.Add(1)
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "profile-run", Status: appapi.Pending})
		default:
			_, _ = writer.Write([]byte("[]"))
		}
	}))
	defer server.Close()
	for _, arguments := range [][]string{
		{"config", "set", "--api-url", server.URL, "--organization", "team"},
		{"config", "set", "--profile", "dev", "--space", "review"},
		{"config", "set", "--profile", "prod", "--space", "production"},
		{"config", "use", "dev"},
		{"environment-group", "list", "--organization", "other"},
		{"environment-group", "list", "--organization", "other", "--space", "preview"},
		{"addons", "custom", "list", "--profile", "prod"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	assert.Equal(paths, []string{"/api/v1/organizations/other/spaces/default/environment-groups", "/api/v1/organizations/other/spaces/preview/environment-groups", "/api/v1/organizations/team/spaces/production/custom-charts"})
	file := filepath.Join(t.TempDir(), "apps.yaml")
	assert.NoErr(os.WriteFile(file, []byte("apps: []\n"), 0600))
	_, _, err := runConfiguredApplicationCommand("plan", "-f", file)
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("apply", "--profile", "prod", "-f", file, "--plan-file", file+".borq-plan.json")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "changed since planning"))
	assert.Equal(plans.Load(), int32(1))
	assert.Equal(applies.Load(), int32(0))
	_, _, err = runConfiguredApplicationCommand("apply", "--profile", "prod", "--space", "review", "-f", file)
	assert.NoErr(err)
	assert.Equal(applies.Load(), int32(1))
}

// TestUnknownOrInvalidProfilesFailBeforeRequests makes missing presets and unsafe
// filenames actionable while preserving the active selection and private files.
func TestUnknownOrInvalidProfilesFailBeforeRequests(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte("[]"))
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("org", "list", "--api-url", server.URL, "--profile", "missing")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "borq config set --profile missing"))
	assert.Equal(requests.Load(), int32(0))
	for _, name := range []string{"../outside", "with/slash", ".", "Invalid", ""} {
		_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", name)
		assert.True(err != nil)
	}
	_, _, err = runConfiguredApplicationCommand("config", "set", "--profile", "valid")
	assert.NoErr(err)
	_, path, err := loadClientSettings()
	assert.NoErr(err)
	profilePath, err := clientProfilePath(path, "valid")
	assert.NoErr(err)
	before, err := os.ReadFile(profilePath)
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("config", "set", "--profile", "valid", "--api-url", "invalid")
	assert.True(err != nil)
	after, err := os.ReadFile(profilePath)
	assert.NoErr(err)
	assert.Equal(after, before)
}

func readSelectedProfile(t *testing.T, flags ...string) clientSettings {
	t.Helper()
	output, _, err := runConfiguredApplicationCommand(append([]string{"config", "show"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	var profiles map[string]clientSettings
	if err := yaml.Unmarshal([]byte(output), &profiles); err != nil {
		t.Fatal(err)
	}
	for name, settings := range profiles {
		if name != "default" {
			settings.CurrentProfile = name
		}
		return settings
	}
	t.Fatal("config show returned no profile")
	return clientSettings{}
}
