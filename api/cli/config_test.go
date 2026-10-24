package cli

import (
	"testing"

	"github.com/matryer/is"
)

// TestLoadApiUrlFromEnvironment ensures the advertised management API address
// reaches typed server configuration with surrounding environment whitespace removed.
func TestLoadApiUrlFromEnvironment(t *testing.T) {
	assert := is.New(t)
	t.Setenv("BORQ_API_URL", "  https://borq.example.test:7833/  ")
	t.Setenv("BORQ_INFRASTRUCTURE_ENABLED", "false")
	t.Setenv("BORQ_MANAGEMENT_ENABLED", "false")
	t.Setenv("BORQ_TELEMETRY_PORT", "7834")
	configuration, err := Load()
	assert.NoErr(err)
	assert.Equal(configuration.ApiUrl, "https://borq.example.test:7833/")
}
