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

// TestManagedPostgresCommands owns the public paths, shared fragment YAML, explicit secret output and failure exit behavior.
func TestManagedPostgresCommands(t *testing.T) {
	assert := is.New(t)
	status := appapi.Succeeded
	base := "/api/v1/organizations/team/spaces/dev"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case base + "/managed-addons/postgres/Uppercase":
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "PostgreSQL names must contain 1 to 47 lowercase letters, digits or hyphens, and start and end with a letter or digit."})
		case base + "/managed-addons/postgres/database":
			if request.Method == http.MethodPut {
				var input appapi.PostgresInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
				assert.True(input.Storage != nil && *input.Storage == "2Gi")
				assert.Equal(request.URL.Query().Get("apply"), "false")
			}
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Postgres{Name: "database", Operation: "operation"})
		case "/api/v1/postgres-operations/operation":
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation", Status: status, Message: "database operation failed"})
		case base + "/managed-addons/postgres/database/env":
			_ = json.NewEncoder(writer).Encode(appapi.PostgresEnv{PGPASSWORD: "x'$(touch /tmp/never)", PGHOST: "database.example", DATABASEURL: "postgres://private"})
		case base + "/configuration":
			var input appapi.SpaceConfiguration
			assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
			assert.True(input.ManagedAddons != nil && input.ManagedAddons.Postgres != nil)
			assert.Equal((*input.ManagedAddons.Postgres)[0].Name, "database")
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "aggregate", Status: appapi.Succeeded})
		case base + "/managed-addons/postgres/restored/restore":
			var input appapi.PostgresRestoreInput
			assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
			assert.Equal(input.BackupID, "approved-backup")
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Postgres{Name: "restored", Operation: "operation"})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	flags := []string{"--api-url", server.URL, "--organization", "team", "--space", "dev"}
	arguments := append([]string{"am", "postgres", "create", "database", "--storage", "2Gi"}, flags...)
	output, _, err := runApplicationCommand(t, arguments...)
	assert.NoErr(err)
	assert.True(strings.Contains(output, "succeeded"))
	_, _, nameError := runApplicationCommand(t, append([]string{"am", "postgres", "create", "Uppercase", "--wait=false"}, flags...)...)
	assert.True(nameError != nil && strings.Contains(nameError.Error(), "lowercase letters"))
	status = appapi.Failed
	_, _, err = runApplicationCommand(t, arguments...)
	assert.True(err != nil && strings.Contains(err.Error(), "database operation failed"))
	output, _, err = runApplicationCommand(t, append([]string{"addons", "managed", "postgres", "env", "database", "--output", "json"}, flags...)...)
	assert.NoErr(err)
	var env appapi.PostgresEnv
	assert.NoErr(json.Unmarshal([]byte(output), &env))
	assert.Equal(env.PGPASSWORD, "x'$(touch /tmp/never)")
	output, _, err = runApplicationCommand(t, append([]string{"am", "postgres", "env", "database"}, flags...)...)
	assert.NoErr(err)
	assert.True(strings.Contains(output, `PGPASSWORD='x'"'"'$(touch /tmp/never)'`))
	file := filepath.Join(t.TempDir(), "database.yaml")
	assert.NoErr(os.WriteFile(file, []byte("managedAddons:\n  postgres:\n    - name: database\n"), 0600))
	_, _, err = runApplicationCommand(t, append([]string{"am", "postgres", "apply", "-f", file}, flags...)...)
	assert.NoErr(err)
	_, _, err = runApplicationCommand(t, append([]string{"apply", "-f", file}, flags...)...)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(file, []byte("managedAddons:\n  postgres:\n    - name: database\n      private-token: private-token\n"), 0600))
	output, diagnostic, err := runApplicationCommand(t, append([]string{"am", "postgres", "apply", "-f", file}, flags...)...)
	assert.True(err != nil && strings.Contains(err.Error(), "Passwords are generated automatically."))
	assert.True(!strings.Contains(output+diagnostic+err.Error(), "private-token"))
	_, _, err = runApplicationCommand(t, append([]string{"am", "postgres", "backup", "restore", "restored", "--backup", "approved-backup", "--wait=false"}, flags...)...)
	assert.NoErr(err)
}

// TestEmptySpaceApplyPreservesApplications sends empty named collections through shared apply semantics.
func TestEmptySpaceApplyPreservesApplications(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/configuration")
		var input appapi.SpaceConfiguration
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.True(input.Apps != nil && len(*input.Apps) == 0)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "empty", Status: appapi.Succeeded})
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "apps.yaml")
	assert.NoErr(os.WriteFile(file, []byte("apps: []\n"), 0600))
	_, _, err := runApplicationCommand(t, "apply", "-f", file, "--api-url", server.URL, "--organization", "team", "--space", "dev")
	assert.NoErr(err)
}

// TestManagementCliDocument enforces the management document boundary and keeps component upgrades under apply.
func TestManagementCliDocument(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/management/configuration")
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`{"s3":{"bucket":"shared","region":"us-east-1","pathStyle":true,"credentialsRef":"vault:shared/1"}}`))
			return
		}
		var input appapi.ManagementConfiguration
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.True(input.S3 != nil && input.S3.SecretKey != nil)
		assert.Equal(*input.S3.SecretKey, "private-secret")
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "aggregate", Status: appapi.Succeeded})
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(file, []byte("s3:\n  region: us-east-1\n  bucket: shared\n  pathStyle: true\n  accessKey: private-access\n  secretKey: private-secret\n"), 0600))
	output, _, err := runApplicationCommand(t, "management", "apply", "-f", file, "--api-url", server.URL)
	assert.NoErr(err)
	assert.True(!strings.Contains(output, "private-secret"))
	output, _, err = runApplicationCommand(t, "management", "export", "--api-url", server.URL)
	assert.NoErr(err)
	assert.True(strings.Contains(output, "credentialsRef"))
	assert.True(!strings.Contains(output, "private-secret"))
	for _, source := range []string{"null", "{}\n---\n{}", "s3:\n  bucket: one\n  bucket: two", "version: 1"} {
		assert.NoErr(os.WriteFile(file, []byte(source), 0600))
		_, _, err = runApplicationCommand(t, "management", "apply", "-f", file, "--api-url", server.URL)
		assert.True(err != nil)
	}
}
