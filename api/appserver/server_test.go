package appserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestServerStartsWithoutClusters verifies the SQLite application API starts
// independently of legacy infrastructure services and serves authenticated routes.
func TestServerStartsWithoutClusters(t *testing.T) {
	is := is.New(t)
	directory := t.TempDir()
	server, err := New(context.Background(), NewInput{Version: "0.0.1-rc42", Port: 7833, DataDir: directory})
	is.NoErr(err)
	defer func() { is.NoErr(server.Close()) }()
	_, err = os.Stat(filepath.Join(directory, "borq.sqlite"))
	is.NoErr(err)
	for _, input := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/assets/style.css", http.StatusOK},
		{http.MethodGet, "/assets/htmx.min.js", http.StatusNotFound},
		{http.MethodGet, "/api/ui/page", http.StatusOK},
		{http.MethodGet, "/ui/application", http.StatusNotFound},
		{http.MethodPost, "/ui/actions", http.StatusNotFound},
		{http.MethodGet, "/api/ui/missing", http.StatusNotFound},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/healthz", http.StatusOK},
		{http.MethodGet, "/api/readyz", http.StatusOK},
		{http.MethodGet, "/api/livez", http.StatusOK},
		{http.MethodPost, "/api/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodGet, "/api/openapi.yaml", http.StatusOK},
		{http.MethodGet, "/v1/organizations", http.StatusNotFound},
		{http.MethodGet, "/openapi.yaml", http.StatusNotFound},
		{http.MethodGet, "/mcp", http.StatusNotFound},
	} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(input.method, input.path, nil)
		authorizeAdminTestRequest(t, directory, request)
		server.http.Handler.ServeHTTP(writer, request)
		is.Equal(writer.Code, input.status)
		if input.path == "/" && input.method == http.MethodGet {
			is.Equal(writer.Header().Get("Content-Type"), "text/html; charset=utf-8")
		}
		if input.path == "/api/ui/page" {
			is.Equal(writer.Header().Get("Content-Type"), "application/json")
			is.Equal(writer.Header().Get("Cache-Control"), "no-store")
		}
		if (input.path == "/api/healthz" || input.path == "/api/readyz" || input.path == "/api/livez") && input.method == http.MethodGet {
			var report struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			is.NoErr(json.Unmarshal(writer.Body.Bytes(), &report))
			is.Equal(report.Status, "success")
			is.Equal(report.Version, "0.0.1-rc42")
		}
	}
}

// TestServerValidatesRuntimeInputs rejects invalid listeners and absent storage
// before opening a database or starting background workers.
func TestServerValidatesRuntimeInputs(t *testing.T) {
	for _, input := range []NewInput{{Port: 0, DataDir: t.TempDir()}, {Port: 65536, DataDir: t.TempDir()}, {Port: 7833}, {Port: 7833, DataDir: t.TempDir(), RolloutTimeout: -time.Second}, {Port: 7833, DataDir: t.TempDir(), ManagementEnabled: true}} {
		is := is.New(t)
		_, err := New(context.Background(), input)
		is.True(err != nil)
	}
}

// TestHealthFailsWhenStateCloses prevents a live HTTP listener from hiding a failed storage connection.
func TestHealthFailsWhenStateCloses(t *testing.T) {
	assert := is.New(t)
	server, err := New(context.Background(), NewInput{Port: 7833, DataDir: t.TempDir()})
	assert.NoErr(err)
	assert.NoErr(server.manager.Close())
	t.Cleanup(func() { _ = server.Close() })
	for _, path := range []string{"/api/healthz", "/api/readyz", "/api/livez"} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(response.Code, http.StatusServiceUnavailable)
	}
}

// TestServeDrainsAndClosesState verifies cancellation stops the HTTP runtime and releases the database lock.
func TestServeDrainsAndClosesState(t *testing.T) {
	assert := is.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	address := listener.Addr().String()
	assert.NoErr(listener.Close())
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server, err := New(ctx, NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	server.http.Addr = address
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(ctx) }()
	client := http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.After(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/api/healthz")
		if err == nil {
			assert.Equal(response.StatusCode, http.StatusOK)
			assert.NoErr(response.Body.Close())
			break
		}
		select {
		case err := <-completed:
			t.Fatalf("server exited during startup: %v", err)
		case <-deadline:
			t.Fatal("server did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-completed:
		assert.NoErr(err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	assert.True(server.manager.CheckHealth(context.Background()) != nil)
	reopened, err := New(context.Background(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	assert.NoErr(reopened.Close())
}

// TestServeReportsListenerFailure releases durable state when another process owns the requested address.
func TestServeReportsListenerFailure(t *testing.T) {
	assert := is.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	t.Cleanup(func() { _ = listener.Close() })
	server, err := New(context.Background(), NewInput{Port: 7833, DataDir: t.TempDir()})
	assert.NoErr(err)
	server.http.Addr = listener.Addr().String()
	assert.True(server.Serve(context.Background()) != nil)
	assert.True(server.manager.CheckHealth(context.Background()) != nil)
}

// authorizeAdminTestRequest uses the real credential generated by server startup
// so routing and lifecycle tests still pass through production authentication.
func authorizeAdminTestRequest(t *testing.T, directory string, request *http.Request) {
	t.Helper()
	token, err := os.ReadFile(filepath.Join(directory, "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
}
