package managementbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"filippo.io/age"
)

// ValidateRecipient accepts only a public age X25519 recipient, never a private recovery identity.
func ValidateRecipient(recipient string) error {
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return errors.New("backup encryption key must be a public age X25519 key")
	}
	return nil
}

// Encrypt creates a private, complete native ciphertext and its verified artifact
// without publishing it into the independently retained local backup catalogue.
func Encrypt(ctx context.Context, artifact Artifact, plaintextFile, ciphertextFile string) (Artifact, error) {
	if artifact.Version != 1 || !ValidKind(artifact.Kind) || !ManagedKey(PayloadKey(artifact)) || artifact.CapturedAt.IsZero() {
		return Artifact{}, errors.New("invalid management snapshot identity")
	}
	if err := encryptFile(ctx, plaintextFile, ciphertextFile, artifact.Recipient); err != nil {
		return Artifact{}, err
	}
	opened, err := os.Open(ciphertextFile)
	if err != nil {
		return Artifact{}, errors.New("open encrypted native checkpoint")
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPayloadSize {
		return Artifact{}, errors.New("management snapshot payload has an unsupported size")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, contextReader{ctx: ctx, Reader: io.LimitReader(opened, MaxPayloadSize+1)})
	if err != nil || size != info.Size() {
		return Artifact{}, errors.New("verify complete encrypted native checkpoint")
	}
	artifact.Size, artifact.SHA256 = size, hex.EncodeToString(digest.Sum(nil))
	return artifact, nil
}

func encryptFile(ctx context.Context, source, destination, recipient string) error {
	public, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return errors.New("invalid backup encryption public key")
	}
	input, err := os.Open(source)
	if err != nil {
		return errors.New("open native snapshot for encryption")
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPlaintextSize {
		return errors.New("native snapshot exceeds the 8 GiB encrypted payload limit")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("create private encrypted native snapshot")
	}
	completed := false
	defer func() {
		_ = output.Close()
		if !completed {
			_ = os.Remove(destination)
		}
	}()
	encrypted, err := age.Encrypt(output, public)
	if err != nil {
		return errors.New("initialize native snapshot encryption")
	}
	count, copyErr := io.Copy(encrypted, contextReader{ctx: ctx, Reader: io.LimitReader(input, MaxPlaintextSize+1)})
	if errors.Join(copyErr, encrypted.Close(), output.Sync(), output.Close()) != nil || count != info.Size() || count > MaxPlaintextSize {
		return errors.New("encrypt complete native snapshot")
	}
	completed = true
	return nil
}

// Decrypt verifies authenticated age ciphertext completely before a native restore is permitted.
func Decrypt(ctx context.Context, source, destination, identity string) error {
	private, err := age.ParseX25519Identity(identity)
	if err != nil {
		return errors.New("a retained age X25519 identity is required for native recovery")
	}
	input, err := os.Open(source)
	if err != nil {
		return errors.New("open encrypted management snapshot")
	}
	defer input.Close()
	decrypted, err := age.Decrypt(input, private)
	if err != nil {
		return errors.New("management snapshot identity does not decrypt this payload")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("create private native recovery file")
	}
	count, copyErr := io.Copy(output, contextReader{ctx: ctx, Reader: io.LimitReader(decrypted, MaxPlaintextSize+1)})
	if errors.Join(copyErr, output.Sync(), output.Close()) != nil || count > MaxPlaintextSize {
		os.Remove(destination)
		return errors.New("management snapshot ciphertext authentication failed")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (reader contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.Reader.Read(data)
}
