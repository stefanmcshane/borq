package platform

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	// OpenBaoNodePort exposes Borq's management vault to deployment targets.
	OpenBaoNodePort = 30200
	// RustfsNodePort exposes the built-in S3 service to deployment targets.
	RustfsNodePort = 30900
	// TelemetryNodePort exposes authenticated management ingestion to target Alloy.
	TelemetryNodePort = 31834
)

func managedEndpointDefaults(settings Settings) (Settings, error) {
	settings.ApiUrl = strings.TrimSpace(settings.ApiUrl)
	if settings.ApiUrl == "" {
		return settings, nil
	}
	address, err := url.Parse(settings.ApiUrl)
	if err != nil || address.Hostname() == "" || (address.Scheme != "http" && address.Scheme != "https") || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
		return Settings{}, fmt.Errorf("management API URL must be an HTTP(S) URL without credentials, a query, or a fragment")
	}
	// The advertised hostname must reach the management node from target clusters.
	// An API served through a separate load balancer needs explicit component
	// endpoints when its hostname does not expose the management NodePorts.
	if settings.S3Endpoint == "" {
		settings.S3Endpoint = (&url.URL{Scheme: "http", Host: net.JoinHostPort(address.Hostname(), strconv.Itoa(RustfsNodePort))}).String()
	}
	if settings.OpenBaoURL == "" {
		settings.OpenBaoURL = (&url.URL{Scheme: "http", Host: net.JoinHostPort(address.Hostname(), strconv.Itoa(OpenBaoNodePort))}).String()
	}
	if settings.TelemetryURL == "" && settings.LokiURL == "" && settings.PrometheusURL == "" {
		settings.TelemetryURL = (&url.URL{Scheme: "http", Host: net.JoinHostPort(address.Hostname(), strconv.Itoa(TelemetryNodePort))}).String()
	}
	return settings, nil
}
