package platform

import (
	"fmt"
	"net/url"
)

// ValidateServerSettings checks the management topology before durable server state
// is opened. Component reconciliation applies the same profile and URL normalization.
func ValidateServerSettings(settings Settings) error {
	normalized, err := normalizeSettings(settings)
	if err != nil {
		return err
	}
	if normalized.OpenBaoURL == "" {
		return fmt.Errorf("infrastructure management requires a target-reachable management OpenBao URL")
	}
	if normalized.TelemetryURL == "" && (normalized.LokiURL == "" || normalized.PrometheusURL == "") {
		return fmt.Errorf("infrastructure management requires a management telemetry URL or both Loki and Prometheus ingestion URLs")
	}
	for _, endpoint := range []struct {
		name, address string
		origin        bool
	}{
		{"management OpenBao", normalized.OpenBaoURL, true},
		{"management telemetry", normalized.TelemetryURL, true},
		{"Loki ingestion", normalized.LokiURL, false},
		{"Prometheus ingestion", normalized.PrometheusURL, false},
	} {
		if endpoint.address == "" {
			continue
		}
		parsed, err := url.Parse(endpoint.address)
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("%s URL must not contain credentials, a query, or a fragment", endpoint.name)
		}
		if endpoint.origin && parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("%s URL must be an HTTP(S) origin without a path", endpoint.name)
		}
	}
	return nil
}
