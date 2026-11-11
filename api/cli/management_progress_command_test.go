package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestManagementApplyProgress retains the current resource and operation ID on
// timeout and terminal failure, with progress on stderr and no partial JSON result.
func TestManagementApplyProgress(t *testing.T) {
	for _, outcome := range []string{"timeout", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			keys := filepath.Join(t.TempDir(), "token")
			assert.NoErr(os.WriteFile(keys, []byte(strings.Repeat("a", 64)), 0600))
			file := filepath.Join(t.TempDir(), "management.yaml")
			assert.NoErr(os.WriteFile(file, []byte("organizations: []\n"), 0600))
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPut {
					writer.WriteHeader(http.StatusAccepted)
					_, _ = writer.Write([]byte(`{"id":"setup-123","status":"queued","message":"accepted"}`))
					return
				}
				assert.Equal(request.URL.Path, "/api/v1/management/operations/setup-123")
				status := "running"
				if outcome == "failed" {
					status = "failed"
				}
				_, _ = writer.Write([]byte(`{"id":"setup-123","status":"` + status + `","phase":"management","message":"verifying Tailscale management HTTPS connectivity","resources":[{"kind":"Job","namespace":"borq-system","name":"borq-network-probe-current","message":"waiting for management HTTPS verification"}]}`))
			}))
			defer server.Close()
			output, diagnostics, err := runApplicationCommand(t, "management", "apply", "--api-url", server.URL, "--token-file", keys, "-f", file, "--timeout", "150ms")
			assert.True(err != nil)
			for _, want := range []string{"setup-123", "verifying Tailscale", "borq-network-probe-current"} {
				assert.True(strings.Contains(err.Error(), want))
			}
			assert.True(strings.Contains(diagnostics, "operation setup-123"))
			assert.Equal(output, "")
			if outcome == "timeout" {
				assert.True(errors.Is(err, context.DeadlineExceeded))
			}
		})
	}
}
