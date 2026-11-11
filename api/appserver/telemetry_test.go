package appserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestPublicTelemetryListenerExcludesAdministrativeRoutes keeps the public listener limited to authenticated writes.
func TestPublicTelemetryListenerExcludesAdministrativeRoutes(t *testing.T) {
	is := is.New(t)
	kubeconfig := filepath.Join(t.TempDir(), "management.yaml")
	is.NoErr(os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: management
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
users:
- name: management
  user:
    token: unused-test-credential
contexts:
- name: management
  context:
    cluster: management
    user: management
current-context: management
`), 0600))
	server, err := New(context.Background(), NewInput{Port: 7833, DataDir: t.TempDir(), InfrastructureEnabled: true,
		ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"}})
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(server.Close()) })
	is.True(server.telemetry != nil)
	for _, path := range []string{"/", "/api/healthz", "/api/v1/organizations", "/api/v1/management/components", "/api/openapi.yaml", "/api/docs/"} {
		response := httptest.NewRecorder()
		server.telemetry.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		is.Equal(response.Code, http.StatusNotFound)
	}
	for _, path := range []string{"loki/api/v1/push", "api/v1/write"} {
		response := httptest.NewRecorder()
		server.telemetry.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ingest/0123456789abcdef0123456789abcdef/"+path, nil))
		is.Equal(response.Code, http.StatusUnauthorized)
	}
}
