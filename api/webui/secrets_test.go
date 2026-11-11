package webui

import (
	"encoding/json"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSecretActions owns conversion of key/value forms into typed JSON and the
// delete version guard; real manager integration owns the resulting secret lifecycle.
func TestSecretActions(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "delete"}[deleting], func(t *testing.T) {
			assert := is.New(t)
			calls := 0
			web, err := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveActionPageFixture(w, r) {
					return
				}
				calls++
				assert.Equal(r.URL.Path, "/api/v1/organizations/team/spaces/dev/secrets/github-packages")
				if deleting {
					assert.Equal(r.Method, http.MethodDelete)
					assert.Equal(r.URL.Query().Get("version"), "2")
					_ = json.NewEncoder(w).Encode(appapi.DeletedSecret{Name: "github-packages"})
					return
				}
				assert.Equal(r.Method, http.MethodPut)
				var body appapi.PutSecretInput
				assert.NoErr(json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(string(body.Type), "registry")
				assert.Equal(body.Data, map[string]string{"server": "ghcr.io", "username": "user", "password": "private-token"})
				assert.Equal(*body.BaseVersion, 2)
				_ = json.NewEncoder(w).Encode(appapi.Secret{Name: "github-packages", Type: appapi.SecretTypeRegistry, Data: map[string]string{"username": "user"}, Version: 3})
			}))
			assert.NoErr(err)
			action := "put-secret"
			if deleting {
				action = "delete-secret"
			}
			response := postForm(web, url.Values{"action": {action}, "organization": {"team"}, "space": {"dev"}, "name": {"github-packages"}, "secretType": {"registry"}, "baseVersion": {"2"}, "format": {"fields"}, "variableName": {"server", "username", "password"}, "variableValue": {"ghcr.io", "user", "private-token"}})
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(calls, 1)
			assert.True(!strings.Contains(response.Body.String(), "private-token"))
		})
	}
}
