package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestManagementReadinessGatesDependentRequests verifies bootstrap keeps local
// health and diagnostics available while vault-dependent API calls wait.
func TestManagementReadinessGatesDependentRequests(t *testing.T) {
	is := is.New(t)
	input := managementStartupInput(t, "http://127.0.0.1:1")
	server, err := New(context.Background(), input)
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(server.Close()) })
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/livez", http.StatusOK},
		{http.MethodGet, "/api/healthz", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/readyz", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/openapi.yaml", http.StatusOK},
		{http.MethodGet, "/api/docs/", http.StatusOK},
		{http.MethodGet, "/api/docs/swagger-ui-bundle.js", http.StatusOK},
		{http.MethodGet, "/api/docs/swagger-ui.css", http.StatusOK},
		{http.MethodGet, "/api/v1/management/components", http.StatusOK},
		{http.MethodGet, "/api/v1/organizations", http.StatusOK},
		{http.MethodPut, "/api/v1/organizations/acme/clusters/target", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/organizations/acme/spaces/default/environment-groups/backend/versions", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/v1/organizations/acme/spaces/default/environment-groups/backend", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/v1/plans/saved/apply", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/v1/management/components/upgrade", http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		is.Equal(response.Code, test.status)
		if test.status == http.StatusServiceUnavailable && strings.HasPrefix(test.path, "/v1/") {
			is.Equal(response.Header().Get("Retry-After"), "15")
			is.True(strings.Contains(response.Body.String(), "/api/readyz"))
		}
	}
	server.management.mutex.Lock()
	server.management.state = managementState{Phase: "ready", Ready: true, Message: "Management components are ready."}
	server.management.mutex.Unlock()
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/readyz", nil))
	is.Equal(response.Code, http.StatusOK)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/organizations/acme/clusters/target", strings.NewReader("invalid-json")))
	is.Equal(response.Code, http.StatusBadRequest)
}

// TestManagementStartupRetriesBehindLiveHttp verifies installation starts after
// binding, failures remain observable, retry is deterministic, and shutdown
// releases the database even while management setup has not succeeded.
func TestManagementStartupRetriesBehindLiveHttp(t *testing.T) {
	is := is.New(t)
	var calls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","message":"private-diagnostic-marker","code":403}`))
	}))
	defer api.Close()
	input := managementStartupInput(t, api.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := New(ctx, input)
	is.NoErr(err)
	defer server.Close()
	is.Equal(calls.Load(), int64(0))
	server.http.Addr = unusedManagementAddress(t)
	server.telemetry.Addr = unusedManagementAddress(t)
	server.management.retryDelay = 30 * time.Millisecond
	server.management.attemptTimeout = time.Second
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(ctx) }()
	client := http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get("http://" + server.http.Addr + "/api/livez")
		if err == nil {
			is.Equal(response.StatusCode, http.StatusOK)
			is.NoErr(response.Body.Close())
			if snapshot := server.management.snapshot(); snapshot.Attempt >= 2 && snapshot.Phase == "failed" {
				break
			}
		}
		select {
		case err := <-completed:
			t.Fatalf("server exited while setup retried: %v", err)
		case <-deadline.C:
			t.Fatal("management setup did not retry behind a live listener")
		case <-time.After(5 * time.Millisecond):
		}
	}
	is.True(calls.Load() >= 2)
	snapshot := server.management.snapshot()
	is.True(!snapshot.NextRetryAt.IsZero())
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/readyz", nil))
	is.Equal(response.Code, http.StatusServiceUnavailable)
	is.True(!strings.Contains(response.Body.String(), "private-diagnostic-marker"))
	var health struct {
		Status string `json:"status"`
	}
	is.NoErr(json.Unmarshal(response.Body.Bytes(), &health))
	is.Equal(health.Status, "fail")
	cancel()
	select {
	case err := <-completed:
		is.NoErr(err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop failed management setup")
	}
	is.True(server.manager.CheckHealth(context.Background()) != nil)
	reopened, err := New(context.Background(), NewInput{Port: 7833, DataDir: input.DataDir})
	is.NoErr(err)
	is.NoErr(reopened.Close())
}

// TestManagementCloseCancelsAnInFlightAttempt verifies cancellation reaches the
// Kubernetes request and that Close waits for setup before closing SQLite.
func TestManagementCloseCancelsAnInFlightAttempt(t *testing.T) {
	is := is.New(t)
	started := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer api.Close()
	server, err := New(context.Background(), managementStartupInput(t, api.URL))
	is.NoErr(err)
	server.management.start()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("management setup did not contact Kubernetes")
	}
	completed := make(chan error, 1)
	go func() { completed <- server.Close() }()
	select {
	case err := <-completed:
		is.NoErr(err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel management setup")
	}
	is.True(server.manager.CheckHealth(context.Background()) != nil)
	is.True(server.management.snapshot().Phase == "stopped")
}

func managementStartupInput(t *testing.T, address string) NewInput {
	t.Helper()
	path := filepath.Join(t.TempDir(), "management.yaml")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: management\n  cluster:\n    server: %s\ncontexts:\n- name: management\n  context:\n    cluster: management\n    user: management\ncurrent-context: management\nusers:\n- name: management\n  user: {}\n", address)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewInput{Port: 7833, TelemetryPort: 7834, DataDir: t.TempDir(), ManagementEnabled: true, InfrastructureEnabled: true, ManagementKubeconfigPath: path,
		PlatformSettings: platform.Settings{Profile: "dev", OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"},
	}
}

func unusedManagementAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
