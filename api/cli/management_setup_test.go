package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestInitialAdministratorEnrollmentLinkValidation keeps malformed or foreign enrollment credentials out of CLI diagnostics and output.
func TestInitialAdministratorEnrollmentLinkValidation(t *testing.T) {
	for _, response := range []string{
		`{"enrolled":false,"url":"https://foreign.example.test/lc/private-link"}`,
		`{"enrolled":false,"url":"http://id.example.test/lc/private-link"}`,
		`{"enrolled":false}`,
	} {
		assert := is.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assert.Equal(request.Method, http.MethodPost)
			assert.Equal(request.URL.Path, "/api/v1/organizations/team/pocketid/admin-enrollment")
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(response))
		}))
		t.Cleanup(server.Close)
		client, err := applicationClientWithToken(server.URL, "")
		assert.NoErr(err)
		command := &cobra.Command{}
		command.SetContext(t.Context())
		var output, diagnostics bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&diagnostics)
		err = printInitialAdministratorEnrollment(command, client, "team", "https://id.example.test")
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error()+output.String()+diagnostics.String(), "private-link"))
	}
}

// TestManagementApplySavedSignInSummary prints every saved provider after an unrelated update, including the observed private origin.
func TestManagementApplySavedSignInSummary(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "PUT /api/v1/management/configuration":
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"updated","status":"succeeded","resources":[]}`))
		case "GET /api/v1/management/configuration":
			_, _ = writer.Write([]byte(`{"borqPublicUrl":"https://borq.example.test/","network":{"managementUrl":"https://observed.tail123.ts.net/"},"organizations":[{"name":"first","oidc":{"issuerUrl":"https://accounts.google.com","adminEmail":"first@example.test","clientSecretRef":"private-vault-reference"}},{"name":"second","oidc":{"issuerUrl":"https://login.example.test/tenant","adminEmail":"second@example.test","borqPublicUrl":"https://other.example.test"}}]}`))
		case "GET /api/v1/organizations":
			_, _ = writer.Write([]byte(`[{"name":"first","id":"0123456789abcdef0123456789abcdef"},{"name":"second","id":"fedcba9876543210fedcba9876543210"}]`))
		default:
			t.Errorf("unexpected summary request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
	output, diagnostics, err := runApplicationCommand(t, "management", "apply", "-f", file, "--api-url", server.URL)
	assert.NoErr(err)
	assert.Equal(diagnostics, "")
	assert.Equal(output, `Management configuration applied.
Public sign-in: https://borq.example.test/login
Private sign-in: https://observed.tail123.ts.net/login

Register every callback and logout URL below with its identity provider.

Organization first
  OIDC issuer: https://accounts.google.com
  Administrator: first@example.test
  Public sign-in: https://borq.example.test/login?organization=first
  Public callback: https://borq.example.test/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b
  Public logout: https://borq.example.test/login
  Private sign-in: https://observed.tail123.ts.net/login?organization=first
  Private callback: https://observed.tail123.ts.net/api/auth/oidc/callback/100cfc133493de683d5680eb2855507b
  Private logout: https://observed.tail123.ts.net/login

Organization second
  OIDC issuer: https://login.example.test/tenant
  Administrator: second@example.test
  Public sign-in: https://other.example.test/login?organization=second
  Public callback: https://other.example.test/api/auth/oidc/callback/36dced1873af46ff0acae1ffad8a5097
  Public logout: https://other.example.test/login
  Private sign-in: https://observed.tail123.ts.net/login?organization=second
  Private callback: https://observed.tail123.ts.net/api/auth/oidc/callback/36dced1873af46ff0acae1ffad8a5097
  Private logout: https://observed.tail123.ts.net/login
`)
	t.Logf("CLI output:\n%s", output)
}

// TestManagementApplySummaryReadFailure keeps a successful apply successful when optional saved configuration or identity reads fail.
func TestManagementApplySummaryReadFailure(t *testing.T) {
	for _, unavailable := range []string{"/api/v1/management/configuration", "/api/v1/organizations"} {
		t.Run(unavailable, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPut {
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"updated","status":"succeeded"}`))
					return
				}
				if request.URL.Path == unavailable {
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = writer.Write([]byte(`{"message":"private-diagnostic-sentinel"}`))
					return
				}
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				_, _ = writer.Write([]byte(`{"organizations":[{"name":"team","oidc":{"issuerUrl":"https://accounts.google.com","adminEmail":"owner@example.test"}}]}`))
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
			output, diagnostics, err := runApplicationCommand(t, "management", "apply", "-f", file, "--api-url", server.URL)
			assert.NoErr(err)
			assert.True(strings.HasPrefix(output, "Management configuration applied.\n"))
			assert.True(strings.Contains(diagnostics, "Warning: management configuration applied"))
			assert.True(strings.Contains(diagnostics, "borq org oidc get NAME"))
			assert.True(!strings.Contains(output+diagnostics, "private-diagnostic-sentinel"))
		})
	}
}

// TestManagementApplyJsonOutput preserves one structured operation and skips human setup reads for both accepted and completed work.
func TestManagementApplyJsonOutput(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "completed"}[wait], func(t *testing.T) {
			assert := is.New(t)
			status := "queued"
			if wait {
				status = "succeeded"
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPut)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusAccepted)
				_, _ = writer.Write([]byte(`{"id":"updated","status":"` + status + `"}`))
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("components: {}\n"), 0600))
			arguments := []string{"management", "apply", "-f", file, "--api-url", server.URL, "--output", "json"}
			if !wait {
				arguments = append(arguments, "--wait=false")
			}
			output, diagnostics, err := runApplicationCommand(t, arguments...)
			assert.NoErr(err)
			var operation appapi.Operation
			assert.NoErr(json.Unmarshal([]byte(output), &operation))
			assert.Equal(operation.ID, "updated")
			assert.Equal(string(operation.Status), status)
			assert.Equal(diagnostics, "")
		})
	}
}

// TestManagementSetupPrintsAllAddressesBeforeEnrollment preserves usable addresses when one provider temporarily cannot create its initial enrollment link.
func TestManagementSetupPrintsAllAddressesBeforeEnrollment(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/management/configuration" {
			_, _ = writer.Write([]byte(`{"organizations":[{"name":"first","managedPocketId":{"issuerUrl":"https://id-first.example.test","adminEmail":"first@example.test"}},{"name":"second","managedPocketId":{"issuerUrl":"https://id-second.example.test","adminEmail":"second@example.test"}}]}`))
			return
		}
		if request.URL.Path == "/api/v1/organizations" {
			_, _ = writer.Write([]byte(`[{"name":"first","id":"0123456789abcdef0123456789abcdef"},{"name":"second","id":"abcdef0123456789abcdef0123456789"}]`))
			return
		}
		assert.Equal(request.URL.Path, "/api/v1/organizations/first/pocketid/admin-enrollment")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"message":"Provider enrollment is temporarily unavailable."}`))
	}))
	defer server.Close()
	client, err := applicationClientWithToken(server.URL, "")
	assert.NoErr(err)
	command := &cobra.Command{}
	command.SetContext(t.Context())
	var output bytes.Buffer
	command.SetOut(&output)
	err = printManagementSetup(command, client)
	assert.True(err != nil)
	assert.True(strings.Contains(output.String(), "Pocket ID first: https://id-first.example.test"))
	assert.True(strings.Contains(output.String(), "Pocket ID second: https://id-second.example.test"))
	assert.True(strings.Contains(err.Error(), "management configuration applied"))
}
