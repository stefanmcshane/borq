package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestManagementSecretsCommands owns profile and org selection, complete JSON/YAML
// submissions, explicit version reads and write output that does not echo values.
func TestManagementSecretsCommands(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var mutex sync.Mutex
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		secret := appapi.Secret{Name: "smtp", Type: appapi.SecretTypeSecretGroup, Data: map[string]string{"PASSWORD": "private-cli-secret", "EMPTY": ""}, Version: 2}
		if request.Method == http.MethodPut {
			var body appapi.PutSecretInput
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				writer.WriteHeader(400)
				return
			}
			assert.Equal(string(body.Type), "secret-group")
			assert.Equal(body.Data, secret.Data)
			assert.True(body.BaseVersion != nil && *body.BaseVersion == 0)
		}
		if strings.HasSuffix(request.URL.Path, "/management-secrets") {
			_ = json.NewEncoder(writer).Encode([]appapi.Secret{secret})
			return
		}
		_ = json.NewEncoder(writer).Encode(secret)
	}))
	defer server.Close()
	_, _, err := runConfiguredApplicationCommand("config", "set", "--server", server.URL, "--organization", "selected")
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "smtp.yaml")
	assert.NoErr(os.WriteFile(file, []byte("type: secret-group\nbaseVersion: 0\ndata:\n  PASSWORD: private-cli-secret\n  EMPTY: ''\n"), 0600))
	for _, arguments := range [][]string{
		{"management", "secrets", "put", "smtp", "--org", "team", "-f", file},
		{"management", "secrets", "get", "smtp", "--organization", "team", "--version", "2"},
		{"management", "secrets", "list"},
	} {
		output, diagnostics, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
		assert.True(strings.Contains(output, "smtp"))
		if arguments[2] == "put" {
			assert.True(!strings.Contains(output+diagnostics, "private-cli-secret"))
		} else {
			assert.True(strings.Contains(output, "private-cli-secret"))
		}
	}
	for _, source := range []string{
		`{"type":"registry","data":{"password":"private-cli-secret"}}`,
		`{"type":"secret-group","data":{"PASSWORD":123}}`,
		"type: secret-group\ndata: {}\ndata: {}\n",
		"type: secret-group\ndata: {}\n---\ntype: secret-group\ndata: {}\n",
	} {
		assert.NoErr(os.WriteFile(file, []byte(source), 0600))
		output, diagnostics, err := runConfiguredApplicationCommand("management", "secrets", "put", "smtp", "--org", "team", "-f", file)
		assert.True(err != nil)
		assert.True(!strings.Contains(output+diagnostics+err.Error(), "private-cli-secret"))
	}
	_, _, err = runConfiguredApplicationCommand("management", "secrets", "list", "--org", "other", "--organization", "team")
	assert.True(err != nil && strings.Contains(err.Error(), "must select the same organization"))
	mutex.Lock()
	defer mutex.Unlock()
	assert.Equal(requests, []string{"PUT /api/v1/organizations/team/management-secrets/smtp", "GET /api/v1/organizations/team/management-secrets/smtp/versions/2", "GET /api/v1/organizations/selected/management-secrets"})
}
