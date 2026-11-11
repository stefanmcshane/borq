package cli

import (
	"encoding/json"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestOrganizationOidcCallbackCommand obtains provider registration URLs before
// credentials or discovery exist, using only the organization's immutable identity.
func TestOrganizationOidcCallbackCommand(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/organizations")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[{"name":"team","id":"0123456789abcdef0123456789abcdef","managedAddons":false}]`))
	}))
	defer server.Close()
	output, _, err := runApplicationCommand(t, "org", "oidc", "callback", "team", "--api-url", server.URL, "--issuer-url", "https://accounts.google.com", "--borq-public-url", "https://borq.example.test/")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "https://borq.example.test/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b"))
	assert.True(strings.Contains(output, "https://borq.example.test/login"))
}

// TestOrganizationOidcCallbackGuards refuses unknown identities and invalid
// origins rather than printing a provider redirect that Borq would never use.
func TestOrganizationOidcCallbackGuards(t *testing.T) {
	for _, test := range []struct{ name, response, base, message string }{
		{"missing organization", `[]`, "https://borq.example.test", "organization not found"},
		{"missing identity", `[{"name":"team"}]`, "https://borq.example.test", "immutable organization identity"},
		{"origin with path", `[{"name":"team","id":"0123456789abcdef0123456789abcdef"}]`, "https://borq.example.test/path", "origin without a path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			output, _, err := runApplicationCommand(t, "org", "oidc", "callback", "team", "--api-url", server.URL, "--issuer-url", "https://accounts.google.com", "--borq-public-url", test.base)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			assert.Equal(output, "")
		})
	}
}

// TestOrganizationOidcGetCommand reads saved callbacks through the existing
// authorized endpoint, so registration details remain available after apply exits.
func TestOrganizationOidcGetCommand(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved settings", true: "access denied"}[denied], func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/oidc")
				writer.Header().Set("Content-Type", "application/json")
				if denied {
					writer.WriteHeader(http.StatusForbidden)
					_, _ = writer.Write([]byte(`{"message":"permission denied"}`))
					return
				}
				_, _ = writer.Write([]byte(`{"callbackUrl":"https://borq.example.test/api/auth/oidc/callback/saved","clientSecretConfigured":true}`))
			}))
			defer server.Close()
			output, _, err := runApplicationCommand(t, "org", "oidc", "get", "team", "--api-url", server.URL)
			assert.Equal(err != nil, denied)
			if denied {
				assert.True(strings.Contains(err.Error(), "permission denied"))
				assert.Equal(output, "")
			} else {
				assert.True(strings.Contains(output, "https://borq.example.test/api/auth/oidc/callback/saved"))
			}
		})
	}
}

// TestManagementOidcCallbackBeforeFailure preserves registration URLs when the
// accepted operation fails, without disclosing client credentials or minting enrollment links.
func TestManagementOidcCallbackBeforeFailure(t *testing.T) {
	for _, savedBase := range []bool{false, true} {
		t.Run(map[bool]string{false: "declared origin", true: "saved origin"}[savedBase], func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method + " " + request.URL.Path {
				case "PUT /api/v1/management/configuration":
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"oidc-failure","status":"queued"}`))
				case "GET /api/v1/management/configuration":
					_, _ = writer.Write([]byte(`{"borqPublicUrl":"https://borq.example.test"}`))
				case "GET /api/v1/organizations":
					_, _ = writer.Write([]byte(`[{"name":"team","id":"0123456789abcdef0123456789abcdef","managedAddons":false}]`))
				case "GET /api/v1/management/operations/oidc-failure":
					_, _ = writer.Write([]byte(`{"id":"oidc-failure","status":"failed","phase":"oidc","message":"provider discovery unavailable"}`))
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "management.yaml")
			prefix := "borqPublicUrl: https://borq.example.test\n"
			if savedBase {
				prefix = ""
			}
			assert.NoErr(os.WriteFile(file, []byte(prefix+"organizations:\n  - name: team\n    oidc:\n      issuerUrl: https://accounts.google.com\n      clientId: client\n      clientSecret: private-client-secret\n      adminEmail: owner@example.test\n"), 0600))
			_, diagnostics, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "-f", file)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), "provider discovery unavailable"))
			assert.True(strings.Contains(diagnostics, "https://borq.example.test/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b"))
			assert.True(!strings.Contains(diagnostics, "private-client-secret"))

		})
	}
}

// TestManagementOidcCallbackReadFailure keeps optional callback diagnostics from
// replacing the operation's real failure or changing its exit behavior.
func TestManagementOidcCallbackReadFailure(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPut {
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"oidc-failure","status":"failed","message":"provider discovery unavailable"}`))
			return
		}
		assert.Equal(request.URL.Path, "/api/v1/organizations")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"private-diagnostic-sentinel"}`))
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("organizations:\n  - name: team\n    oidc:\n      adminEmail: owner@example.test\n      issuerUrl: https://accounts.google.com\n      borqPublicUrl: https://borq.example.test\n"), 0600))
	_, diagnostics, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "-f", file)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "provider discovery unavailable"))
	assert.True(strings.Contains(diagnostics, "borq org oidc callback NAME"))
	assert.True(!strings.Contains(diagnostics+err.Error(), "private-diagnostic-sentinel"))
}

// TestOrganizationOidcEnrollAdminCommand fences explicit subject enrollment with
// the current provider revision and preserves server authorization/conflict errors.
func TestOrganizationOidcEnrollAdminCommand(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := is.New(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				writer.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					assert.Equal(request.Method, http.MethodGet)
					assert.Equal(request.URL.Path, "/api/v1/organizations/team/oidc")
					_, _ = writer.Write([]byte(`{"revision":7}`))
					return
				}
				assert.Equal(request.Method, http.MethodPost)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/oidc/administrator")
				var input appapi.OrganizationOidcAdministratorInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
				assert.Equal(input.Subject, "provider-account-ID")
				assert.Equal(input.Revision, int64(7))
				writer.WriteHeader(status)
				if status != http.StatusOK {
					_, _ = writer.Write([]byte(`{"message":"enrollment rejected"}`))
					return
				}
				_, _ = writer.Write([]byte(`{"subject":"provider-account-ID","permissions":["*"],"revision":1}`))
			}))
			defer server.Close()
			output, _, err := runApplicationCommand(t, "org", "oidc", "enroll-admin", "team", "--subject", "provider-account-ID", "--api-url", server.URL)
			assert.Equal(calls, 2)
			if status == http.StatusOK {
				assert.NoErr(err)
				assert.True(strings.Contains(output, "provider-account-ID"))
				return
			}
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), "enrollment rejected"))
			assert.Equal(output, "")
		})
	}
}

// TestManagementOidcAdministratorDeclaration proves YAML account selection reaches
// management apply without a separate enrollment command or request.
func TestManagementOidcAdministratorDeclaration(t *testing.T) {
	assert := is.New(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "PUT /api/v1/management/configuration":
			var configuration appapi.ManagementConfiguration
			assert.NoErr(json.NewDecoder(request.Body).Decode(&configuration))
			assert.Equal(len(*configuration.Organizations), 1)
			provider := (*configuration.Organizations)[0].Oidc
			assert.True(provider.AdminSubject != nil)
			assert.Equal(*provider.AdminSubject, "selected-account")
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"configured","status":"succeeded"}`))
		case "GET /api/v1/management/configuration":
			_, _ = writer.Write([]byte(`{}`))
		case "GET /api/v1/organizations":
			_, _ = writer.Write([]byte(`[{"name":"team","id":"0123456789abcdef0123456789abcdef"}]`))
		default:
			t.Errorf("unexpected setup request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("borqPublicUrl: https://borq.example.test\norganizations:\n  - name: team\n    oidc:\n      issuerUrl: https://login.microsoftonline.com/tenant/v2.0\n      clientId: client\n      adminEmail: owner@example.test\n      adminSubject: selected-account\n"), 0600))
	_, _, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "-f", file)
	assert.NoErr(err)
	assert.Equal(calls, 3)
}
