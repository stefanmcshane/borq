package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestManagementBootstrapHonorsExternalKubeconfig keeps an explicit external
// connection from falling back to local provisioning or the current kubeconfig.
func TestManagementBootstrapHonorsExternalKubeconfig(t *testing.T) {
	assert := is.New(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, err := runApplicationCommand(t, "management", "bootstrap", "local", "--kubeconfig", "/missing/management-kubeconfig")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "kubeconfig"))
	assert.True(strings.Contains(err.Error(), "/missing/management-kubeconfig"))
}

// TestManagementBootstrapRejectsConflictingNames catches ambiguous local cluster
// selection before reading credentials or changing any Kubernetes resources.
func TestManagementBootstrapRejectsConflictingNames(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, err := runApplicationCommand(t, "management", "bootstrap", "local", "--cluster-name", "other")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "cluster argument and --cluster-name differ"))
}

// TestManagementBootstrapApiUrlKeepsAdvancedEndpointsHidden exposes one primary
// API address without requiring a configured or already-running Borq server.
func TestManagementBootstrapApiUrlKeepsAdvancedEndpointsHidden(t *testing.T) {
	assert := is.New(t)
	command := newManagementBootstrapCommand(context.Background(), logger.Logger{}, Config{})
	primary := command.Flags().Lookup("api-url")
	assert.True(primary != nil)
	assert.Equal(primary.DefValue, "http://localhost:31833")
	assert.True(!primary.Hidden)
	assert.Equal(command.Flags().Lookup("server-image").DefValue, "ghcr.io/stefanmcshane/borq:latest")
	for _, name := range []string{"probe-url", "management-openbao-url", "management-telemetry-url", "management-loki-url", "management-prometheus-url"} {
		assert.True(command.Flags().Lookup(name).Hidden)
	}
	output, _, err := runApplicationCommand(t, "management", "bootstrap", "--help")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "--api-url"))
	assert.True(!strings.Contains(output, "--probe-url"))
	assert.True(!strings.Contains(output, "--management-openbao-url"))
	assert.True(!strings.Contains(output, "--management-telemetry-url"))
}

// TestManagementUpgradeUsesSelectedServer proves management lifecycle changes
// run through Borq's API instead of installing charts from the client machine.
func TestManagementUpgradeUsesSelectedServer(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/management/components/upgrade")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"clusterId":"management","status":"ready"}`))
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--server", server.URL)
	assert.NoErr(err)
	output, _, err := runConfiguredApplicationCommand("management", "upgrade")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "status: ready"))
}

// TestClusterCreationCommandsAreUnavailable keeps Kubernetes installation outside
// Borq while bootstrap and linking remain explicit, supported commands.
func TestClusterCreationCommandsAreUnavailable(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, arguments := range [][]string{{"cluster", "create", "target"}, {"management", "create-cluster", "management"}} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "unknown command"))
	}
	bootstrap := newManagementBootstrapCommand(context.Background(), logger.Logger{}, Config{KubeconfigPath: "/existing/k3s.yaml"})
	assert.Equal(bootstrap.Flags().Lookup("kubeconfig").DefValue, "/existing/k3s.yaml")
	for _, flag := range []string{"runtime", "host", "ssh-key", "kubeconfig-output", "network", "port"} {
		assert.True(bootstrap.Flags().Lookup(flag) == nil)
	}
}
