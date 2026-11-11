package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config describes borq server runtime configuration read from the environment.
type Config struct {
	// Version is the image tag embedded at build time, independent of environment settings.
	Version string
	// NetworkConfigFile contains non-secret networking settings mounted by bootstrap.
	NetworkConfigFile string `env:"BORQ_NETWORK_CONFIG_FILE"`
	// TailscaleClientID identifies the operator's bootstrap OAuth application.
	TailscaleClientID string `env:"BORQ_TAILSCALE_CLIENT_ID"`
	// TailscaleClientSecret is accepted only at the bootstrap boundary.
	TailscaleClientSecret string `env:"BORQ_TAILSCALE_CLIENT_SECRET"`
	// CloudflareApiToken is staged only into the management namespace.
	CloudflareApiToken string `env:"BORQ_CLOUDFLARE_API_TOKEN"`
	// PrometheusQueryUrl selects the private metrics query endpoint; empty uses management service discovery.
	PrometheusQueryUrl string `env:"BORQ_PROMETHEUS_QUERY_URL"`
	// LokiQueryUrl selects the private logs query endpoint; empty uses management service discovery.
	LokiQueryUrl string `env:"BORQ_LOKI_QUERY_URL"`
	// ApiUrl is the management API address used to default managed service endpoints.
	ApiUrl string `env:"BORQ_API_URL"`
	Port   int
	// TelemetryPort serves authenticated ingestion independently of the private API.
	TelemetryPort         int `env:"BORQ_TELEMETRY_PORT"`
	DataDir               string
	KubeconfigPath        string
	kubeconfigFromDefault bool
	OpenBaoAddress        string
	// InfrastructureEnabled opts the server into platform installation on registration.
	InfrastructureEnabled bool `env:"BORQ_INFRASTRUCTURE_ENABLED"`
	// ManagementEnabled lets this server install its own supporting components.
	ManagementEnabled bool `env:"BORQ_MANAGEMENT_ENABLED"`
	// ManagementClusterName identifies the management installation for shared storage.
	ManagementClusterName string `env:"BORQ_MANAGEMENT_CLUSTER_NAME"`
	// InfrastructureProfile selects dev or prod component defaults.
	InfrastructureProfile string `env:"BORQ_INFRASTRUCTURE_PROFILE"`
	// ManagementKubeconfigPath is empty for service-account authentication.
	ManagementKubeconfigPath string `env:"BORQ_MANAGEMENT_KUBECONFIG"`
	// ManagementOpenBaoURL must be reachable from every managed target.
	ManagementOpenBaoURL string `env:"BORQ_MANAGEMENT_OPENBAO_URL"`
	// ManagementTelemetryURL exposes the authenticated Borq ingestion routes.
	ManagementTelemetryURL string `env:"BORQ_MANAGEMENT_TELEMETRY_URL"`
	// ManagementLokiURL overrides the authenticated log push endpoint.
	ManagementLokiURL string `env:"BORQ_MANAGEMENT_LOKI_URL"`
	// ManagementPrometheusURL overrides the authenticated remote-write endpoint.
	ManagementPrometheusURL string `env:"BORQ_MANAGEMENT_PROMETHEUS_URL"`
	// ManagementS3Endpoint overrides the built-in S3 endpoint reachable from targets.
	ManagementS3Endpoint string `env:"BORQ_MANAGEMENT_S3_ENDPOINT"`
	// InfrastructureStorageClass selects the local persistent storage class.
	InfrastructureStorageClass string `env:"BORQ_INFRASTRUCTURE_STORAGE_CLASS"`
}

// Load reads configuration from environment variables, applying defaults
// where a variable is not set.
func Load() (Config, error) {
	cfg := Config{
		Port:          7833,
		TelemetryPort: 7834,
		DataDir:       "/var/lib/borq",
	}
	if value, ok := os.LookupEnv("BORQ_TELEMETRY_PORT"); ok {
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("BORQ_TELEMETRY_PORT must be between 1 and 65535")
		}
		cfg.TelemetryPort = port
	}

	if v, ok := os.LookupEnv("BORQ_PORT"); ok {
		if port, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Port = port
		}
	}
	if v, ok := os.LookupEnv("BORQ_DATA_DIR"); ok {
		cfg.DataDir = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("BORQ_KUBECONFIG"); ok {
		cfg.KubeconfigPath = strings.TrimSpace(v)
	} else {
		cfg.KubeconfigPath = defaultKubeconfigPath()
		cfg.kubeconfigFromDefault = true
	}
	if v, ok := os.LookupEnv("BORQ_OPENBAO_ADDRESS"); ok {
		cfg.OpenBaoAddress = strings.TrimSpace(v)
	}
	if value, ok := os.LookupEnv("BORQ_INFRASTRUCTURE_ENABLED"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("BORQ_INFRASTRUCTURE_ENABLED must be a boolean")
		}
		cfg.InfrastructureEnabled = enabled
	}
	if value, ok := os.LookupEnv("BORQ_MANAGEMENT_ENABLED"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("BORQ_MANAGEMENT_ENABLED must be a boolean")
		}
		cfg.ManagementEnabled = enabled
	}
	// Destinations are pointers because this boundary populates its command-local config.
	for _, field := range []struct {
		name        string
		destination *string
	}{
		{"BORQ_NETWORK_CONFIG_FILE", &cfg.NetworkConfigFile},
		{"BORQ_TAILSCALE_CLIENT_ID", &cfg.TailscaleClientID},
		{"BORQ_TAILSCALE_CLIENT_SECRET", &cfg.TailscaleClientSecret},
		{"BORQ_CLOUDFLARE_API_TOKEN", &cfg.CloudflareApiToken},
		{"BORQ_MANAGEMENT_S3_ENDPOINT", &cfg.ManagementS3Endpoint},
		{"BORQ_API_URL", &cfg.ApiUrl},
		{"BORQ_PROMETHEUS_QUERY_URL", &cfg.PrometheusQueryUrl},
		{"BORQ_LOKI_QUERY_URL", &cfg.LokiQueryUrl},
		{"BORQ_INFRASTRUCTURE_PROFILE", &cfg.InfrastructureProfile},
		{"BORQ_MANAGEMENT_CLUSTER_NAME", &cfg.ManagementClusterName},
		{"BORQ_MANAGEMENT_KUBECONFIG", &cfg.ManagementKubeconfigPath},
		{"BORQ_MANAGEMENT_OPENBAO_URL", &cfg.ManagementOpenBaoURL},
		{"BORQ_MANAGEMENT_TELEMETRY_URL", &cfg.ManagementTelemetryURL},
		{"BORQ_MANAGEMENT_LOKI_URL", &cfg.ManagementLokiURL},
		{"BORQ_MANAGEMENT_PROMETHEUS_URL", &cfg.ManagementPrometheusURL},
		{"BORQ_INFRASTRUCTURE_STORAGE_CLASS", &cfg.InfrastructureStorageClass},
	} {
		if value, ok := os.LookupEnv(field.name); ok {
			*field.destination = strings.TrimSpace(value)
		}
	}

	return cfg, nil
}

func defaultKubeconfigPath() string {
	kubeconfigPaths := filepath.SplitList(os.Getenv("KUBECONFIG"))
	for _, p := range kubeconfigPaths {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}

	home, err := os.UserHomeDir()
	if err == nil {
		if _, err := os.Stat(filepath.Join(home, ".kube", "config")); err == nil {
			return filepath.Join(home, ".kube", "config")
		}
	}

	return ""
}
