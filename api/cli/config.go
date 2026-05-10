package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config describes borq server runtime configuration read from the environment.
type Config struct {
	Port           int
	DataDir        string
	KubeconfigPath string
	OpenBaoAddress string
}

// Load reads configuration from environment variables, applying defaults
// where a variable is not set.
func Load() (Config, error) {
	cfg := Config{
		Port:    7833,
		DataDir: "/var/lib/borq",
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
	}
	if v, ok := os.LookupEnv("BORQ_OPENBAO_ADDRESS"); ok {
		cfg.OpenBaoAddress = strings.TrimSpace(v)
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
