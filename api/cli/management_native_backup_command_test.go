package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/matryer/is"
)

// TestNativeRecoveryKeygen owns private identity custody, public-only output and no overwrite at the CLI boundary.
func TestNativeRecoveryKeygen(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	destination := filepath.Join(t.TempDir(), "retained.agekey")
	output, _, err := runConfiguredApplicationCommand("management", "backups", "local", "keygen", "--identity-file", destination)
	assert.NoErr(err)
	saved, err := os.ReadFile(destination)
	assert.NoErr(err)
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(saved)))
	assert.NoErr(err)
	assert.Equal(strings.TrimSpace(output), identity.Recipient().String())
	assert.True(!strings.Contains(output, identity.String()))
	info, err := os.Stat(destination)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	_, _, err = runConfiguredApplicationCommand("management", "backups", "local", "keygen", "--identity-file", destination)
	assert.True(err != nil && strings.Contains(err.Error(), "new private file"))
	after, err := os.ReadFile(destination)
	assert.NoErr(err)
	assert.Equal(after, saved)
}
