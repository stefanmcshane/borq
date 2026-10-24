package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// localDockerEnvironment adapts Docker CLI context selection for k3d's Engine client.
// The inherited environment is copied so explicit daemon, TLS, proxy and registry settings survive.
func localDockerEnvironment(environment []string) ([]string, error) {
	if environmentValue(environment, "DOCKER_HOST") != "" {
		return environment, nil
	}
	directory, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find Docker configuration: %w", err)
	}
	return dockerEnvironment(dockerHelperEnvironment(environment, directory), directory)
}

// Docker Desktop may keep its credential helper outside the user's command path.
// Add its installed tool directories only to the private k3d subprocess.
func dockerHelperEnvironment(environment []string, homeDirectory string) []string {
	originalPath := environmentValue(environment, "PATH")
	path := originalPath
	for _, directory := range []string{filepath.Join(homeDirectory, ".docker", "bin"), "/Applications/Docker.app/Contents/Resources/bin"} {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			continue
		}
		if path != "" {
			path += string(os.PathListSeparator)
		}
		path += directory
	}
	if path == originalPath {
		return environment
	}
	return withEnvironmentValue(environment, "PATH", path)
}

func dockerEnvironment(environment []string, homeDirectory string) ([]string, error) {
	if environmentValue(environment, "DOCKER_HOST") != "" {
		return environment, nil
	}
	configurationDirectory := environmentValue(environment, "DOCKER_CONFIG")
	if configurationDirectory == "" {
		configurationDirectory = filepath.Join(homeDirectory, ".docker")
	}
	selected := environmentValue(environment, "DOCKER_CONTEXT")
	if selected == "" {
		encoded, err := os.ReadFile(filepath.Join(configurationDirectory, "config.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read Docker context selection: %w", err)
		}
		if err == nil {
			var configuration struct {
				CurrentContext string `json:"currentContext"`
			}
			if err := json.Unmarshal(encoded, &configuration); err != nil {
				return nil, fmt.Errorf("read Docker context selection: invalid JSON")
			}
			selected = configuration.CurrentContext
		}
	}
	if selected == "default" {
		return environment, nil
	}
	if selected != "" {
		digest := sha256.Sum256([]byte(selected))
		metadataPath := filepath.Join(configurationDirectory, "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json")
		encoded, err := os.ReadFile(metadataPath)
		if err != nil {
			return nil, fmt.Errorf("read selected Docker context %q: %w", selected, err)
		}
		var metadata struct {
			Name      string `json:"Name"`
			Endpoints struct {
				Docker struct {
					Host string `json:"Host"`
				} `json:"docker"`
			} `json:"Endpoints"`
		}
		if err := json.Unmarshal(encoded, &metadata); err != nil || metadata.Name != selected || metadata.Endpoints.Docker.Host == "" {
			return nil, fmt.Errorf("selected Docker context %q has no valid Docker endpoint", selected)
		}
		return withEnvironmentValue(environment, "DOCKER_HOST", metadata.Endpoints.Docker.Host), nil
	}
	// Desktop can run without the system-wide /var/run/docker.sock symlink.
	for _, socket := range []string{"/var/run/docker.sock", filepath.Join(homeDirectory, ".docker", "run", "docker.sock"), filepath.Join(homeDirectory, ".docker", "desktop", "docker.sock")} {
		info, err := os.Stat(socket)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return withEnvironmentValue(environment, "DOCKER_HOST", "unix://"+socket), nil
		}
	}
	return environment, nil
}

func environmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		if value, ok := strings.CutPrefix(environment[index], name+"="); ok {
			return value
		}
	}
	return ""
}

func withEnvironmentValue(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, name+"=") {
			result = append(result, entry)
		}
	}
	return append(result, name+"="+value)
}
