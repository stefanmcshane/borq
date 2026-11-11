package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestManagementDestroyRequiresExplicitCluster prevents teardown from borrowing ambient Kubernetes defaults.
func TestManagementDestroyRequiresExplicitCluster(t *testing.T) {
	assert := is.New(t)
	for _, arguments := range [][]string{{"management", "destroy"}, {"management", "destroy", "--confirm"}, {"management", "destroy", "--kubeconfig", "", "--confirm"}} {
		_, _, err := runApplicationCommand(t, arguments...)
		assert.True(err != nil && strings.Contains(err.Error(), "kubeconfig"))
	}
	output, _, err := runApplicationCommand(t, "management", "destroy", "--help")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "--confirm") && strings.Contains(output, "read-only plan"))
}

// TestManagementDestroyRequiresSavedLogin proves Kubernetes access alone cannot submit Borq teardown.
func TestManagementDestroyRequiresSavedLogin(t *testing.T) {
	_, _, err := runApplicationCommand(t, "management", "destroy", "--kubeconfig", "unopened", "--api-url", "https://borq.example.test", "--confirm")
	if err == nil || !strings.Contains(err.Error(), "borq login") {
		t.Fatalf("expected saved login requirement, got %v", err)
	}
}

// TestManagementDestroyPrivateProtocol owns saved-login selection, private API
// submission/polling and refusal to remove Borq without server completion proof.
func TestManagementDestroyPrivateProtocol(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		confirm       bool
		expected      string
	}{
		{"plan", "plan", false, ""},
		{"denied", "denied", true, "403"},
		{"server failure", "failed", true, "server teardown failed"},
		{"completion without receipt", "succeeded", true, "verify completed server teardown receipt"},
		{"unavailable", "unavailable", true, "503"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			origin := "https://borq.example.test"
			token := strings.Repeat("a", 64)
			credential, err := credentialPath(origin)
			assert.NoErr(err)
			assert.NoErr(savePrivateCredential(credential, token))
			prefix := "/api/v1/namespaces/borq-mgmt/services/http:borq:80/proxy"
			var calls []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls = append(calls, request.Method+" "+request.URL.Path)
				assert.Equal(request.Header.Get("Authorization"), "Bearer kube-only-token")
				writer.Header().Set("Content-Type", "application/json")
				if !strings.HasPrefix(request.URL.Path, prefix+"/api/") {
					assert.Equal(request.Method, http.MethodGet)
					assert.Equal(request.Header.Get("X-Borq-Admin-Token"), "")
					writer.WriteHeader(http.StatusNotFound)
					_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
					return
				}
				assert.Equal(request.Header.Get("X-Borq-Admin-Token"), token)
				path := strings.TrimPrefix(request.URL.Path, prefix)
				switch {
				case test.outcome == "denied":
					writer.WriteHeader(http.StatusForbidden)
					_, _ = writer.Write([]byte(`{"message":"permission denied"}`))
				case test.outcome == "unavailable":
					writer.WriteHeader(http.StatusServiceUnavailable)
				case path == "/api/v1/management/destroy" && request.Method == http.MethodGet:
					assert.True(!test.confirm)
					_, _ = writer.Write([]byte(`{"namespace":"borq-mgmt","namespaceUid":"namespace-id","clusterUid":"cluster-id","server":[],"resources":[],"dependencies":[{"group":"","version":"v1","resource":"configmaps","kind":"ConfigMap","namespace":"borq-mgmt","name":"borq-network-settings","uid":"settings-uid","resourceVersion":"1","ownershipHash":"network-settings-content-hash"}],"actions":[],"retained":["volumes"],"executed":false}`))
				case path == "/api/v1/management/destroy" && request.Method == http.MethodPost:
					assert.True(test.confirm)
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"teardown-operation","status":"queued"}`))
				case path == "/api/v1/management/destroy/operations/teardown-operation" && request.Method == http.MethodGet:
					assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"operation": map[string]string{"id": "teardown-operation", "status": test.outcome, "message": "provider ownership changed"}, "plan": map[string]any{}}))
				default:
					t.Errorf("unexpected private API request %s %s", request.Method, path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "kubeconfig")
			assert.NoErr(os.WriteFile(file, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: management\n    user: cli\nclusters:\n- name: management\n  cluster:\n    server: %s\n    insecure-skip-tls-verify: true\nusers:\n- name: cli\n  user:\n    token: kube-only-token\n", server.URL)), 0600))
			arguments := []string{"management", "destroy", "--api-url", origin, "--kubeconfig", file, "--timeout", "3s"}
			if test.confirm {
				arguments = append(arguments, "--confirm")
			}
			output, diagnostics, err := runConfiguredApplicationCommand(arguments...)
			if test.expected == "" {
				assert.NoErr(err)
				assert.True(strings.Contains(output, "volumes"))
				assert.True(strings.Contains(output, "network-settings-content-hash"))
			} else {
				assert.True(err != nil && strings.Contains(err.Error(), test.expected))
			}
			assert.True(!strings.Contains(output+diagnostics, token))
			assert.True(len(calls) >= 2)
			for _, call := range calls {
				assert.True(!strings.HasPrefix(call, "DELETE ") && !strings.HasPrefix(call, "PATCH ") && !strings.HasPrefix(call, "PUT "))
			}
		})
	}
}

// TestManagementDestroyResumeCompletedReceipt exercises the interrupted CLI path
// after server core objects are already gone, and refuses corrupt proof before
// contacting any API or issuing mutations.
func TestManagementDestroyResumeCompletedReceipt(t *testing.T) {
	for _, intact := range []bool{true, false} {
		t.Run(fmt.Sprintf("intact=%t", intact), func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			origin := "https://borq.example.test"
			credential, err := credentialPath(origin)
			assert.NoErr(err)
			assert.NoErr(savePrivateCredential(credential, strings.Repeat("r", 64)))
			plan := map[string]any{"namespace": "borq-mgmt", "namespaceUid": "management-uid", "clusterUid": "system-uid", "server": []any{}, "resources": []any{}, "dependencies": []any{}, "actions": []any{}, "retained": []string{"identity data"}, "executed": true}
			encoded, err := json.Marshal(map[string]any{"operationId": "completed-operation", "plan": plan})
			assert.NoErr(err)
			var calls []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls = append(calls, request.URL.Path)
				assert.Equal(request.Method, http.MethodGet)
				assert.True(!strings.Contains(request.URL.Path, "/proxy/"))
				assert.Equal(request.Header.Get("Authorization"), "Bearer kube-only-token")
				assert.Equal(request.Header.Get("X-Borq-Admin-Token"), "")
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/api/v1/namespaces/borq-mgmt/configmaps/borq-management-destroy":
					assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "immutable": intact,
						"metadata": map[string]any{"name": "borq-management-destroy", "namespace": "borq-mgmt", "uid": "receipt-uid", "resourceVersion": "1",
							"labels":      map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/component": "management-destroy"},
							"annotations": map[string]string{"borq.run/destroy-operation": "completed-operation", "borq.run/cluster-uid": "system-uid", "borq.run/namespace-uid": "management-uid"}},
						"data": map[string]string{"receipt.json": string(encoded)}}))
				case "/api/v1/namespaces/borq-mgmt", "/api/v1/namespaces/kube-system":
					name, uid := "borq-mgmt", "management-uid"
					if strings.HasSuffix(request.URL.Path, "kube-system") {
						name, uid = "kube-system", "system-uid"
					}
					assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": name, "uid": uid}}))
				default:
					writer.WriteHeader(http.StatusNotFound)
					_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "kubeconfig")
			assert.NoErr(os.WriteFile(file, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: management\n    user: cli\nclusters:\n- name: management\n  cluster:\n    server: %s\n    insecure-skip-tls-verify: true\nusers:\n- name: cli\n  user:\n    token: kube-only-token\n", server.URL)), 0600))
			output, _, err := runConfiguredApplicationCommand("management", "destroy", "--api-url", origin, "--kubeconfig", file, "--confirm")
			if intact {
				assert.NoErr(err)
				assert.True(strings.Contains(output, "identity data"))
			} else {
				assert.True(err != nil && strings.Contains(err.Error(), "intact owned immutable record"))
				assert.Equal(len(calls), 1)
			}
		})
	}
}
