package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestManagementBackupStatus owns the logged-in API route and human/JSON output
// contract. Export success must not obscure a missing database in CLI output.
func TestManagementBackupStatus(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/management/backups/status")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"checkedAt":"2026-11-05T12:00:00Z","state":"attention","components":[{"kind":"sqlite","name":"team/dev/web","state":"missing","message":"No complete backup was found."},{"kind":"offsite","name":"export","state":"available","lastSuccess":"2026-11-05T11:00:00Z","message":"Export completed."}]}`))
	}))
	defer server.Close()
	for _, output := range []string{"cli", "json"} {
		stdout, _, err := runApplicationCommand(t, "management", "backups", "status", "--api-url", server.URL, "--output", output)
		assert.NoErr(err)
		if output == "json" {
			var result map[string]any
			assert.NoErr(json.Unmarshal([]byte(stdout), &result))
			assert.Equal(result["state"], "attention")
			continue
		}
		assert.True(strings.Contains(stdout, "team/dev/web"))
		assert.True(strings.Contains(stdout, "missing"))
		assert.True(strings.Contains(stdout, "No complete backup was found."))
		assert.True(strings.Contains(stdout, "2026-11-05T11:00:00Z"))
		assert.True(strings.Contains(stdout, "Offsite export success does not confirm"))
	}
	_, _, err := runApplicationCommand(t, "management", "backups", "status", "--output", "invalid")
	assert.True(err != nil && strings.Contains(err.Error(), "--output must be"))
}
