package managementbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// DecryptInput selects a completed native artifact and independently retained recovery material.
type DecryptInput struct {
	// Storage supplies explicit local or rehydrated S3 access.
	Storage objectstore.Config
	// Kind and ID identify one completed SQLite bundle or OpenBao Raft snapshot.
	Kind, ID string
	// SealKey is read from the original combined management recovery file.
	SealKey string
	// OutputFile must be a new path; only authenticated native bytes are published there.
	OutputFile string
}

// DecryptSnapshot verifies a selected ciphertext and atomically creates a private native file.
// It never contacts a vault or starts restored workloads.
func DecryptSnapshot(ctx context.Context, input DecryptInput) (Artifact, error) {
	identity, err := RecoveryIdentity(input.SealKey)
	if err != nil {
		return Artifact{}, err
	}
	if input.OutputFile == "" {
		return Artifact{}, errors.New("native decrypt requires a new output file")
	}
	destination, err := filepath.Abs(input.OutputFile)
	if err != nil {
		return Artifact{}, errors.New("resolve native decrypt output")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return Artifact{}, errors.New("native decrypt output must not already exist, including symlinks")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	directory, err := os.MkdirTemp(filepath.Dir(destination), ".borq-native-decrypt-")
	if err != nil {
		return Artifact{}, errors.New("create private native decrypt workspace")
	}
	defer os.RemoveAll(directory)
	encrypted, native := filepath.Join(directory, "encrypted"), filepath.Join(directory, "native")
	artifact, err := Download(ctx, input.Storage, input.Kind, input.ID, encrypted)
	if err != nil {
		return Artifact{}, err
	}
	if err := Decrypt(ctx, encrypted, native, identity.String()); err != nil {
		return Artifact{}, err
	}
	// Linking the complete file within this filesystem is atomic and never replaces a concurrent file or symlink.
	if err := os.Link(native, destination); err != nil {
		return Artifact{}, errors.New("publish native decrypt output without replacing existing files")
	}
	return artifact, nil
}
