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
