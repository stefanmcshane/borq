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

// TestOrganizationRoleCommands owns role YAML, generated-client routing, revision
// propagation and API failures returned to CLI callers.
func TestOrganizationRoleCommands(t *testing.T) {
	assert := is.New(t)
	create := filepath.Join(t.TempDir(), "create.yaml")
	update := filepath.Join(t.TempDir(), "update.yaml")
	assert.NoErr(os.WriteFile(create, []byte("name: Reader\npermissions: [organization:read, app:read]\nrevision: 0\n"), 0600))
	assert.NoErr(os.WriteFile(update, []byte("name: Reader\npermissions: [organization:read, app:read]\nrevision: 3\n"), 0600))
	role := `{"id":"reader","name":"Reader","permissions":["organization:read","app:read"],"revision":4}`
	for _, scenario := range []struct {
		name, method, suffix, response, want string
		arguments                            []string
		status                               int
		revision                             int64
	}{
		{"list", http.MethodGet, "", "[" + role + "]", "Reader", []string{"list", "team"}, 200, 0},
		{"create", http.MethodPost, "", role, "Reader", []string{"create", "team", "--file", create}, 201, 0},
		{"update", http.MethodPut, "/reader", role, "Reader", []string{"update", "team", "reader", "--file", update}, 200, 3},
		{"delete", http.MethodDelete, "/reader", `{"id":"reader"}`, "reader", []string{"delete", "team", "reader", "--revision", "3"}, 200, 3},
		{"conflict", http.MethodDelete, "/reader", `{"message":"role is still assigned"}`, "role is still assigned", []string{"delete", "team", "reader", "--revision", "3"}, 409, 3},
		{"denied", http.MethodPost, "", `{"message":"permission denied"}`, "permission denied", []string{"create", "team", "--file", create}, 403, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, scenario.method)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/roles"+scenario.suffix)
				if request.Method == http.MethodPost || request.Method == http.MethodPut {
					var input appapi.OrganizationRoleInput
					assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
					assert.Equal(input.Name, "Reader")
					assert.Equal(input.Revision, scenario.revision)
					assert.Equal(input.Permissions, []appapi.Permission{appapi.Permission("organization:read"), appapi.Permission("app:read")})
				}
				if request.Method == http.MethodDelete {
					assert.Equal(request.URL.Query().Get("revision"), "3")
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(scenario.status)
				_, _ = writer.Write([]byte(scenario.response))
			}))
			defer server.Close()
			arguments := append([]string{"org", "roles"}, scenario.arguments...)
			arguments = append(arguments, "--api-url", server.URL)
			output, _, err := runApplicationCommand(t, arguments...)
			if scenario.status >= 400 {
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), scenario.want))
				return
			}
			assert.NoErr(err)
			assert.True(strings.Contains(output, scenario.want))
		})
	}
}
