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
)

// TestOrganizationMemberCommands owns member file input, generated-client routing,
// role assignment updates, and permission failures propagated to CLI callers.
func TestOrganizationMemberCommands(t *testing.T) {
	update := filepath.Join(t.TempDir(), "update.yaml")
	assert := is.New(t)
	assert.NoErr(os.WriteFile(update, []byte("revision: 3\ndisabled: true\nroleIds: [reader]\n"), 0600))
	member := `{"id":"b9c734d7-5d2c-4be6-baf8-4bd63529a43b","email":"reader@example.test","emailVerified":true,"issuer":"https://id.example.test","subject":"","state":"active","roleIds":["reader"],"revision":4,"permissions":["organization:read"]}`
	for _, scenario := range []struct {
		name, method, suffix, response, want string
		arguments                            []string
		status                               int
	}{
		{"list", http.MethodGet, "", "[" + member + "]", "reader@example.test", []string{"list", "team"}, 200},
		{"update", http.MethodPut, "/member-id", member, "reader@example.test", []string{"update", "team", "member-id", "--file", update}, 200},
		{"denied", http.MethodPut, "/member-id", `{"message":"permission denied"}`, "permission denied", []string{"update", "team", "member-id", "--file", update}, 403},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, scenario.method)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/members"+scenario.suffix)
				if scenario.method == http.MethodPut {
					var input map[string]any
					assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
					assert.Equal(input["roleIds"], []any{"reader"})
					assert.Equal(input["revision"], float64(3))
					assert.Equal(input["disabled"], true)
					_, hasPermissions := input["permissions"]
					assert.True(!hasPermissions)
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(scenario.status)
				_, _ = writer.Write([]byte(scenario.response))
			}))
			defer server.Close()
			arguments := append([]string{"org", "members"}, scenario.arguments...)
			arguments = append(arguments, "--api-url", server.URL)
			output, _, err := runApplicationCommand(t, arguments...)
			if scenario.status == http.StatusForbidden {
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), scenario.want))
				return
			}
			assert.NoErr(err)
			assert.True(strings.Contains(output, scenario.want))
		})
	}
}
