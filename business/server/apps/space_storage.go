package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// SpaceBackupStorage exposes provisioning progress without credential material.
type SpaceBackupStorage struct {
	// Bucket belongs exclusively to this space's backup storage incarnation.
	Bucket string `json:"bucket"`
	// State is pending, ready, or failed; failed provisioning is retried.
	State string `json:"state"`
	// Message explains provisioning without exposing provider responses.
	Message string `json:"message"`
}

type spaceStorageRecord struct {
	ID          string             `json:"id"`
	SpaceID     string             `json:"spaceId"`
	Parent      ManagementS3       `json:"parent"`
	Storage     ManagementS3       `json:"storage"`
	Status      SpaceBackupStorage `json:"status"`
	NextAttempt time.Time          `json:"nextAttempt,omitempty"`
}

func (manager Manager) spaceStorageInUse(ctx context.Context) (bool, error) {
	records, err := manager.database.List(ctx, "space-storage")
	if err != nil {
		return false, err
	}
	for _, record := range records {
		var stored spaceStorageRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return false, err
		}
		if stored.Status.State == "ready" {
			return true, nil
		}
	}
	return false, nil
}

func newSpaceStorageWrite(space Space) (sqlite.Write, error) {
	id, err := newId()
	if err != nil {
		return sqlite.Write{}, err
	}
	bucket := "borq-space-" + id
	stored := spaceStorageRecord{ID: id, SpaceID: space.ID, Storage: ManagementS3{Bucket: bucket, CredentialsRef: "borq/management/s3/" + id}, Status: SpaceBackupStorage{Bucket: bucket, State: "pending", Message: "Space backup storage is queued."}}
	return documentWrite("space-storage", space.ID, stored, 0)
}

func (manager Manager) spaceBackupStorage(ctx context.Context, space Space) (SpaceBackupStorage, error) {
	stored, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", space.ID)
	if errors.Is(err, ErrNotFound) {
		return SpaceBackupStorage{}, nil
	}
	return stored.Status, err
}

func (manager Manager) plannedSpaceStorage(ctx context.Context, space Space) (ManagementS3, error) {
	stored, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", space.ID)
	if err != nil {
		return ManagementS3{}, fmt.Errorf("space backup storage is unavailable: %w", err)
	}
	if stored.Status.State == "ready" {
		return stored.Storage, nil
	}
	parent, err := manager.GetManagementS3(ctx)
	if err != nil {
		return ManagementS3{}, err
	}
	parent.Bucket, parent.CredentialsRef = stored.Storage.Bucket, stored.Storage.CredentialsRef
	return parent, nil
}

// Callers serialize provisioning and primary-storage changes with storageMutex.
func (manager Manager) ensureSpaceStorage(ctx context.Context, space Space) (ManagementS3, error) {
	stored, record, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", space.ID)
	if err != nil {
		return ManagementS3{}, err
	}
	if stored.Status.State == "ready" {
		return stored.Storage, nil
	}
	storage, parent, err := manager.provisionSpaceStorage(ctx, stored)
	stored.NextAttempt = time.Now().UTC().Add(time.Minute)
	stored.Status.State, stored.Status.Message = "failed", "Space backup storage could not be provisioned; check management RustFS access."
	if err == nil {
		stored.Storage, stored.Parent = storage, parent
		stored.Status.State, stored.Status.Message = "ready", "Space backup storage is ready."
	}
	write, writeErr := documentWrite("space-storage", space.ID, stored, record.Revision)
	if writeErr == nil {
		writeErr = manager.database.Commit(ctx, []sqlite.Write{write})
	}
	if writeErr != nil {
		return ManagementS3{}, writeErr
	}
	if err != nil {
		return ManagementS3{}, fmt.Errorf("%w: %s", ErrComponentsNotReady, stored.Status.Message)
	}
	return stored.Storage, nil
}

func (manager Manager) provisionSpaceStorage(ctx context.Context, stored spaceStorageRecord) (ManagementS3, ManagementS3, error) {
	rotation, _, err := loadDocument[storageRotation](ctx, manager.database, "management-s3-rotation", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementS3{}, ManagementS3{}, err
	}
	if rotation.Status == "staged" {
		return ManagementS3{}, ManagementS3{}, fmt.Errorf("%w: shared S3 credential rotation is still pending", ErrConflict)
	}
	parent, err := manager.GetManagementS3(ctx)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	config, err := manager.ResolveManagementS3(ctx, parent)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	config.Bucket, config.Prefix = stored.Storage.Bucket, ""
	client, err := objectstore.New(config)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	if err := client.EnsureBucket(ctx); err != nil {
		return ManagementS3{}, parent, err
	}
	credentials, err := objectstore.RustfsCredentials(ctx, config)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	config.Credentials = credentials
	client, err = objectstore.New(config)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	if err := client.Validate(ctx); err != nil {
		return ManagementS3{}, parent, err
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return ManagementS3{}, parent, err
	}
	metadata, err := vault.ReadKvMetadata(ctx, "secret", stored.Storage.CredentialsRef)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return ManagementS3{}, parent, err
	}
	if _, err := vault.WriteKv(ctx, "secret", stored.Storage.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"accessKey": credentials.AccessKey, "secretKey": credentials.SecretKey}, CheckVersion: metadata.CurrentVersion}); err != nil {
		return ManagementS3{}, parent, err
	}
	storage := parent
	storage.Bucket, storage.CredentialsRef = stored.Storage.Bucket, stored.Storage.CredentialsRef
	return storage, parent, nil
}

func (manager Manager) startSpaceStorageWorker() {
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			manager.provisionPendingSpaceStorage(manager.runtime.context)
			select {
			case <-manager.runtime.context.Done():
				return
			case <-ticker.C:
			case <-manager.runtime.spaceStorageWake:
			}
		}
	}()
}

func (manager Manager) provisionPendingSpaceStorage(ctx context.Context) {
	if !manager.runtime.infrastructureEnabled && manager.runtime.managementKubeconfigPath == "" {
		return
	}
	records, err := manager.database.List(ctx, "space-storage")
	if err != nil {
		return
	}
	for _, record := range records {
		var stored spaceStorageRecord
		if json.Unmarshal(record.Value, &stored) != nil || stored.Status.State == "ready" || stored.NextAttempt.After(time.Now()) {
			continue
		}
		manager.runtime.storageMutex.Lock()
		attempt, cancel := context.WithTimeout(ctx, time.Minute)
		_, _ = manager.ensureSpaceStorage(attempt, Space{ID: stored.SpaceID})
		cancel()
		manager.runtime.storageMutex.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
}
