package cli

import (
	"encoding/json"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretsCommands owns typed JSON file parsing and scoped CRUD transport;
// registry values returned by the API never include the submitted write-only token.
func TestSecretsCommands(t *testing.T) {
	assert := is.New(t)
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		secret := appapi.Secret{Name: "github-packages", Type: appapi.SecretTypeRegistry, Data: map[string]string{"username": "user"}, Version: 2}
		if r.Method == http.MethodPut {
			var body appapi.PutSecretInput
			assert.NoErr(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(string(body.Type), "registry")
			assert.Equal(body.Data, map[string]string{"server": "ghcr.io", "username": "user", "password": "write-only-token"})
		}
		if r.Method == http.MethodDelete {
			_ = json.NewEncoder(w).Encode(appapi.DeletedSecret{Name: "github-packages"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			_ = json.NewEncoder(w).Encode([]appapi.Secret{secret})
			return
		}
		_ = json.NewEncoder(w).Encode(secret)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "registry.json")
	assert.NoErr(os.WriteFile(file, []byte(`{"type":"registry","data":{"server":"ghcr.io","username":"user","password":"write-only-token"}}`), 0600))
	flags := []string{"--server", server.URL, "--organization", "team", "--space", "dev"}
	for _, args := range [][]string{{"secrets", "put", "github-packages", "-f", file}, {"secrets", "get", "github-packages"}, {"secrets", "list"}, {"secrets", "delete", "github-packages", "--version", "2"}} {
		output, diagnostics, err := runApplicationCommand(t, append(args, flags...)...)
		assert.NoErr(err)
		assert.True(!strings.Contains(output+diagnostics, "write-only-token"))
	}
	base := "/api/v1/organizations/team/spaces/dev/secrets"
	assert.Equal(requests, []string{"PUT " + base + "/github-packages", "GET " + base + "/github-packages/versions/latest", "GET " + base, "DELETE " + base + "/github-packages?version=2"})
}
