package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) sqliteStorageInUse(ctx context.Context) (bool, error) {
	records, err := manager.database.List(ctx, "space")
	if err != nil {
		return false, err
	}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return false, err
		}
		if stored.SqliteStorage.Bucket != "" {
			return true, nil
		}
	}
	return false, nil
}

func (manager Manager) sqliteBackupSources(ctx context.Context) ([]ManagementS3, error) {
	records, err := manager.database.List(ctx, "space-storage")
	if err != nil {
		return nil, err
	}
	sources := []ManagementS3{}
	seen := map[ManagementS3]bool{}
	for _, record := range records {
		var stored spaceStorageRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.Status.State == "ready" && !seen[stored.Storage] {
			seen[stored.Storage] = true
			sources = append(sources, stored.Storage)
		}
	}
	// Older spaces can retain a shared SQLite bucket alongside their new bucket.
	records, err = manager.database.List(ctx, "space")
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.SqliteStorage.Bucket != "" && !seen[stored.SqliteStorage] {
			seen[stored.SqliteStorage] = true
			sources = append(sources, stored.SqliteStorage)
		}
	}
	records, err = manager.database.List(ctx, "organization-pocketid")
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		var stored organizationPocketIdRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if !stored.BackupConfigured {
			continue
		}
		stored, _, err = manager.readPocketId(ctx, record.Key)
		if err != nil {
			return nil, err
		}
		location := manager.pocketIdBackupStorage(stored)
		// Export from the bucket root so the archive retains its sqlite/ prefix.
		// The management worker already uses RustFS administrator access for offsite copies.
		source := ManagementS3{Endpoint: location.Endpoint, Region: location.Region, Bucket: location.Bucket, PathStyle: location.PathStyle, CredentialsRef: "borq/platform/rustfs"}
		if !seen[source] {
			seen[source] = true
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (manager Manager) currentSqliteStorage(ctx context.Context, space Space) (ManagementS3, error) {
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(space.Organization, space.Name))
	if err != nil {
		return ManagementS3{}, err
	}
	if stored.SqliteStorage.Bucket != "" {
		return stored.SqliteStorage, nil
	}
	return manager.plannedSpaceStorage(ctx, space)
}

func (manager Manager) prepareSqliteStorage(ctx context.Context, configuration Configuration, stored spaceRecord) (ManagementS3, error) {
	needed := false
	for _, application := range configuration.Apps {
		needed = needed || application.Sqlite.Backups
	}
	if !needed {
		return stored.SqliteStorage, nil
	}
	rotation, _, err := loadDocument[storageRotation](ctx, manager.database, "management-s3-rotation", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementS3{}, err
	}
	if rotation.Status == "staged" {
		return ManagementS3{}, fmt.Errorf("%w: shared S3 credential rotation is still pending", ErrConflict)
	}
	if stored.SqliteStorage.Bucket != "" {
		return stored.SqliteStorage, nil
	}
	storage, err := manager.plannedSpaceStorage(ctx, stored.Space)
	if err != nil {
		return ManagementS3{}, err
	}
	// Internal DNS is valid for converged clusters; remote applications need
	// an operator-configured routable endpoint. Runtime probes verify access.
	if err := storageClientConfig(storage, objectstore.Credentials{}).Validate(); err != nil {
		return ManagementS3{}, err
	}
	return storage, nil
}

func (manager Manager) prepareSqliteRuntime(ctx context.Context, client kubeapps.Client, saved planRecord) error {
	for _, application := range saved.Plan.Configuration.Apps {
		if !application.Sqlite.Enabled {
			continue
		}
		key := saved.Plan.Space.ID + "/" + application.Name
		state, record, err := loadDocument[string](ctx, manager.database, "sqlite-initialization", key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if errors.Is(err, ErrNotFound) {
			state = "pending"
			write, err := documentWrite("sqlite-initialization", key, state, 0)
			if err != nil {
				return err
			}
			if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
				return err
			}
			_, record, err = loadDocument[string](ctx, manager.database, "sqlite-initialization", key)
			if err != nil {
				return err
			}
		}
		if err := client.EnsureSqliteState(ctx, saved.Plan.Space.Namespace, application.Name, saved.Plan.Space.ID, state == "pending"); err != nil {
			return err
		}
		if state == "pending" {
			// Persist consumption before any predeploy or application pod can use this authorization.
			write, err := documentWrite("sqlite-initialization", key, "active", record.Revision)
			if err != nil {
				return err
			}
			if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
				return err
			}
		}
	}
	secrets, err := manager.sqliteBackupCredentials(ctx, saved)
	if err != nil {
		return err
	}
	if err := client.Apply(ctx, secrets, saved.Plan.Space.ID); err != nil {
		return fmt.Errorf("install SQLite backup credentials: %w", err)
	}
	return nil
}

// The caller holds storageMutex across this preflight and acceptance, but waits
// for network I/O before taking the application queue's mutex. Acceptance reloads
// the operation and space revision before committing any desired state.
func (manager Manager) preflightSqlitePlan(ctx context.Context, id string) error {
	_, _, err := loadDocument[Operation](ctx, manager.database, "operation", id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", id)
	if err != nil {
		return err
	}
	if saved.SqliteStorage.Bucket == "" {
		return nil
	}
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(saved.Plan.Space.Organization, saved.Plan.Space.Name))
	if err != nil {
		return err
	}
	if saved.Plan.BaseRevision != stored.Space.Revision {
		return fmt.Errorf("%w: plan is stale; create a new plan from the accepted configuration", ErrConflict)
	}
	current, err := manager.prepareSqliteStorage(ctx, saved.Plan.Configuration, stored)
	if err != nil {
		return err
	}
	if current != saved.SqliteStorage {
		return fmt.Errorf("%w: SQLite backup storage changed; create a new plan", ErrConflict)
	}
	if stored.SqliteStorage.Bucket == "" {
		if _, err := manager.ensureSpaceStorage(ctx, stored.Space); err != nil {
			return fmt.Errorf("%w: SQLite backup storage could not be provisioned: %v", ErrInvalid, err)
		}
	}
	if _, err := manager.sqliteBackupCredentials(ctx, saved); err != nil {
		return fmt.Errorf("%w: SQLite backup storage could not be provisioned: %v", ErrInvalid, err)
	}
	return nil
}

func (manager Manager) sqliteBackupCredentials(ctx context.Context, saved planRecord) ([]map[string]any, error) {
	secrets := []map[string]any{}
	needed := false
	for _, application := range saved.Plan.Configuration.Apps {
		needed = needed || application.Sqlite.Backups
	}
	if !needed {
		return secrets, nil
	}
	spaceStorage, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", saved.Plan.Space.ID)
	if err != nil {
		return nil, err
	}
	parent := spaceStorage.Parent
	if saved.SqliteStorage != spaceStorage.Storage {
		// Pre-space archives retain their original administrative reference. Do not
		// redirect an existing database to an empty dedicated bucket on upgrade.
		retained, err := manager.currentSqliteStorage(ctx, saved.Plan.Space)
		if err != nil || retained != saved.SqliteStorage || retained.CredentialsRef == spaceStorage.Storage.CredentialsRef {
			return nil, fmt.Errorf("%w: SQLite backup storage changed", ErrConflict)
		}
		parent = retained
	}
	configuration, err := manager.ResolveManagementS3(ctx, parent)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite backup storage: %w", err)
	}
	configuration.Bucket, configuration.Prefix = saved.SqliteStorage.Bucket, saved.SqliteStorage.Prefix
	for _, application := range saved.Plan.Configuration.Apps {
		if !application.Sqlite.Backups {
			continue
		}
		archive, err := manager.sqliteArchiveKey(ctx, saved.Plan.Space, application.Name)
		if err != nil {
			return nil, err
		}
		appStorage := configuration
		appStorage.Prefix = path.Join(configuration.Prefix, "sqlite", saved.Plan.Space.ID, application.Name)
		credentials, err := objectstore.RustfsCredentials(ctx, appStorage)
		if err != nil {
			return nil, fmt.Errorf("provision scoped SQLite backup credentials: %w", err)
		}
		appStorage.Credentials = credentials
		scoped, err := objectstore.New(appStorage)
		if err != nil {
			return nil, err
		}
		if err := scoped.Validate(ctx); err != nil {
			return nil, fmt.Errorf("validate scoped SQLite backup access: %w", err)
		}
		secret := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{
				"name": application.Name + "-litestream", "namespace": saved.Plan.Space.Namespace,
				"labels": map[string]any{
					"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": application.Name,
					"borq.run/target-id": saved.Plan.Space.ID, "borq.run/phase": "sqlite",
				},
			},
			"stringData": map[string]any{
				"AWS_ACCESS_KEY_ID":         credentials.AccessKey,
				"AWS_SECRET_ACCESS_KEY":     credentials.SecretKey,
				"AWS_EC2_METADATA_DISABLED": "true",
				"LITESTREAM_AGE_IDENTITY":   archive.Identity,
				"LITESTREAM_AGE_RECIPIENT":  archive.Recipient,
			},
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func splitSqliteManifests(manifests []map[string]any) ([]map[string]any, []map[string]any) {
	storage, applications := []map[string]any{}, []map[string]any{}
	for _, manifest := range manifests {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if labels["borq.run/phase"] == "sqlite" && manifest["kind"] != "CronJob" {
			storage = append(storage, manifest)
			continue
		}
		applications = append(applications, manifest)
	}
	return storage, applications
}
