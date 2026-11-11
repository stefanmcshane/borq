package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestVersionCommands reports the binary's release tag without loading profiles or starting a server.
func TestVersionCommands(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	assert := is.New(t)
	assert.NoErr(os.MkdirAll(filepath.Join(directory, "borq"), 0o700))
	assert.NoErr(os.WriteFile(filepath.Join(directory, "borq", "config.yaml"), []byte("invalid: ["), 0o600))
	for _, build := range []struct{ version, expected string }{
		{"0.0.1-rc42", "borq 0.0.1-rc42\n"},
		{"", "borq dev\n"},
	} {
		for _, arguments := range [][]string{{"--version"}, {"version"}, {"server", "--version"}, {"management", "server", "--version"}} {
			t.Run(build.expected+strings.Join(arguments, " "), func(t *testing.T) {
				assert := is.New(t)
				command := NewRootCommand(context.Background(), logger.Logger{}, Config{Version: build.version})
				var output, diagnostics bytes.Buffer
				command.SetOut(&output)
				command.SetErr(&diagnostics)
				command.SetArgs(arguments)
				assert.NoErr(command.Execute())
				assert.Equal(output.String(), build.expected)
				assert.Equal(diagnostics.String(), "")
			})
		}
	}
}
