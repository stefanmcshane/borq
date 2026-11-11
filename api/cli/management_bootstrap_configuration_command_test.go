package cli

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/cli/management"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestManagementBootstrapPrivateSetup prints exact private access and explicit authentication without making follow-up API requests.
func TestManagementBootstrapPrivateSetup(t *testing.T) {
	assert := is.New(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	command := newManagementBootstrapCommand(t.Context(), logger.Logger{}, Config{})
	var output bytes.Buffer
	command.SetOut(&output)
	token := strings.Repeat("d", 64)
	assert.NoErr(finishManagementBootstrap(command, management.Result{ClusterName: "selected-cluster", Namespace: "borq-mgmt", KubeconfigPath: "/private/selected kubeconfig", ApiUrl: server.URL, AdminToken: token}))
	assert.Equal(calls, 0)
	assert.True(strings.Contains(output.String(), "borq management keys --kubeconfig '/private/selected kubeconfig'"))
	assert.True(!strings.Contains(output.String(), token))
	assert.True(!strings.Contains(output.String(), server.URL))
	assert.True(strings.Contains(output.String(), "Supporting services are being reconciled"))

}

// TestManagementBootstrapConfigurationGuard checks local configuration before any Kubernetes access.
func TestManagementBootstrapConfigurationGuard(t *testing.T) {
	for _, test := range []struct{ name, document, message string }{
		{"organization Pocket ID", "organizations:\n  - name: team\n    managedPocketId:\n      issuerUrl: https://id.example.test\n      adminEmail: owner@example.test\n", "deploy Pocket ID separately after bootstrap"},
		{"application Pocket ID", "organizations:\n  - name: team\n    spaces:\n      - name: identity\n        managedAddons:\n          pocketid:\n            - name: id\n", "deploy Pocket ID as a managed application after bootstrap"},
		{"unknown field", "notAConfigurationField: true\n", "configuration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte(test.document), 0600))
			_, _, err := runApplicationCommand(t, "management", "bootstrap", "--kubeconfig", "/must-not-read", "-f", file)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			assert.True(!strings.Contains(err.Error(), "must-not-read"))
		})
	}
}

// TestManagementBootstrapExternalOidc owns explicit configuration submission through the private Kubernetes connection without changing saved logins or enrolling Pocket ID.
func TestManagementBootstrapExternalOidc(t *testing.T) {
	for _, test := range []struct{ name, status, output string }{
		{"succeeded", "succeeded", "cli"},
		{"failed", "failed", "cli"},
		{"structured success", "succeeded", "json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			token := strings.Repeat("b", 64)
			prefix := "/api/v1/namespaces/borq-mgmt/services/http:borq:80/proxy"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				assert.Equal(request.Header.Get("X-Borq-Admin-Token"), token)
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method + " " + strings.TrimPrefix(request.URL.Path, prefix) {
				case "PUT /api/v1/management/configuration":
					var body appapi.ManagementConfiguration
					assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
					organization := (*body.Organizations)[0]
					assert.True(organization.ManagedPocketID == nil)
					assert.Equal(*organization.Oidc.ClientSecret, "private-google-client-secret")
					assert.Equal(organization.Oidc.AdminEmail, "owner@example.test")
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"configuration","status":"queued"}`))
				case "GET /api/v1/management/operations/configuration":
					_, _ = writer.Write([]byte(`{"id":"configuration","status":"` + status + `","message":"OIDC configuration result"}`))
				case "GET /api/v1/management/configuration":
					_, _ = writer.Write([]byte(`{"borqPublicUrl":"https://borq.example.test","network":{"managementUrl":"https://private.example.ts.net"},"organizations":[{"name":"team","oidc":{"issuerUrl":"https://accounts.google.com","adminEmail":"owner@example.test"}}]}`))
				case "GET /api/v1/organizations":
					_, _ = writer.Write([]byte(`[{"name":"team","id":"0123456789abcdef0123456789abcdef","managedAddons":false}]`))
				default:
					t.Errorf("unexpected bootstrap follow-up: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
			assert.NoErr(os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: selected\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: selected\n  context:\n    cluster: selected\ncurrent-context: selected\n"), 0600))
			access, err := management.ConnectPrivateServer(management.PrivateServerInput{KubeconfigPath: kubeconfig, Namespace: "borq-mgmt", AdminToken: token})
			assert.NoErr(err)
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("borqPublicUrl: https://borq.example.test\norganizations:\n  - name: team\n    oidc:\n      issuerUrl: https://accounts.google.com\n      clientId: client.apps.googleusercontent.com\n      clientSecret: private-google-client-secret\n      adminEmail: owner@example.test\n"), 0600))
			configuration, err := readManagementBootstrapConfiguration(file)
			assert.NoErr(err)
			credential, err := credentialPath(server.URL)
			assert.NoErr(err)
			assert.NoErr(savePrivateCredential(credential, strings.Repeat("s", 64)))
			command := newManagementBootstrapCommand(t.Context(), logger.Logger{}, Config{})
			command.SetContext(t.Context())
			assert.NoErr(command.Flags().Set("output", test.output))
			var output, diagnostic bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&diagnostic)
			err = applyManagementBootstrapConfiguration(command, management.Result{ApiUrl: access.URL, AdminToken: token, KubernetesProxy: true, HttpClient: access.HttpClient}, configuration, time.Minute)
			assert.Equal(err != nil, status == "failed")
			if err != nil {
				assert.True(strings.Contains(err.Error(), "Borq is installed"))
				assert.True(strings.Contains(err.Error(), "borq management apply"))
			}
			if status == "succeeded" && test.output == "cli" {
				assert.Equal(calls, 5)
				assert.True(strings.Contains(output.String(), "Private callback: https://private.example.ts.net/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b"))
			} else {
				assert.Equal(calls, 3)
			}
			if test.output == "json" {
				var operation appapi.Operation
				assert.NoErr(json.Unmarshal(output.Bytes(), &operation))
				assert.Equal(operation.Status, appapi.Succeeded)
			}
			assert.True(strings.Contains(diagnostic.String(), "https://borq.example.test/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b"))
			assert.True(!strings.Contains(output.String()+diagnostic.String(), token))
			assert.True(!strings.Contains(output.String()+diagnostic.String(), "private-google-client-secret"))
			retained, err := os.ReadFile(credential)
			assert.NoErr(err)
			assert.Equal(strings.TrimSpace(string(retained)), strings.Repeat("s", 64))
		})
	}
}

// TestManagementApplyCommandScopedToken proves bootstrap setup works without login, persists no replacement credential and prints provider enrollment only after successful apply.
func TestManagementApplyCommandScopedToken(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial enrollment", true: "existing passkey"}[enrolled], func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			token := strings.Repeat("e", 64)
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("borqPublicUrl: https://borq.example.test\norganizations:\n  - name: team\n    managedPocketId:\n      issuerUrl: https://id.example.test\n      adminEmail: owner@example.test\n"), 0600))
			tokenFile := filepath.Join(t.TempDir(), "bootstrap.token")
			assert.NoErr(os.WriteFile(tokenFile, []byte(token), 0600))
			submissions, enrollments, polls := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Header.Get("Authorization"), "Bearer "+token)
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method + " " + request.URL.Path {
				case "PUT /api/v1/management/configuration":
					submissions++
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"configuration","status":"queued"}`))
				case "GET /api/v1/management/operations/configuration":
					polls++
					if polls <= 2 {
						assert.Equal(enrollments, 0)
						_, _ = writer.Write([]byte(`{"id":"configuration","status":"running","phase":"pocketid","message":"Checking Pocket ID discovery"}`))
						return
					}
					_, _ = writer.Write([]byte(`{"id":"configuration","status":"succeeded"}`))
				case "GET /api/v1/management/configuration":
					_, _ = writer.Write([]byte(`{"borqPublicUrl":"https://borq.example.test","network":{"managementUrl":"https://private.example.ts.net"},"organizations":[{"name":"team","managedPocketId":{"issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}}]}`))
				case "GET /api/v1/organizations":
					_, _ = writer.Write([]byte(`[{"name":"team","id":"0123456789abcdef0123456789abcdef"}]`))
				case "POST /api/v1/organizations/team/pocketid/admin-enrollment":
					enrollments++
					if enrolled {
						_, _ = writer.Write([]byte(`{"enrolled":true}`))
					} else {
						_, _ = writer.Write([]byte(`{"enrolled":false,"url":"https://id.example.test/lc/private-initial-link"}`))
					}
				default:
					t.Errorf("unexpected setup request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			credential, err := credentialPath(server.URL)
			assert.NoErr(err)
			assert.NoErr(savePrivateCredential(credential, strings.Repeat("s", 64)))
			output, diagnostic, err := runConfiguredApplicationCommand("management", "apply", "--api-url", server.URL, "--token-file", tokenFile, "-f", file)
			assert.NoErr(err)
			assert.Equal(submissions, 1)
			assert.Equal(enrollments, 1)
			assert.True(strings.Contains(diagnostic, "operation configuration: queued"))
			assert.Equal(strings.Count(diagnostic, "operation configuration: running (pocketid): Checking Pocket ID discovery"), 1)
			assert.True(strings.Contains(output, "Public sign-in: https://borq.example.test/login"))
			assert.True(strings.Contains(output, "Private sign-in: https://private.example.ts.net/login"))
			assert.True(strings.Contains(output, "Pocket ID team: https://id.example.test"))
			assert.True(strings.Contains(output, "Public callback: https://borq.example.test/api/auth/oidc/callback/"))
			assert.True(!strings.Contains(output, "Private callback:"))
			assert.Equal(strings.Contains(output, "https://id.example.test/lc/private-initial-link"), !enrolled)
			assert.True(!strings.Contains(output+diagnostic, token))
			assert.True(!strings.Contains(diagnostic, "private-initial-link"))
			retained, err := os.ReadFile(credential)
			assert.NoErr(err)
			assert.Equal(strings.TrimSpace(string(retained)), strings.Repeat("s", 64))
			settings, _, err := loadClientSettings()
			assert.NoErr(err)
			assert.True(settings.Server != server.URL)
		})
	}
}

// TestManagementApplyEnrollmentRequiresCompletion prevents queued or failed configuration from minting an enrollment credential.
func TestManagementApplyEnrollmentRequiresCompletion(t *testing.T) {
	for _, test := range []struct {
		name, status    string
		wait, wantError bool
	}{
		{"queued without waiting", "queued", false, false},
		{"failed", "failed", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				assert.Equal(request.Method, http.MethodPut)
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusAccepted)
				_, _ = writer.Write([]byte(`{"id":"configuration","status":"` + test.status + `","message":"setup rejected"}`))
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
			arguments := []string{"management", "apply", "--api-url", server.URL, "-f", file}
			if !test.wait {
				arguments = append(arguments, "--wait=false")
			}
			_, _, err := runApplicationCommand(t, arguments...)
			assert.Equal(err != nil, test.wantError)
			assert.Equal(calls, 1)
		})
	}
}

// TestManagementApplyPrivateTokenGuard rejects public or empty credential files before a request can use a saved login or leak an administrator token.
func TestManagementApplyPrivateTokenGuard(t *testing.T) {
	for _, test := range []struct {
		name  string
		empty bool
		mode  os.FileMode
	}{{"empty path", true, 0600}, {"public permissions", false, 0644}} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "admin.token")
			assert.NoErr(os.WriteFile(tokenFile, []byte(strings.Repeat("g", 64)), test.mode))
			assert.NoErr(os.Chmod(tokenFile, test.mode))
			if test.empty {
				tokenFile = ""
			}
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
			_, _, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "--token-file", tokenFile, "-f", file)
			assert.True(err != nil)
			if test.empty {
				assert.True(strings.Contains(err.Error(), "must name a private"))
			} else {
				assert.True(strings.Contains(err.Error(), "accessible only by its owner"))
			}
			assert.Equal(calls, 0)
		})
	}
}
