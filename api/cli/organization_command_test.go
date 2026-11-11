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
)

// TestOrganizationCreateProviderInput owns private file input and explicit managed-instance selection through the generated client.
func TestOrganizationCreateProviderInput(t *testing.T) {
	assert := is.New(t)
	path := filepath.Join(t.TempDir(), "organization.yaml")
	assert.NoErr(os.WriteFile(path, []byte("adminEmail: owner@example.test\noidc:\n  issuerUrl: https://id.example.test\n  clientId: target-client\n  clientSecret: private-organization-client-secret\n  borqPublicUrl: https://borq.example.test\n  revision: 0\n"), 0o600))
	for _, test := range []struct {
		name string
		args []string
	}{
		{"external file", []string{"--file", path}},
		{"file administrator override", []string{"--file", path, "--admin-email", "other-owner@example.test"}},
		{"managed selection", []string{"--pocketid-organization", "initial", "--admin-email", "other-owner@example.test"}},
		{"administrator-token default", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPut)
				assert.Equal(request.URL.Path, "/api/v1/organizations/target")
				var input appapi.OrganizationOnboardingInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
				if strings.Contains(test.name, "file") {
					assert.True(input.Oidc != nil)
					expectedEmail := "owner@example.test"
					if test.name == "file administrator override" {
						expectedEmail = "other-owner@example.test"
					}
					assert.Equal(*input.AdminEmail, expectedEmail)
					assert.Equal(*input.Oidc.ClientSecret, "private-organization-client-secret")
				} else if test.name == "managed selection" {
					assert.True(input.Oidc == nil)
					assert.Equal(*input.PocketIDOrganization, "initial")
					assert.Equal(*input.AdminEmail, "other-owner@example.test")
				} else {
					assert.True(input.Oidc == nil && input.PocketID == nil && input.PocketIDOrganization == nil && input.AdminEmail == nil)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"name":"target","id":"target-id","managedAddons":true,"authenticationState":"queued"}`))
			}))
			defer server.Close()
			arguments := append([]string{"org", "create", "target", "--server", server.URL}, test.args...)
			output, diagnostic, err := runApplicationCommand(t, arguments...)
			assert.NoErr(err)
			assert.True(!strings.Contains(output+diagnostic, "private-organization-client-secret"))
			assert.True(strings.Contains(output, "authenticationState: queued"))
		})
	}
}
