package managementbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
)

// TestLocalNativeEncryption creates recoverable ciphertext with complete artifact
// metadata without requiring a source bucket or changing native retention history.
func TestLocalNativeEncryption(t *testing.T) {
	assert := is.New(t)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	directory := t.TempDir()
	plaintext, encrypted := filepath.Join(directory, "vault.raft"), filepath.Join(directory, "vault.age")
	contents := []byte("native local-only checkpoint secret fixture")
	assert.NoErr(os.WriteFile(plaintext, contents, 0600))
	expected := Artifact{Version: 1, Kind: "openbao", ID: strings.Repeat("a", 32), CapturedAt: time.Now().UTC(), Recipient: identity.Recipient().String()}
	artifact, err := Encrypt(t.Context(), expected, plaintext, encrypted)
	assert.NoErr(err)
	ciphertext, err := os.ReadFile(encrypted)
	assert.NoErr(err)
	digest := sha256.Sum256(ciphertext)
	expected.Size, expected.SHA256 = int64(len(ciphertext)), hex.EncodeToString(digest[:])
	assert.Equal(artifact, expected)
	info, err := os.Stat(encrypted)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	restored := filepath.Join(directory, "recovered.raft")
	assert.NoErr(os.Remove(plaintext))
	assert.NoErr(Decrypt(t.Context(), encrypted, restored, identity.String()))
	actual, err := os.ReadFile(restored)
	assert.NoErr(err)
	assert.Equal(actual, contents)
	_, err = Encrypt(t.Context(), expected, restored, encrypted)
	assert.True(err != nil)
	retained, err := os.ReadFile(encrypted)
	assert.NoErr(err)
	assert.Equal(retained, ciphertext)
}

// TestLocalNativeEncryptionFailures rejects invalid metadata and cleans partial
// ciphertext after cancellation, without exposing key material or replacing files.
func TestLocalNativeEncryptionFailures(t *testing.T) {
	for _, failure := range []string{"identity", "artifact", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			assert := is.New(t)
			identity, err := age.GenerateX25519Identity()
			assert.NoErr(err)
			directory := t.TempDir()
			plaintext, encrypted := filepath.Join(directory, "vault.raft"), filepath.Join(directory, "vault.age")
			assert.NoErr(os.WriteFile(plaintext, []byte("native checkpoint"), 0600))
			artifact := Artifact{Version: 1, Kind: "openbao", ID: strings.Repeat("a", 32), CapturedAt: time.Now().UTC(), Recipient: identity.Recipient().String()}
			ctx := t.Context()
			switch failure {
			case "identity":
				artifact.Recipient = identity.String()
			case "artifact":
				artifact.ID = "invalid"
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, err = Encrypt(ctx, artifact, plaintext, encrypted)
			assert.True(err != nil && !strings.Contains(err.Error(), identity.String()))
			_, err = os.Stat(encrypted)
			assert.True(os.IsNotExist(err))
		})
	}
}
