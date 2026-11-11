package cli

import (
	"testing"

	"github.com/matryer/is"
)

// TestLoadManagementConfigFromEnvironment keeps the management API address and
// installation name in typed server configuration without environment whitespace.
func TestLoadManagementConfigFromEnvironment(t *testing.T) {
	assert := is.New(t)
	t.Setenv("BORQ_API_URL", "  https://borq.example.test:7833/  ")
	t.Setenv("BORQ_MANAGEMENT_CLUSTER_NAME", "  custom-management  ")
	t.Setenv("BORQ_INFRASTRUCTURE_ENABLED", "false")
	t.Setenv("BORQ_MANAGEMENT_ENABLED", "false")
	t.Setenv("BORQ_TELEMETRY_PORT", "7834")
	configuration, err := Load()
	assert.NoErr(err)
	assert.Equal(configuration.ApiUrl, "https://borq.example.test:7833/")
	assert.Equal(configuration.ManagementClusterName, "custom-management")
}
