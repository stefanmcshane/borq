package apps

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

type backupCoverageTarget struct {
	component BackupCoverageComponent
	storage   ManagementS3
	prefix    string
	server    string
	major     int
	schedule  cron.Schedule
}

func (manager Manager) workloadBackupTargets(ctx context.Context) ([]backupCoverageTarget, error) {
	targets := []backupCoverageTarget{}
	records, err := manager.database.List(ctx, "space")
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		for _, application := range stored.Space.Configuration.Apps {
			if !application.Sqlite.Enabled {
				continue
			}
			target := backupCoverageTarget{component: BackupCoverageComponent{Kind: "sqlite", Name: path.Join(stored.Space.Organization, stored.Space.Name, application.Name)}, prefix: path.Join("sqlite", stored.Space.ID, application.Name), storage: stored.SqliteStorage}
			if !application.Sqlite.Backups {
				target.component.State, target.component.Message = "disabled", "SQLite backups are disabled for this application. Enable sqlite.backups."
			} else if target.storage.Bucket == "" {
				var err error
				target, err = manager.backupTargetSpaceStorage(ctx, stored.Space.ID, target)
				if err != nil {
					return nil, err
				}
			}
			targets = append(targets, target)
		}
	}
	records, err = manager.database.List(ctx, "space-pocketid")
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		var stored spacePocketIdRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.PocketId.State == "removed" {
			continue
		}
		target := backupCoverageTarget{component: BackupCoverageComponent{Kind: "pocketid", Name: path.Join(stored.Organization, stored.Space, stored.PocketId.Name)}, prefix: path.Join("sqlite", stored.SpaceID, "pocketid", stored.PocketId.ID)}
		target, err = manager.backupTargetSpaceStorage(ctx, stored.SpaceID, target)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
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
		location := manager.pocketIdBackupStorage(stored)
		target := backupCoverageTarget{component: BackupCoverageComponent{Kind: "pocketid", Name: stored.Organization + "/identity"}, prefix: "sqlite", storage: ManagementS3{Endpoint: location.Endpoint, Region: location.Region, Bucket: location.Bucket, PathStyle: location.PathStyle, CredentialsRef: "borq/platform/rustfs"}}
		if !stored.BackupConfigured {
			target.component.State, target.component.Message = "missing", "Pocket ID backup storage has not been configured. Check the organization's Pocket ID setup."
		}
		targets = append(targets, target)
	}
	records, err = manager.database.List(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		var stored postgresRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.Postgres.State == "removed" {
			continue
		}
		target := backupCoverageTarget{component: BackupCoverageComponent{Kind: "postgres", Name: path.Join(stored.Organization, stored.Space, stored.Postgres.Name)}}
		if !postgresBackupsEnabled(stored.Postgres.Specification) {
			target.component.State, target.component.Message = "disabled", "PostgreSQL backups are disabled for this database. Enable backup.enabled."
			targets = append(targets, target)
			continue
		}
		archive, _, err := loadDocument[postgresArchive](ctx, manager.database, "postgres-archive", stored.ArchiveID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if errors.Is(err, ErrNotFound) {
			target.component.State, target.component.Message = "missing", "PostgreSQL backup storage has not been registered. Check database provisioning."
		}
		target.storage, target.server, target.major = archive.managementS3(), archive.ServerName, archive.MajorVersion
		schedule := stored.Postgres.Specification.Backup.Schedule
		if schedule == "" {
			schedule = "0 0 * * *"
		}
		target.schedule, err = cron.ParseStandard(schedule)
		if err != nil {
			target.component.State, target.component.Message = "unknown", "The saved PostgreSQL backup schedule could not be read. Reapply a valid backup schedule."
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func (manager Manager) backupTargetSpaceStorage(ctx context.Context, spaceID string, target backupCoverageTarget) (backupCoverageTarget, error) {
	stored, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", spaceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return target, err
	}
	if err != nil || stored.Status.State != "ready" {
		target.component.State, target.component.Message = "missing", "Space backup storage is not ready. Check the space's backup storage status."
		return target, nil
	}
	target.storage = stored.Storage
	return target, nil
}

func sqliteArchiveActivity(objects []objectstore.Object, prefix string) time.Time {
	snapshot := regexp.MustCompile(`^[0-9a-f]{16}/snapshots/[0-9a-f]{8}\.snapshot\.lz4$`)
	wal := regexp.MustCompile(`^[0-9a-f]{16}/wal/[0-9a-f]{8}_[0-9a-f]{8}\.wal\.lz4$`)
	generations := map[string]bool{}
	root := prefix + "/generations/"
	for _, object := range objects {
		relative := strings.TrimPrefix(object.Key, root)
		if object.Size > 0 && strings.HasPrefix(object.Key, root) && snapshot.MatchString(relative) {
			generations[relative[:16]] = true
		}
	}
	latest := time.Time{}
	for _, object := range objects {
		relative := strings.TrimPrefix(object.Key, root)
		if object.Size > 0 && len(relative) > 16 && generations[relative[:16]] && (snapshot.MatchString(relative) || wal.MatchString(relative)) && object.LastModified.After(latest) {
			latest = object.LastModified
		}
	}
	return latest
}
