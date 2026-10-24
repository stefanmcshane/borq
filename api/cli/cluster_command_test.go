package cli

import (
	"bytes"
	"context"
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
	"k8s.io/client-go/tools/clientcmd"
)

// clusterLinkTestKubeconfig creates independent, embedded credentials for CLI HTTP fixtures.
func clusterLinkTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: test-credential
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClusterLinkApiServerOverride rewrites only the chosen cluster in the upload,
// without probing Kubernetes or changing the source kubeconfig.
func TestClusterLinkApiServerOverride(t *testing.T) {
	is := is.New(t)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	original := `apiVersion: v1
kind: Config
current-context: first
clusters:
- name: first
  cluster:
    server: https://first.invalid:6443
- name: second
  cluster:
    server: https://second.invalid:6443
contexts:
- name: first
  context:
    cluster: first
    user: shared
- name: second
  context:
    cluster: second
    user: shared
users:
- name: shared
  user:
    token: private-test-token
`
	is.NoErr(os.WriteFile(kubeconfig, []byte(original), 0600))
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		var body appapi.ClusterInput
		is.NoErr(json.NewDecoder(r.Body).Decode(&body))
		is.True(body.Context != nil)
		is.Equal(*body.Context, "second")
		is.True(body.Kubeconfig != nil)
		is.True(body.ClientApiServer != nil)
		is.Equal(*body.ClientApiServer, "https://second.invalid:6443")
		configuration, err := clientcmd.Load([]byte(*body.Kubeconfig))
		is.NoErr(err)
		is.Equal(configuration.Clusters["first"].Server, "https://first.invalid:6443")
		is.Equal(configuration.Clusters["second"].Server, "https://server-reachable.invalid:7443")
		is.Equal(configuration.CurrentContext, "first")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"target"}`))
	}))
	defer server.Close()
	_, _, err := runApplicationCommand(t, "cluster", "link", "target", "--organization", "team", "--server", server.URL, "--kubeconfig", kubeconfig, "--context", "second", "--api-server", "https://server-reachable.invalid:7443", "--reconcile=false", "--diff=false")
	is.NoErr(err)
	is.True(received)
	after, err := os.ReadFile(kubeconfig)
	is.NoErr(err)
	is.Equal(string(after), original)
}

// TestClusterLinkRejectsInvalidSelection avoids sending unusable contexts or unsafe endpoint overrides to Borq.
func TestClusterLinkRejectsInvalidSelection(t *testing.T) {
	kubeconfig := clusterLinkTestKubeconfig(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"target"}`))
	}))
	defer server.Close()
	for _, flags := range [][]string{
		{"--context", "missing"},
		{"--api-server", "not-a-url"},
		{"--api-server", "ftp://target.invalid"},
		{"--api-server", "https://user:secret@target.invalid"},
		{"--api-server", "https://target.invalid?token=secret"},
		{"--api-server", "https://target.invalid#secret"},
	} {
		is := is.New(t)
		arguments := []string{"cluster", "link", "target", "--organization", "team", "--server", server.URL, "--kubeconfig", kubeconfig}
		_, _, err := runApplicationCommand(t, append(arguments, flags...)...)
		is.True(err != nil)
		message := "--api-server must"
		if flags[0] == "--context" {
			message = "selected kubeconfig context"
		}
		is.True(strings.Contains(err.Error(), message))
		is.Equal(requests.Load(), int32(0))
	}
}

// TestClusterLinkSelectsOnlyBorqLocalCredentials keeps implicit linking scoped
// to the requested locally created cluster instead of the active kubeconfig.
func TestClusterLinkSelectsOnlyBorqLocalCredentials(t *testing.T) {
	assert := is.New(t)
	homeDirectory := t.TempDir()
	t.Setenv("HOME", homeDirectory)
	t.Setenv("KUBECONFIG", clusterLinkTestKubeconfig(t))
	selected, err := resolveClusterLinkCredentials(clusterLinkOptions{}, "management")
	assert.NoErr(err)
	assert.Equal(selected.path, filepath.Join(homeDirectory, ".config", "borq", "kubeconfigs", "management"))
	assert.Equal(selected.localName, "management")
	alias, err := resolveClusterLinkCredentials(clusterLinkOptions{localName: "management"}, "local")
	assert.NoErr(err)
	assert.Equal(alias, selected)
	explicit := clusterLinkTestKubeconfig(t)
	selected, err = resolveClusterLinkCredentials(clusterLinkOptions{kubeconfig: explicit}, "local")
	assert.NoErr(err)
	assert.Equal(selected.path, explicit)
	assert.Equal(selected.localName, "")
	_, err = resolveClusterLinkCredentials(clusterLinkOptions{kubeconfig: explicit, localName: "management"}, "local")
	assert.True(err != nil)
	_, _, err = runApplicationCommand(t, "cluster", "link", "local", "--organization", "team", "--kubeconfig", explicit, "--local", "management")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "kubeconfig"))
	assert.True(strings.Contains(err.Error(), "local"))
}

// TestClusterLinkLocalDiscoveryUploadsInternalAddress exercises SDK node lookup
// and registration together while preserving the operator's local kubeconfig.
func TestClusterLinkLocalDiscoveryUploadsInternalAddress(t *testing.T) {
	for _, input := range []struct{ local, override, routed bool }{{local: true}, {local: true, override: true}, {local: true, routed: true}, {}} {
		assert := is.New(t)
		var nodeRequests, registrations atomic.Int32
		kube := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			nodeRequests.Add(1)
			assert.Equal(request.Method, http.MethodGet)
			assert.Equal(request.URL.Path, "/api/v1/nodes")
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","items":[{"metadata":{"name":"agent"},"status":{"addresses":[{"type":"InternalIP","address":"172.30.0.11"}]}},{"metadata":{"name":"management-server-0","labels":{"node-role.kubernetes.io/control-plane":"true"}},"status":{"addresses":[{"type":"InternalIP","address":"172.30.0.10"}]}}]}`))
		}))
		configuration := clusterLinkTestKubeconfig(t)
		original, err := os.ReadFile(configuration)
		assert.NoErr(err)
		original = []byte(strings.ReplaceAll(string(original), "https://127.0.0.1:6443", kube.URL))
		if input.routed {
			original = []byte(strings.ReplaceAll(string(original), kube.URL, "https://routed.example.test:6443"))
		}
		assert.NoErr(os.WriteFile(configuration, original, 0600))
		expected := kube.URL
		localName := ""
		if input.local {
			expected = "http://172.30.0.10:6443"
			localName = "management"
		}
		if input.routed {
			expected = "https://routed.example.test:6443"
		}
		apiServer := ""
		if input.override {
			apiServer = "https://explicit-api.example.test:7443"
			expected = apiServer
		}
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			registrations.Add(1)
			assert.Equal(request.Method, http.MethodPut)
			assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters/local")
			var input appapi.ClusterInput
			assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
			assert.True(input.Kubeconfig != nil)
			uploaded, err := clientcmd.Load([]byte(*input.Kubeconfig))
			assert.NoErr(err)
			assert.Equal(uploaded.Clusters["test"].Server, expected)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"name":"local"}`))
		}))
		command := &cobra.Command{}
		command.SetContext(context.Background())
		var output bytes.Buffer
		command.SetOut(&output)
		options := clusterLinkOptions{scope: applicationScope{server: server.URL, organization: "team"}, apiServer: apiServer}
		err = executeClusterLinkRegistration(command, options, "local", clusterLinkCredentials{path: configuration, localName: localName})
		assert.NoErr(err)
		assert.Equal(registrations.Load(), int32(1))
		expectedLookups := int32(1)
		if input.override || input.routed || !input.local {
			expectedLookups = 0
		}
		assert.Equal(nodeRequests.Load(), expectedLookups)
		after, err := os.ReadFile(configuration)
		assert.NoErr(err)
		assert.Equal(after, original)
		server.Close()
		kube.Close()
	}
}

// TestClusterLinkMissingLocalCredentialsExplainsRecovery prevents falling back
// to another cluster when the requested local cluster has not been created.
func TestClusterLinkMissingLocalCredentialsExplainsRecovery(t *testing.T) {
	assert := is.New(t)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	err := executeClusterLinkRegistration(command, clusterLinkOptions{}, "local", clusterLinkCredentials{path: filepath.Join(t.TempDir(), "missing"), localName: "management"})
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "no saved credentials"))
	assert.True(strings.Contains(err.Error(), "--kubeconfig"))
}
