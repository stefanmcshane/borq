// Package localconfig locates the shared directory owned by the local Borq CLI.
package localconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// Directory returns ~/.config/borq, respecting an absolute XDG_CONFIG_HOME override.
// This CLI boundary keeps cluster credentials, profiles, and tools in one location.
func Directory() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find Borq config directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path")
	}
	return filepath.Join(base, "borq"), nil
}
