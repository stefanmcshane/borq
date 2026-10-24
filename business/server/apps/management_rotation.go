package apps

import (
	"context"
	"errors"
	"fmt"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type storageRotation struct {
	Before ManagementS3 `json:"before"`
	After  ManagementS3 `json:"after"`
	Status string       `json:"status"`
}

func (manager Manager) commitManagementS3(ctx context.Context, before, after ManagementS3, revision int64) (ManagementS3, error) {
	rotation, previous, err := loadDocument[storageRotation](ctx, manager.database, "management-s3-rotation", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementS3{}, err
	}
	if rotation.Status == "staged" && rotation.After != after {
		return ManagementS3{}, fmt.Errorf("%w: a shared S3 rotation is still pending", ErrConflict)
	}
	if rotation.Status != "staged" {
		rotation = storageRotation{Before: before, After: after, Status: "staged"}
		write, err := documentWrite("management-s3-rotation", "shared", rotation, previous.Revision)
		if err != nil {
			return ManagementS3{}, err
		}
		if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
			return ManagementS3{}, err
		}
		previous, err = manager.database.Get(ctx, "management-s3-rotation", "shared")
		if err != nil {
			return ManagementS3{}, err
		}
	}
	if rotation.Before.CredentialsRef != "" && rotation.Before.CredentialsRef != rotation.After.CredentialsRef {
		if err := manager.rotatePostgresStorage(ctx, rotation.Before, rotation.After); err != nil {
			return ManagementS3{}, err
		}
	}
	if manager.runtime.infrastructureEnabled {
		dependency, err := manager.managementStorageDependency(ctx)
		if err != nil {
			return ManagementS3{}, err
		}
		if dependency {
			manager.runtime.platformMutex.Lock()
			settings, settingsErr := manager.GetPlatformSettings(ctx)
			if settingsErr == nil {
				settings.S3 = platform.S3Settings{}
				if after.CredentialsRef != "borq/platform/rustfs" {
					settings.S3 = platform.S3Settings{Endpoint: after.Endpoint, Region: after.Region, Bucket: after.Bucket, Prefix: after.Prefix, PathStyle: after.PathStyle, CredentialsRef: after.CredentialsRef}
				}
				_, settingsErr = platform.ReconcileManagementStorage(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: settings, Upgrade: true})
			}
			manager.runtime.platformMutex.Unlock()
			if settingsErr != nil {
				return ManagementS3{}, settingsErr
			}
		}
	}
	write, err := documentWrite("management-s3", "shared", after, revision)
	if err != nil {
		return ManagementS3{}, err
	}
	rotation.Status = "succeeded"
	completed, err := documentWrite("management-s3-rotation", "shared", rotation, previous.Revision)
	if err != nil {
		return ManagementS3{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{write, completed}); err != nil {
		return ManagementS3{}, translateStoreError(err)
	}
	return after, nil
}

func (manager Manager) resumeStorageRotation(ctx context.Context) error {
	rotation, _, err := loadDocument[storageRotation](ctx, manager.database, "management-s3-rotation", "shared")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rotation.Status != "staged" {
		return nil
	}
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		execution, cancel := context.WithTimeout(manager.runtime.context, manager.runtime.rolloutTimeout)
		defer cancel()
		manager.runtime.storageMutex.Lock()
		defer manager.runtime.storageMutex.Unlock()
		current, _, err := loadDocument[storageRotation](execution, manager.database, "management-s3-rotation", "shared")
		if err != nil || current.Status != "staged" {
			return
		}
		config, err := manager.ResolveManagementS3(execution, current.After)
		if err != nil {
			return
		}
		client, err := objectstore.New(config)
		if err != nil {
			return
		}
		if err := client.Validate(execution); err != nil {
			return
		}
		_, record, err := loadDocument[ManagementS3](execution, manager.database, "management-s3", "shared")
		if err != nil && !errors.Is(err, ErrNotFound) {
			return
		}
		_, _ = manager.commitManagementS3(execution, current.Before, current.After, record.Revision)
	}()
	return nil
}
