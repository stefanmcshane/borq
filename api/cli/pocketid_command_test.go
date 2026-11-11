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

// TestManagedPocketIdCommands owns CLI scoping, YAML overrides, enrollment output, and operation failure exits.
func TestManagedPocketIdCommands(t *testing.T) {
	assert := is.New(t)
	base := "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid"
	operationStatus := appapi.Succeeded
	wantedApply := "false"
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		methods = append(methods, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case base:
			_ = json.NewEncoder(writer).Encode([]appapi.PocketID{{Name: "identity"}})
		case base + "/identity":
			if request.Method == http.MethodPut {
				var input appapi.PocketIDInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
				assert.True(input.IssuerURL != nil && *input.IssuerURL == "https://id.example.test")
				assert.True(input.AdminEmail != nil && *input.AdminEmail == "owner@example.test")
				assert.True(input.AdminUsername != nil && *input.AdminUsername == "owner")
				assert.True(input.Storage != nil && *input.Storage == "2Gi")
				assert.Equal(request.URL.Query().Get("apply"), wantedApply)
			}
			_ = json.NewEncoder(writer).Encode(appapi.PocketID{Name: "identity", Operation: "work"})
		case base + "/identity/enrollment":
			assert.Equal(request.Method, http.MethodPost)
			_ = json.NewEncoder(writer).Encode(appapi.PocketIDEnrollment{URL: "https://id.example.test/signup/setup-token"})
		case "/api/v1/pocketid-operations/work":
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "work", Status: operationStatus, Message: "rollover failed"})
		case "/api/v1/organizations/team/spaces/dev/configuration":
			var input appapi.SpaceConfiguration
			assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
			assert.True(input.ManagedAddons != nil && input.ManagedAddons.Pocketid != nil)
			assert.Equal((*input.ManagedAddons.Pocketid)[0].Name, "identity")
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "aggregate", Status: appapi.Succeeded})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	flags := []string{"--api-url", server.URL, "--organization", "team", "--space", "dev"}
	file := filepath.Join(t.TempDir(), "identity.yaml")
	assert.NoErr(os.WriteFile(file, []byte("issuerUrl: https://id.example.test\nadminUsername: owner\nadminEmail: owner@example.test\nstorage: 1Gi\n"), 0600))
	create := append([]string{"addons", "managed", "pocketid", "create", "identity", "-f", file, "--storage", "2Gi"}, flags...)
	output, _, err := runApplicationCommand(t, create...)
	assert.NoErr(err)
	assert.True(strings.Contains(output, "succeeded"))
	wantedApply = "true"
	apply := append([]string{}, create...)
	apply[3] = "apply"
	_, _, err = runApplicationCommand(t, apply...)
	assert.NoErr(err)
	wantedApply = "false"
	operationStatus = appapi.Failed
	_, _, err = runApplicationCommand(t, create...)
	assert.True(err != nil && strings.Contains(err.Error(), "rollover failed"))
	for _, action := range []string{"get", "delete", "enroll", "operation"} {
		arguments := []string{"am", "pocketid", action, "identity"}
		if action == "operation" {
			arguments[3] = "work"
			arguments = append(arguments, "--api-url", server.URL)
		} else {
			arguments = append(arguments, flags...)
		}
		if action == "delete" {
			arguments = append(arguments, "--wait=false")
		}
		output, _, err = runApplicationCommand(t, arguments...)
		assert.NoErr(err)
		if action == "enroll" {
			assert.Equal(output, "https://id.example.test/signup/setup-token\n")
		}
	}
	output, _, err = runApplicationCommand(t, append([]string{"am", "pocketid", "list"}, flags...)...)
	assert.NoErr(err)
	assert.True(strings.Contains(output, "identity"))
	assert.NoErr(os.WriteFile(file, []byte("managedAddons:\n  pocketid:\n    - name: identity\n      issuerUrl: https://id.example.test\n      adminUsername: owner\n      adminEmail: owner@example.test\n"), 0600))
	_, _, err = runApplicationCommand(t, append([]string{"am", "pocketid", "apply", "-f", file}, flags...)...)
	assert.NoErr(err)
	before := len(methods)
	assert.NoErr(os.WriteFile(file, []byte("issuerUrl: https://id.example.test\npassword: private-password\n"), 0600))
	output, diagnostic, err := runApplicationCommand(t, append([]string{"am", "pocketid", "create", "identity", "-f", file}, flags...)...)
	assert.True(err != nil)
	assert.True(!strings.Contains(output+diagnostic+err.Error(), "private-password"))
	assert.Equal(len(methods), before)
}
