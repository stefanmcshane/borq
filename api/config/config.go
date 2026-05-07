package config

import (
	"os"
	"strings"
)

// Config describes borq server runtime configuration read from the environment.
type Config struct {
	Port           int    `env:"BORQ_PORT"`
	DataDir        string `env:"BORQ_DATA_DIR"`
	KubeconfigPath string `env:"BORQ_KUBECONFIG"`
}

// Load reads configuration from environment variables, applying defaults
// where a variable is not set.
func Load() (Config, error) {
	cfg := Config{
		Port:    7833,
		DataDir: "/var/lib/borq",
	}

	if v, ok := os.LookupEnv("BORQ_DATA_DIR"); ok {
		cfg.DataDir = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("BORQ_KUBECONFIG"); ok {
		cfg.KubeconfigPath = strings.TrimSpace(v)
	}

	return cfg, nil
}
