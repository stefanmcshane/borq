package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestManagementBootstrapHasOnlyInstallationFlags confines setup to the selected Kubernetes connection and an explicit configuration file.
func TestManagementBootstrapHasOnlyInstallationFlags(t *testing.T) {
	assert := is.New(t)
	command := newManagementBootstrapCommand(context.Background(), logger.Logger{}, Config{})
	for _, name := range []string{"api-url", "probe-url", "management-openbao-url", "management-telemetry-url", "management-loki-url", "management-prometheus-url", "node-port", "host-port"} {
		assert.True(command.Flags().Lookup(name) == nil)
	}
	assert.True(command.Flags().Lookup("file") != nil)
	assert.True(command.Flags().Lookup("server-image") != nil)
	assert.True(command.Flags().Lookup("kubeconfig") != nil)
}

// TestManagementApplyUsesSelectedServer proves management lifecycle changes
// run through Borq's API instead of installing charts from the client machine.
func TestManagementApplyUsesSelectedServer(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodPut)
		assert.Equal(request.URL.Path, "/api/v1/management/configuration")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"id":"management-apply","status":"succeeded"}`))
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--server", server.URL)
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
	output, _, err := runConfiguredApplicationCommand("management", "apply", "-f", file, "--wait=false")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "status: succeeded"))
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

// TestManagementApplyPrivateRecoveryConfiguration binds setup credentials to independently retained recovery material.
func TestManagementApplyPrivateRecoveryConfiguration(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		assert.NoErr(err)
		assert.True(strings.Contains(string(body), `"password":"retained-password"`))
		assert.True(strings.Contains(string(body), `"repository":"s3:https://offsite.example/backups/borq"`))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"id":"accepted","status":"succeeded"}`))
	}))
	defer server.Close()
	policy := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(policy, []byte("offsite:\n  enabled: true\n  interval: 6h\n  keepLast: 14\n"), 0600))
	recovery := filepath.Join(t.TempDir(), "recovery.json")
	assert.NoErr(os.WriteFile(recovery, []byte(`{"offsite":{"repository":"s3:https://offsite.example/backups/borq","region":"region-one","password":"retained-password","accessKey":"access","secretKey":"secret"}}`), 0600))
	output, _, err := runConfiguredApplicationCommand("management", "apply", "--server", server.URL, "-f", policy, "--recovery-config", recovery, "--wait=false")
	assert.NoErr(err)
	assert.True(!strings.Contains(output, "retained-password"))
}

// TestManagementBootstrapIgnoresClientCluster keeps a saved application cluster
// from replacing the name selected by the explicit bootstrap kubeconfig.
func TestManagementBootstrapIgnoresClientCluster(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, err := runConfiguredApplicationCommand("config", "set", "--cluster", "other/profile")
	assert.NoErr(err)
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads++
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/namespaces/borq-mgmt/configmaps/borq-network-settings")
		http.Error(writer, "access denied", http.StatusForbidden)
	}))
	defer server.Close()
	path := clusterLinkTestKubeconfig(t)
	encoded, err := os.ReadFile(path)
	assert.NoErr(err)
	encoded = []byte(strings.ReplaceAll(string(encoded), "https://127.0.0.1:6443", server.URL))
	assert.NoErr(os.WriteFile(path, encoded, 0600))
	_, _, err = runConfiguredApplicationCommand("management", "bootstrap", "--kubeconfig", path)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "read installed management name"))
	assert.Equal(reads, 1)
}
