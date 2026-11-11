package platform

import (
	"testing"

	"github.com/matryer/is"
)

// TestValidateServerSettings requires reachable destination shapes without probing services.
func TestValidateServerSettings(t *testing.T) {
	for _, settings := range []Settings{
		{OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"},
		{Profile: "prod", OpenBaoURL: "https://vault.test", TelemetryURL: "https://telemetry.test"},
		{Profile: "production", OpenBaoURL: "https://vault.test", LokiURL: "https://logs.test/loki/api/v1/push", PrometheusURL: "https://metrics.test/api/v1/write"},
	} {
		is.New(t).NoErr(ValidateServerSettings(settings))
	}
	for _, settings := range []Settings{
		{},
		{TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "https://vault.test"},
		{OpenBaoURL: "https://vault.test", LokiURL: "https://logs.test/loki/api/v1/push"},
		{Profile: "unknown", OpenBaoURL: "https://vault.test", TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "file:///vault", TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "https://root:secret@vault.test", TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "https://vault.test", TelemetryURL: "https://token@telemetry.test"},
		{OpenBaoURL: "https://vault.test", LokiURL: "https://token@logs.test", PrometheusURL: "https://metrics.test"},
		{OpenBaoURL: "https://vault.test?token=secret", TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "https://vault.test", TelemetryURL: "https://telemetry.test#secret"},
		{OpenBaoURL: "https://vault.test/path", TelemetryURL: "https://telemetry.test"},
		{OpenBaoURL: "https://vault.test", TelemetryURL: "https://telemetry.test/path"},
		{OpenBaoURL: "https://vault.test", LokiURL: "https://logs.test/loki/api/v1/push?token=secret", PrometheusURL: "https://metrics.test/api/v1/write"},
		{OpenBaoURL: "https://vault.test", LokiURL: "https://logs.test/loki/api/v1/push", PrometheusURL: "https://metrics.test/api/v1/write#secret"},
	} {
		is.New(t).True(ValidateServerSettings(settings) != nil)
	}
}

// TestValidateBootstrapSettings allows services to be absent during initial
// installation while validating every supplied endpoint and profile normally.
func TestValidateBootstrapSettings(t *testing.T) {
	for _, settings := range []Settings{
		{},
		{Profile: "dev"},
		{Profile: "prod"},
		{Profile: "production"},
		{OpenBaoURL: "https://vault.example.test"},
		{TelemetryURL: "https://telemetry.example.test"},
		{LokiURL: "https://logs.example.test/loki/api/v1/push"},
		{PrometheusURL: "https://metrics.example.test/api/v1/write"},
	} {
		is.New(t).NoErr(ValidateBootstrapSettings(settings))
	}
	for _, test := range []struct {
		name     string
		settings Settings
	}{
		{"unknown profile", Settings{Profile: "unknown"}},
		{"unsupported scheme", Settings{OpenBaoURL: "file:///vault"}},
		{"missing host", Settings{OpenBaoURL: "https://"}},
		{"relative endpoint", Settings{TelemetryURL: "telemetry.example.test"}},
		{"credentials", Settings{OpenBaoURL: "https://private-token@vault.example.test"}},
		{"invalid escaping", Settings{TelemetryURL: "https://telemetry.example.test/%zz"}},
		{"origin path", Settings{OpenBaoURL: "https://vault.example.test/path"}},
		{"telemetry origin path", Settings{TelemetryURL: "https://telemetry.example.test/path"}},
		{"query", Settings{LokiURL: "https://logs.example.test/loki/api/v1/push?token=private"}},
		{"fragment", Settings{PrometheusURL: "https://metrics.example.test/api/v1/write#private"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			is.New(t).True(ValidateBootstrapSettings(test.settings) != nil)
		})
	}
}
