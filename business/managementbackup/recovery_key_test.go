package managementbackup

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestRecoveryIdentity proves cluster loss needs no second private file and pins the derivation for historical archives.
func TestRecoveryIdentity(t *testing.T) {
	assert := is.New(t)
	root := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	identity, err := RecoveryIdentity(root)
	assert.NoErr(err)
	assert.Equal(identity.Recipient().String(), "age17m3l86y866dzanm0e37krg4dxwuxzrcm5wreypggjc3knmexdupq2adafa")
	again, err := RecoveryIdentity(root)
	assert.NoErr(err)
	assert.Equal(identity.String(), again.String())
	directory := t.TempDir()
	source, cipher, recovered := filepath.Join(directory, "source"), filepath.Join(directory, "cipher"), filepath.Join(directory, "recovered")
	original := []byte("complete native snapshot with sensitive contents")
	assert.NoErr(os.WriteFile(source, original, 0600))
	assert.NoErr(encryptFile(t.Context(), source, cipher, identity.Recipient().String()))
	assert.NoErr(Decrypt(t.Context(), cipher, recovered, again.String()))
	contents, err := os.ReadFile(recovered)
	assert.NoErr(err)
	assert.Equal(contents, original)
	other, err := RecoveryIdentity(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	assert.NoErr(err)
	assert.True(Decrypt(t.Context(), cipher, filepath.Join(directory, "wrong-root"), other.String()) != nil)
	for _, bad := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		_, err := RecoveryIdentity(bad)
		assert.True(err != nil)
	}
}
