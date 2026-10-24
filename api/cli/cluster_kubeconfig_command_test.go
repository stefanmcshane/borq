package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"k8s.io/client-go/tools/clientcmd"
)

// TestClusterKubeconfigUsesManagementApi downloads credentials using profile defaults
// and explicit targets without reading a local Kubernetes configuration.
func TestClusterKubeconfigUsesManagementApi(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	source, err := os.ReadFile(clusterLinkTestKubeconfig(t))
	assert.NoErr(err)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		paths = append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		assert.NoErr(json.NewEncoder(writer).Encode(appapi.ClusterKubeconfig{Kubeconfig: string(source)}))
	}))
	defer server.Close()
	_, _, err = runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization", "team", "--cluster", "staging")
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"cluster", "kubeconfig", "--profile", "work"},
		{"cluster", "kubeconfig", "production", "--profile", "work", "--organization", "other", "--output", "-"},
	} {
		output, diagnostics, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
		assert.Equal(output, string(source))
		assert.Equal(diagnostics, "")
	}
	assert.Equal(paths, []string{
		"/api/v1/organizations/team/clusters/staging/kubeconfig",
		"/api/v1/organizations/other/clusters/production/kubeconfig",
	})
}

// TestClusterKubeconfigSavesPrivateFile replaces an explicit destination atomically
// with private permissions and keeps credentials out of command diagnostics.
func TestClusterKubeconfigSavesPrivateFile(t *testing.T) {
	assert := is.New(t)
	source, err := os.ReadFile(clusterLinkTestKubeconfig(t))
	assert.NoErr(err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		assert.NoErr(json.NewEncoder(writer).Encode(appapi.ClusterKubeconfig{Kubeconfig: string(source)}))
	}))
	defer server.Close()
	directory := t.TempDir()
	destination := filepath.Join(directory, "downloaded.yaml")
	assert.NoErr(os.WriteFile(destination, []byte("old"), 0644))
	output, diagnostics, err := runApplicationCommand(t, "cluster", "kubeconfig", "target", "--organization", "team", "--api-url", server.URL, "--output", destination)
	assert.NoErr(err)
	assert.Equal(output, "")
	assert.True(strings.Contains(diagnostics, "Select this file with kubectl --kubeconfig or KUBECONFIG"))
	downloaded, err := os.ReadFile(destination)
	assert.NoErr(err)
	assert.Equal(string(downloaded), string(source))
	info, err := os.Stat(destination)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	files, err := os.ReadDir(directory)
	assert.NoErr(err)
	assert.Equal(len(files), 1)
}

// TestClusterKubeconfigOverridesClientEndpoint allows an exported config to be used
// from another network without changing the management server's stored config.
func TestClusterKubeconfigOverridesClientEndpoint(t *testing.T) {
	assert := is.New(t)
	source, err := os.ReadFile(clusterLinkTestKubeconfig(t))
	assert.NoErr(err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		writer.Header().Set("Content-Type", "application/json")
		assert.NoErr(json.NewEncoder(writer).Encode(appapi.ClusterKubeconfig{Kubeconfig: string(source)}))
	}))
	defer server.Close()
	arguments := []string{"cluster", "kubeconfig", "target", "--organization", "team", "--api-url", server.URL}
	output, _, err := runApplicationCommand(t, append(arguments, "--api-server", "https://reachable.invalid:7443")...)
	assert.NoErr(err)
	configuration, err := clientcmd.Load([]byte(output))
	assert.NoErr(err)
	assert.Equal(configuration.Clusters["test"].Server, "https://reachable.invalid:7443")
	assert.Equal(configuration.AuthInfos["test"].Token, "test-credential")
	output, _, err = runApplicationCommand(t, arguments...)
	assert.NoErr(err)
	assert.Equal(output, string(source))
	output, diagnostics, err := runApplicationCommand(t, append(arguments, "--api-server", "https://user:secret@target.invalid")...)
	assert.True(err != nil)
	assert.Equal(output, "")
	assert.True(!strings.Contains(diagnostics, "test-credential"))
}

// TestClusterKubeconfigFailuresNeverPrintCredentials protects stdout and an existing
// destination when the API fails or returns an unusable kubeconfig.
func TestClusterKubeconfigFailuresNeverPrintCredentials(t *testing.T) {
	for _, input := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing", http.StatusNotFound, `{"message":"private-test-token"}`},
		{"server error", http.StatusInternalServerError, `{"message":"private-test-token"}`},
		{"malformed kubeconfig", http.StatusOK, `{"kubeconfig":"private-test-token: ["}`},
		{"empty kubeconfig", http.StatusOK, `{"kubeconfig":""}`},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(input.status)
				_, _ = writer.Write([]byte(input.body))
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "existing.yaml")
			assert.NoErr(os.WriteFile(destination, []byte("existing"), 0600))
			output, diagnostics, err := runApplicationCommand(t, "cluster", "kubeconfig", "target", "--organization", "team", "--api-url", server.URL, "--output", destination)
			assert.True(err != nil)
			assert.Equal(output, "")
			assert.True(!strings.Contains(diagnostics+err.Error(), "private-test-token"))
			contents, err := os.ReadFile(destination)
			assert.NoErr(err)
			assert.Equal(string(contents), "existing")
		})
	}
}

// TestClusterKubeconfigRequiresTarget reports missing local selection before any API
// request, rather than reading a local cluster's kubeconfig as a fallback.
func TestClusterKubeconfigRequiresTarget(t *testing.T) {
	assert := is.New(t)
	_, _, err := runApplicationCommand(t, "cluster", "kubeconfig", "--organization", "team")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "provide a cluster name"))
	_, _, err = runApplicationCommand(t, "cluster", "kubeconfig", "target")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "provide --organization"))
}
