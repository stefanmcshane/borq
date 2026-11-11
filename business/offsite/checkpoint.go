package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// VaultCheckpoint retains encrypted recovery material independently of native S3 pruning.
type VaultCheckpoint struct {
	// Artifact identifies the complete encrypted snapshot captured after the inventory.
	Artifact managementbackup.Artifact `json:"artifact"`
	// File is a private local copy of the encrypted payload, never a tenant bucket key.
	File string `json:"file"`
}

// ExportInput crosses the private exporter boundary without relisting changing workloads.
type ExportInput struct {
	// Storage supplies only this frozen source's credentials.
	Storage StorageConfig `json:"storage"`
	// Inventory fixes the exact conditional-read object identities.
	Inventory FrozenInventory `json:"inventory"`
	// Checkpoint is embedded in every retained source snapshot with its workloads.
	Checkpoint VaultCheckpoint `json:"checkpoint"`
}

func storageConfig(source objectstore.Config) StorageConfig {
	return StorageConfig{Endpoint: source.Endpoint, Region: source.Region, Bucket: source.Bucket, Prefix: source.Prefix, PathStyle: source.PathStyle, AccessKey: source.Credentials.AccessKey, SecretKey: source.Credentials.SecretKey, SessionToken: source.Credentials.SessionToken}
}

// ExportFrozen includes the required fresh key checkpoint in the same completed stream.
func ExportFrozen(ctx context.Context, input ExportInput, output io.Writer) error {
	if err := input.validate(); err != nil {
		return err
	}
	return exportInventory(ctx, input, output)
}

func (input ExportInput) validate() error {
	if err := input.Inventory.validate(input.Storage.Config()); err != nil {
		return err
	}
	return input.Checkpoint.validate(input.Inventory)
}

func (checkpoint VaultCheckpoint) validate(inventory FrozenInventory) error {
	if err := checkpointOrder(inventory.CapturedAt, checkpoint.Artifact); err != nil {
		return err
	}
	info, err := os.Stat(checkpoint.File)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != checkpoint.Artifact.Size {
		return errors.New("vault checkpoint must be a complete private encrypted payload")
	}
	return nil
}

func validateCheckpointManifest(manifest Archive) error {
	if manifest.Version < 4 {
		if !manifest.InventoryCapturedAt.IsZero() || manifest.VaultCheckpoint.Kind != "" {
			return errors.New("offsite key checkpoint requires archive version 4")
		}
		return nil
	}
	if err := checkpointOrder(manifest.InventoryCapturedAt, manifest.VaultCheckpoint); err != nil {
		return err
	}
	for _, artifact := range manifest.Native {
		if artifact == manifest.VaultCheckpoint {
			return nil
		}
	}
	return errors.New("offsite archive is missing its required vault checkpoint")
}

func checkpointOrder(capturedAt time.Time, artifact managementbackup.Artifact) error {
	body, _ := json.Marshal(artifact)
	if _, err := managementbackup.ParseCompletion(managementbackup.CompletionKey(artifact), body); err != nil {
		return err
	}
	if capturedAt.IsZero() || artifact.Kind != "openbao" || artifact.CapturedAt.Before(capturedAt) {
		return errors.New("offsite archive requires an OpenBao checkpoint captured after its workload inventory")
	}
	return nil
}

// ReadExportConfig accepts a bounded private inventory larger than operator recovery configuration.
func ReadExportConfig(file string) (ExportInput, error) {
	var input ExportInput
	if err := readPrivateConfig(file, &input, 64<<20); err != nil {
		return ExportInput{}, err
	}
	return input, input.validate()
}
