package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestOrganizationPermissionsCommand preserves read-only administrator grants and permission diagnostics.
func TestOrganizationPermissionsCommand(t *testing.T) {
	for _, denied := range []bool{false, true} {
		assert := is.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assert.Equal(request.URL.Path, "/api/v1/organizations/team/permissions")
			assert.Equal(request.Method, http.MethodGet)
			writer.Header().Set("Content-Type", "application/json")
			if denied {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(`{"message":"permission denied"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"subject":"initial-admin","revision":4,"permissions":["cluster:link"]}`))
		}))
		output, _, err := runApplicationCommand(t, "org", "permissions", "team", "--api-url", server.URL)
		if denied {
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), "permission denied"))
		} else {
			assert.NoErr(err)
			assert.True(strings.Contains(output, "cluster:link"))
		}
		server.Close()
	}
}
