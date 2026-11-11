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

// TestRegistrySpaceApply routes auth-only YAML through the space configuration
// boundary without sending credentials to the ordinary application plan endpoint.
func TestRegistrySpaceApply(t *testing.T) {
	assert := is.New(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(request.Method, http.MethodPut)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/configuration")
		var input appapi.SpaceConfiguration
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.True(input.Registries != nil && (*input.Registries)["ghcr.io"].Username != nil)
		assert.Equal(*(*input.Registries)["ghcr.io"].Username, "github-user")
		assert.Equal(*(*input.Registries)["ghcr.io"].Token, "private-registry-token")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "registry-operation", Status: appapi.Succeeded})
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "space.yaml")
	assert.NoErr(os.WriteFile(file, []byte("registries:\n  ghcr.io:\n    username: github-user\n    token: private-registry-token\n"), 0600))
	output, diagnostics, err := runApplicationCommand(t, "apply", "-f", file, "--api-url", server.URL, "--organization", "team", "--space", "dev")
	assert.NoErr(err)
	assert.Equal(calls, 1)
	assert.True(!strings.Contains(output+diagnostics, "private-registry-token"))
}
