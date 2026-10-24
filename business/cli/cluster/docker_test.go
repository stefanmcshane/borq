package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestDockerHelperEnvironment finds Docker Desktop's credential tools without
// requiring a shell setup step or changing the parent process's command path.
func TestDockerHelperEnvironment(t *testing.T) {
	assert := is.New(t)
	homeDirectory := t.TempDir()
	tools := filepath.Join(homeDirectory, ".docker", "bin")
	assert.NoErr(os.MkdirAll(tools, 0o700))
	original := []string{"PATH=/existing/tools", "DOCKER_HOST=unix:///selected/docker.sock"}
	actual := dockerHelperEnvironment(original, homeDirectory)
	assert.True(strings.HasPrefix(environmentValue(actual, "PATH"), "/existing/tools"+string(os.PathListSeparator)+tools))
	assert.Equal(environmentValue(actual, "DOCKER_HOST"), "unix:///selected/docker.sock")
	assert.Equal(environmentValue(original, "PATH"), "/existing/tools")
}

// TestDockerEnvironmentPreservesExplicitSettings keeps daemon, proxy and credential choices supplied by the caller.
func TestDockerEnvironmentPreservesExplicitSettings(t *testing.T) {
	is := is.New(t)
	original := []string{"DOCKER_HOST=unix:///explicit/docker.sock", "DOCKER_CONTEXT=missing", "DOCKER_CONFIG=/missing", "DOCKER_TLS_VERIFY=1", "HTTPS_PROXY=http://proxy.invalid"}
	actual, err := dockerEnvironment(original, t.TempDir())
	is.NoErr(err)
	is.Equal(actual, original)
}

// TestDockerEnvironmentUsesDesktopContext resolves Docker Desktop's per-user socket without invoking the Docker CLI.
func TestDockerEnvironmentUsesDesktopContext(t *testing.T) {
	is := is.New(t)
	directory := t.TempDir()
	is.NoErr(os.WriteFile(filepath.Join(directory, "config.json"), []byte(`{"currentContext":"desktop-linux"}`), 0600))
	digest := sha256.Sum256([]byte("desktop-linux"))
	metadata := filepath.Join(directory, "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json")
	is.NoErr(os.MkdirAll(filepath.Dir(metadata), 0700))
	is.NoErr(os.WriteFile(metadata, []byte(`{"Name":"desktop-linux","Endpoints":{"docker":{"Host":"unix:///Users/local/.docker/run/docker.sock"}}}`), 0600))
	original := []string{"DOCKER_CONFIG=" + directory, "HTTPS_PROXY=http://proxy.invalid", "DOCKER_TLS_VERIFY=1"}
	actual, err := dockerEnvironment(original, t.TempDir())
	is.NoErr(err)
	is.Equal(environmentValue(actual, "DOCKER_HOST"), "unix:///Users/local/.docker/run/docker.sock")
	is.Equal(environmentValue(actual, "DOCKER_CONFIG"), directory)
	is.Equal(environmentValue(actual, "HTTPS_PROXY"), "http://proxy.invalid")
	is.Equal(environmentValue(actual, "DOCKER_TLS_VERIFY"), "1")
	is.Equal(len(original), 3)
}

// TestDockerEnvironmentRejectsBrokenContext avoids silently selecting another daemon when the requested context is unavailable.
func TestDockerEnvironmentRejectsBrokenContext(t *testing.T) {
	is := is.New(t)
	directory := t.TempDir()
	is.NoErr(os.WriteFile(filepath.Join(directory, "config.json"), []byte(`malformed`), 0600))
	_, err := dockerEnvironment([]string{"DOCKER_CONFIG=" + directory}, t.TempDir())
	is.True(err != nil)
	_, err = dockerEnvironment([]string{"DOCKER_CONTEXT=missing", "DOCKER_CONFIG=" + directory}, t.TempDir())
	is.True(err != nil)
	original := []string{"DOCKER_CONTEXT=default", "DOCKER_CONFIG=" + directory}
	actual, err := dockerEnvironment(original, t.TempDir())
	is.NoErr(err)
	is.Equal(actual, original)
}
