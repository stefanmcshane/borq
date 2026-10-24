// Package cluster reads existing local cluster credentials and supports development image imports.
package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const defaultK3dVersion = "v5.8.3"

func validateClusterName(name string) error {
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(name) {
		return errors.New("cluster name must use lowercase letters, digits and hyphens, and be at most 63 characters")
	}
	return nil
}

func runK3d(ctx context.Context, environment []string, args ...string) (string, error) {
	binary, err := privateK3d(ctx)
	if err != nil {
		return "", err
	}
	return runCommandEnvironment(ctx, environment, binary, args...)
}

func writePrivateFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create private cluster file directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".borq-write-*")
	if err != nil {
		return fmt.Errorf("create private cluster file: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write private cluster file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync private cluster file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private cluster file: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace private cluster file: %w", err)
	}
	return nil
}

func runCommandEnvironment(ctx context.Context, environment []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", err
		}

		commandLine := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText == "" {
			return "", fmt.Errorf("%s failed: %w", commandLine, err)
		}

		return "", fmt.Errorf("%s failed: %w: %s", commandLine, err, stderrText)
	}

	return stdout.String(), nil
}
