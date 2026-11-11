package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestImportImageUsesPrivateToolArchive passes a bundled archive directly to k3d without requiring a Docker executable.
func TestImportImageUsesPrivateToolArchive(t *testing.T) {
	is := is.New(t)
	directory := t.TempDir()
	tool := filepath.Join(directory, "k3d")
	output := filepath.Join(directory, "arguments")
	t.Setenv("BORQ_TEST_IMPORT_ARGS", output)
	is.NoErr(os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$BORQ_TEST_IMPORT_ARGS\"\n"), 0700))
	t.Setenv("PATH", directory)
	t.Setenv("DOCKER_HOST", "unix:///test/docker.sock")
	archive := filepath.Join(directory, "server-image.tar")
	is.NoErr(os.WriteFile(archive, []byte("archive fixture"), 0600))
	is.NoErr(ImportImage(context.Background(), "management", archive))
	arguments, err := os.ReadFile(output)
	is.NoErr(err)
	is.Equal(strings.TrimSpace(string(arguments)), "image import --cluster management "+archive)
	is.True(ImportImage(context.Background(), "../bad", archive) != nil)
	is.True(ImportImage(context.Background(), "management", directory) != nil)
}
