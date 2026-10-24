//go:build integration

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestApplicationCliIntegrationConnectionFailures checks the actual binary's
// exit status and recovery guidance when the management API closes a connection.
func TestApplicationCliIntegrationConnectionFailures(t *testing.T) {
	assert := is.New(t)
	binary := os.Getenv("BORQ_APPLICATION_TEST_BINARY")
	if binary == "" {
		t.Skip("run make test-apps-e2e to exercise CLI connection failures")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	directory := t.TempDir()
	var environment []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "HOME=") && !strings.HasPrefix(variable, "XDG_CONFIG_HOME=") {
			environment = append(environment, variable)
		}
	}
	environment = append(environment, "HOME="+directory, "XDG_CONFIG_HOME="+filepath.Join(directory, "config"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		connection, _, err := writer.(http.Hijacker).Hijack()
		assert.NoErr(err)
		assert.NoErr(connection.Close())
	}))
	defer server.Close()
	command := exec.CommandContext(ctx, binary, "org", "create", "testorg", "--api-url", server.URL)
	command.Dir, command.Env = directory, environment
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	err := command.Run()
	var exitError *exec.ExitError
	assert.True(errors.As(err, &exitError))
	assert.Equal(exitError.ExitCode(), 1)
	assert.Equal(output.String(), "")
	for _, hint := range []string{server.URL, "connection closed", "borq management bootstrap local", "borq config show"} {
		assert.True(strings.Contains(diagnostic.String(), hint))
	}
	assert.Equal(requests.Load(), int32(1))
}
