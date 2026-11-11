package apps

import (
	"context"
	"errors"
	"path"

	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

func (manager Manager) spacePocketIdBackup(ctx context.Context, stored spacePocketIdRecord) (sqliteworkload.Destination, objectstore.Credentials, error) {
	target, err := manager.GetSpace(ctx, stored.Organization, stored.Space)
	if err != nil || target.ID != stored.SpaceID {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, errors.New("Pocket ID archive space ownership changed")
	}
	manager.runtime.storageMutex.Lock()
	_, err = manager.ensureSpaceStorage(ctx, target)
	manager.runtime.storageMutex.Unlock()
	if err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, err
	}
	spaceStorage, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", stored.SpaceID)
	if err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, err
	}
	configuration, err := manager.ResolveManagementS3(ctx, spaceStorage.Parent)
	if err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, err
	}
	// Follow the existing Space storage binding during recovery. The archive's age
	// key stays in the provider's original credential reference across that move.
	configuration.Bucket = spaceStorage.Storage.Bucket
	configuration.Prefix = path.Join(spaceStorage.Storage.Prefix, "sqlite", stored.SpaceID, "pocketid", stored.PocketId.ID)
	credentials, err := objectstore.RustfsCredentials(ctx, configuration)
	if err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, errors.New("Pocket ID archive credentials could not be provisioned")
	}
	configuration.Credentials = credentials
	archive, err := objectstore.New(configuration)
	if err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, err
	}
	if err := archive.Validate(ctx); err != nil {
		return sqliteworkload.Destination{}, objectstore.Credentials{}, errors.New("Pocket ID archive access could not be verified")
	}
	return sqliteworkload.Destination{Endpoint: configuration.Endpoint, Region: configuration.Region, Bucket: configuration.Bucket, Prefix: configuration.Prefix, PathStyle: configuration.PathStyle, SyncInterval: "1s"}, credentials, nil
}
