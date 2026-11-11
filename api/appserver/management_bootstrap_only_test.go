package appserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestManagementBootstrapOnlyAccess keeps private API access available while
// aggregate health waits for OpenBao, without fabricating controller success.
func TestManagementBootstrapOnlyAccess(t *testing.T) {
	assert := is.New(t)
	kube := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer kube.Close()
	input := managementStartupInput(t, kube.URL)
	input.ManagementBootstrapOnly = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server, err := New(ctx, input)
	assert.NoErr(err)
	defer func() { _ = server.Close() }()
	server.http.Addr = unusedManagementAddress(t)
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(ctx) }()
	client := http.Client{Timeout: 100 * time.Millisecond}
	origin := "http://" + server.http.Addr
	deadline := time.After(5 * time.Second)
	for {
		response, err := client.Get(origin + "/api/readyz")
		if err == nil {
			assert.Equal(response.StatusCode, http.StatusOK)
			var health struct {
				Status string `json:"status"`
				Checks []struct {
					Name  string `json:"name"`
					Phase string `json:"phase"`
				} `json:"checks"`
			}
			assert.NoErr(json.NewDecoder(response.Body).Decode(&health))
			assert.NoErr(response.Body.Close())
			assert.Equal(health.Status, "success")
			break
		}
		select {
		case err := <-completed:
			t.Fatalf("bootstrap server exited before access was available: %v", err)
		case <-deadline:
			t.Fatal("bootstrap server did not become available")
		case <-time.After(5 * time.Millisecond):
		}
	}
	response, err := client.Get(origin + "/api/healthz")
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusServiceUnavailable)
	assert.NoErr(response.Body.Close())
	token, err := os.ReadFile(filepath.Join(input.DataDir, "admin-token"))
	assert.NoErr(err)
	for _, authenticated := range []bool{false, true} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v1/management/configuration", nil)
		assert.NoErr(err)
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		}
		response, err := client.Do(request)
		assert.NoErr(err)
		body, err := io.ReadAll(response.Body)
		assert.NoErr(err)
		assert.NoErr(response.Body.Close())
		if authenticated {
			assert.Equal(response.StatusCode, http.StatusOK)
			var configuration map[string]any
			assert.NoErr(json.Unmarshal(body, &configuration))
			assert.True(configuration["network"] == nil)
			assert.True(configuration["borqPublicUrl"] == nil)
		} else {
			assert.Equal(response.StatusCode, http.StatusUnauthorized)
		}
		assert.True(!strings.Contains(string(body), strings.TrimSpace(string(token))))
	}
	cancel()
	select {
	case err := <-completed:
		assert.NoErr(err)
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap server did not stop")
	}
}
