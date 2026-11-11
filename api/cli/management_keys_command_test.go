package cli

import (
	"encoding/base64"
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

// TestManagementKeysCommand owns the kubeconfig-only export, private destination,
// custody instructions and retry behavior without printing administrator keys.
func TestManagementKeysCommand(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	borqToken, vaultToken := strings.Repeat("a", 64), strings.Repeat("b", 64)
	sealKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		writer.Header().Set("Content-Type", "application/json")
		data := map[string]string{}
		switch request.URL.Path {
		case "/api/v1/namespaces/borq-mgmt":
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"borq-mgmt"}}`))
			return
		case "/api/v1/namespaces/borq-mgmt/secrets/borq-admin-auth":
			data["token"] = base64.StdEncoding.EncodeToString([]byte(borqToken))
		case "/api/v1/namespaces/borq-mgmt/secrets/borq-openbao-admin":
			data["token"] = base64.StdEncoding.EncodeToString([]byte(vaultToken))
			data["sealKey"] = base64.StdEncoding.EncodeToString([]byte(sealKey))
		default:
			t.Errorf("unexpected Kubernetes request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		reads++
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "Secret", "data": data}))
	}))
	defer server.Close()
	kubeconfig := managementKeysTestKubeconfig(t, server.URL)
	destination := filepath.Join(t.TempDir(), "management recovery.json")
	t.Setenv("KUBECONFIG", kubeconfig)
	for _, explicit := range []bool{true, false} {
		arguments := []string{"management", "keys", "--output-file", destination}
		if explicit {
			arguments = append(arguments, "--kubeconfig", kubeconfig)
		}
		output, diagnostic, err := runApplicationCommand(t, arguments...)
		assert.NoErr(err)
		assert.True(strings.Contains(output, "Management keys saved to: "+destination))
		assert.True(strings.Contains(output, "secure storage outside your cluster"))
		assert.True(strings.Contains(output, "auto-unseal key"))
		assert.True(strings.Contains(output, "no separate backup key file is needed"))
		assert.True(strings.Contains(output, "borq login --api-url http://127.0.0.1:7833 --token-file '"+destination+"'"))
		assert.True(strings.Contains(output, "remove the working copy"))
		assert.True(strings.Contains(output, "borq management apply -f management.yaml"))
		assert.True(!strings.Contains(output, "--keys-file"))
		assert.True(strings.Contains(diagnostic, `Using Kubernetes context "selected"`))
		assert.True(!strings.Contains(output, "borq-management-kubeconfig-"))
		for _, secret := range []string{borqToken, vaultToken, sealKey} {
			assert.True(!strings.Contains(output+diagnostic, secret))
		}
	}
	assert.Equal(reads, 4)
	info, err := os.Stat(destination)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
}

// TestManagementBootstrapFromRestorePlaceholder refuses before reading cluster
// configuration or performing any restore operation.
func TestManagementBootstrapFromRestorePlaceholder(t *testing.T) {
	assert := is.New(t)
	t.Setenv("KUBECONFIG", "/must-not-read")
	_, _, err := runApplicationCommand(t, "management", "bootstrap-from-restore")
	assert.True(err != nil && strings.Contains(err.Error(), "not implemented yet; no resources have been changed"))
	assert.True(!strings.Contains(err.Error(), "must-not-read"))
}

// TestManagementLoginRecoveryFileThenApply retains only the Borq token at login
// and proves normal apply works after the working recovery file is removed.
func TestManagementLoginRecoveryFileThenApply(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	token := strings.Repeat("c", 64)
	vaultToken := strings.Repeat("v", 64)
	directory := t.TempDir()
	keys := filepath.Join(directory, "keys.json")
	assert.NoErr(os.WriteFile(keys, []byte(`{"borq":{"adminToken":"`+token+`"},"openbao":{"originalToken":"`+vaultToken+`","sealKey":"private-vault-seal-key"}}`), 0600))
	file := filepath.Join(directory, "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("organizations: []\n"), 0600))
	applies := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.True(request.Header.Get("Authorization") == "Bearer "+token)
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/auth/session":
			_, _ = writer.Write([]byte(`{"authenticated":true,"admin":true}`))
		case "GET /api/v1/organizations":
			_, _ = writer.Write([]byte(`[]`))
		case "PUT /api/v1/management/configuration":
			applies++
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"configuration","status":"queued"}`))
		default:
			t.Errorf("unexpected setup request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	output, diagnostic, err := runConfiguredApplicationCommand("login", "--api-url", server.URL, "--token-file", keys)
	assert.NoErr(err)
	credential, err := credentialPath(server.URL)
	assert.NoErr(err)
	saved, err := os.ReadFile(credential)
	assert.NoErr(err)
	assert.True(strings.TrimSpace(string(saved)) == token)
	assert.True(!strings.Contains(string(saved), vaultToken))
	assert.NoErr(os.Remove(keys))
	applyOutput, applyDiagnostic, err := runConfiguredApplicationCommand("management", "apply", "-f", file, "--wait=false")
	assert.NoErr(err)
	assert.Equal(applies, 1)
	assert.True(!strings.Contains(output+diagnostic+applyOutput+applyDiagnostic, token))
	_, _, err = runConfiguredApplicationCommand("management", "apply", "--keys-file", keys)
	assert.True(err != nil && strings.Contains(err.Error(), "unknown flag: --keys-file"))
	assert.Equal(applies, 1)
}

// TestManagementApplyWaitsForStartup proves the printed setup sequence can run
// before base services finish, with a bounded wait and one accepted submission.
func TestManagementApplyWaitsForStartup(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			directory := t.TempDir()
			keys := filepath.Join(directory, "keys.json")
			assert.NoErr(os.WriteFile(keys, []byte(strings.Repeat("c", 64)), 0600))
			file := filepath.Join(directory, "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("organizations: []\n"), 0600))
			attempts, accepted := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method + " " + request.URL.Path {
				case "PUT /api/v1/management/configuration":
					attempts++
					if attempts == 1 || !ready {
						writer.Header().Set("Retry-After", "1")
						writer.Header().Set("X-Borq-Setup-Pending", "true")
						writer.WriteHeader(http.StatusServiceUnavailable)
						_, _ = writer.Write([]byte(`{"message":"Management components are not ready: FailedCreate, namespace PodSecurity blocks the egress proxy."}`))
						return
					}
					accepted++
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"configuration","status":"succeeded"}`))
				case "GET /api/v1/management/configuration":
					_, _ = writer.Write([]byte(`{"organizations":[]}`))
				default:
					t.Errorf("unexpected setup request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			timeout := "3s"
			if !ready {
				timeout = "50ms"
			}
			_, diagnostic, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "--token-file", keys, "-f", file, "--timeout", timeout)
			assert.True(strings.Contains(diagnostic, "Waiting for Borq's supporting services"))
			assert.True(strings.Contains(diagnostic, "PodSecurity blocks the egress proxy"))
			if ready {
				assert.NoErr(err)
				assert.Equal(attempts, 2)
				assert.Equal(accepted, 1)
			} else {
				assert.True(err != nil && strings.Contains(err.Error(), "rerun management apply"))
				assert.True(strings.Contains(err.Error(), "PodSecurity blocks the egress proxy"))
				assert.Equal(accepted, 0)
			}
		})
	}
}

func managementKeysTestKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "selected kubeconfig")
	is.New(t).NoErr(os.WriteFile(path, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: selected\n    user: selected\nclusters:\n- name: selected\n  cluster:\n    server: %s\nusers:\n- name: selected\n  user: {}\n", server)), 0600))
	return path
}

// TestManagementKeysBeforeReady owns useful errors before bootstrap and while
// credentials are pending, without writing an incomplete recovery file.
func TestManagementKeysBeforeReady(t *testing.T) {
	for _, test := range []struct {
		name, message string
		namespace     bool
		slowNamespace bool
	}{
		{"not bootstrapped", "run borq management bootstrap", false, false},
		{"bootstrap in progress", "timed out waiting for management keys", true, false},
		{"namespace lookup timeout", "timed out waiting for management keys", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if test.slowNamespace {
					<-request.Context().Done()
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/api/v1/namespaces/borq-mgmt" && test.namespace {
					_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"borq-mgmt"}}`))
					return
				}
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Status","code":404,"reason":"NotFound"}`))
			}))
			defer server.Close()
			t.Setenv("KUBECONFIG", managementKeysTestKubeconfig(t, server.URL))
			file := filepath.Join(t.TempDir(), "recovery.json")
			_, diagnostic, err := runApplicationCommand(t, "management", "keys", "--output-file", file, "--timeout", "50ms")
			assert.True(err != nil && strings.Contains(err.Error(), test.message))
			assert.True(strings.Contains(err.Error(), "run borq management keys again"))
			assert.True(strings.Contains(diagnostic, "Waiting for management keys"))
			_, err = os.Stat(file)
			assert.True(os.IsNotExist(err))
		})
	}
}
