package openbao_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

// TestDatabaseConnectionEnrollment protects TLS verification and keeps opaque rotated root passwords out of later updates.
func TestDatabaseConnectionEnrollment(t *testing.T) {
	check := is.New(t)
	stored := false
	writes := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		check.Equal(request.URL.Path, "/v1/database-target/config/pg-id")
		if request.Method == http.MethodGet {
			if !stored {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprint(writer, `{"data":{"allowed_roles":["pg-id"],"connection_details":{"username":"borq_openbao","connection_url":"redacted-existing-url"}}}`)
			return
		}
		var body map[string]any
		check.NoErr(json.NewDecoder(request.Body).Decode(&body))
		writes = append(writes, body)
		stored = true
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
	check.NoErr(err)
	input := openbao.DatabaseConnectionInput{ConnectionUrl: "postgresql://{{username}}:{{password}}@pg.example:5432/postgres?sslmode=verify-full&sslrootcert=/openbao/postgres-ca/id.crt", Username: "borq_openbao", BootstrapPassword: "one-time-password", AllowedRole: "pg-id", RootCaPath: "/openbao/postgres-ca/id.crt"}
	check.NoErr(client.ConfigureDatabaseConnection(t.Context(), "database-target", "pg-id", input))
	check.Equal(writes[0]["password"], "one-time-password")
	check.Equal(writes[0]["password_authentication"], "scram-sha-256")
	check.Equal(writes[0]["verify_connection"], true)
	input.BootstrapPassword = "must-not-restore-this-password"
	check.NoErr(client.ConfigureDatabaseConnection(t.Context(), "database-target", "pg-id", input))
	_, exists := writes[1]["password"]
	check.True(!exists)
	input.ConnectionUrl = strings.ReplaceAll(input.ConnectionUrl, "verify-full", "require")
	check.True(client.ConfigureDatabaseConnection(t.Context(), "database-target", "pg-id", input) != nil)
	check.Equal(len(writes), 2)
}

// TestDatabaseStaticCredentials decodes actual engine metadata and prevents accidental credential serialization.
func TestDatabaseStaticCredentials(t *testing.T) {
	check := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/database-target/static-creds/pg-id":
			_, _ = fmt.Fprint(writer, `{"data":{"username":"app","password":"secret-value","last_vault_rotation":"2026-10-24T12:00:00Z","rotation_period":86400,"ttl":10}}`)
		case "/v1/database-target/static-roles/pg-id":
			if request.Method == http.MethodGet {
				_, _ = fmt.Fprint(writer, `{"data":{"db_name":"pg-id","username":"app","rotation_period":86400}}`)
				return
			}
			var body map[string]any
			check.NoErr(json.NewDecoder(request.Body).Decode(&body))
			check.Equal(body["rotation_period"], float64(86400))
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
	check.NoErr(err)
	credentials, err := client.ReadStaticCredentials(t.Context(), "database-target", "pg-id")
	check.NoErr(err)
	check.Equal(credentials.Username, "app")
	check.Equal(credentials.Password, "secret-value")
	check.Equal(credentials.RotationPeriod, 24*time.Hour)
	check.Equal(credentials.LastRotation, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	encoded, err := json.Marshal(credentials)
	check.NoErr(err)
	check.True(!strings.Contains(string(encoded), "secret-value"))
	role, err := client.ReadStaticRole(t.Context(), "database-target", "pg-id")
	check.NoErr(err)
	check.Equal(role, openbao.StaticRole{Connection: "pg-id", Username: "app", RotationPeriod: 24 * time.Hour})
	check.NoErr(client.PutStaticRole(t.Context(), "database-target", "pg-id", role))
	role.RotationPeriod = time.Second
	check.True(client.PutStaticRole(t.Context(), "database-target", "pg-id", role) != nil)
}

// TestDatabaseErrorsRedactEngineResponses protects control credentials and SQL diagnostics at the adapter boundary.
func TestDatabaseErrorsRedactEngineResponses(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			check := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(status)
				_, _ = fmt.Fprint(writer, `{"errors":["secret-password SQL credential"]}`)
			}))
			defer server.Close()
			client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
			check.NoErr(err)
			for _, operation := range []func() error{
				func() error {
					_, err := client.ReadStaticCredentials(t.Context(), "database-target", "pg-id")
					return err
				},
				func() error { return client.RotateDatabaseRoot(t.Context(), "database-target", "pg-id") },
				func() error { return client.RotateStaticRole(t.Context(), "database-target", "pg-id") },
				func() error { return client.DeleteKvMetadata(t.Context(), "secret", "managed-postgres/id") },
			} {
				err := operation()
				check.True(err != nil)
				check.True(!strings.Contains(err.Error(), "secret-password"))
				check.True(!strings.Contains(err.Error(), server.URL))
				if status == http.StatusNotFound {
					check.True(errors.Is(err, openbao.ErrNotFound))
				}
			}
		})
	}
}
