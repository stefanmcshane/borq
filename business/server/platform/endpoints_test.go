package platform

import (
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestManagedEndpointDefaults derives target-reachable private service origins
// independently of the advertised API's scheme, port, and reverse proxy prefix.
func TestManagedEndpointDefaults(t *testing.T) {
	for _, profile := range []string{"dev", "production"} {
		for _, input := range []struct{ api, vault, telemetry string }{
			{"http://192.0.2.10:31833", "http://192.0.2.10:30200", "http://192.0.2.10:31834"},
			{"https://management.test/control/borq/", "http://management.test:30200", "http://management.test:31834"},
			{"https://[2001:db8::10]:8443/borq", "http://[2001:db8::10]:30200", "http://[2001:db8::10]:31834"},
		} {
			t.Run(profile+"/"+input.api, func(t *testing.T) {
				assert := is.New(t)
				settings := Settings{Profile: profile, ApiUrl: input.api}
				assert.NoErr(ValidateServerSettings(settings))
				resolved, err := targetSettings(TargetInput{ID: "cluster-id", Settings: settings})
				assert.NoErr(err)
				assert.Equal(resolved.OpenBaoURL, input.vault)
				assert.Equal(resolved.TelemetryURL, input.telemetry)
				assert.Equal(resolved.LokiURL, input.telemetry+"/api/v1/ingest/cluster-id/loki/api/v1/push")
				assert.Equal(resolved.PrometheusURL, input.telemetry+"/api/v1/ingest/cluster-id/api/v1/write")
				alloy, err := renderComponentValues(targetCatalog(false)[3], valuesInput{Settings: resolved, ClusterID: "cluster-id"})
				assert.NoErr(err)
				content := alloy["alloy"].(map[string]any)["configMap"].(map[string]any)["content"].(string)
				assert.True(strings.Contains(content, resolved.LokiURL))
				assert.True(strings.Contains(content, resolved.PrometheusURL))
			})
		}
	}
}

// TestManagedEndpointOverrides preserves explicitly routed services and requires
// complete specific telemetry overrides rather than silently mixing endpoints.
func TestManagedEndpointOverrides(t *testing.T) {
	assert := is.New(t)
	settings := Settings{ApiUrl: "https://management.test/api", OpenBaoURL: "https://vault.test", TelemetryURL: "https://ingest.test"}
	resolved, err := normalizeSettings(settings)
	assert.NoErr(err)
	assert.Equal(resolved.OpenBaoURL, settings.OpenBaoURL)
	assert.Equal(resolved.TelemetryURL, settings.TelemetryURL)
	assert.NoErr(ValidateServerSettings(settings))
	settings = Settings{ApiUrl: "https://management.test", LokiURL: "https://logs.test/loki/api/v1/push", PrometheusURL: "https://metrics.test/api/v1/write"}
	resolved, err = normalizeSettings(settings)
	assert.NoErr(err)
	assert.Equal(resolved.TelemetryURL, "")
	assert.Equal(resolved.LokiURL, settings.LokiURL)
	assert.Equal(resolved.PrometheusURL, settings.PrometheusURL)
	assert.NoErr(ValidateServerSettings(settings))
	settings.PrometheusURL = ""
	assert.True(ValidateServerSettings(settings) != nil)
}

// TestManagedEndpointApiValidation rejects credentials and ambiguous API URLs
// even when explicit service overrides would otherwise mask the invalid metadata.
func TestManagedEndpointApiValidation(t *testing.T) {
	for _, api := range []string{"file:///management", "https://root:private@management.test", "https://management.test?token=private", "https://management.test?", "https://management.test#private", "https://:7833", "/api"} {
		assert := is.New(t)
		err := ValidateServerSettings(Settings{ApiUrl: api, OpenBaoURL: "https://vault.test", TelemetryURL: "https://ingest.test"})
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), "private"))
	}
}
