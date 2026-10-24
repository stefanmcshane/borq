package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// ListPostgresBackups reads physical object metadata, including archives retained after source deletion.
func (manager Manager) ListPostgresBackups(ctx context.Context, organization, space, name string) ([]PostgresBackup, error) {
	if err := validateNames(organization, space, name); err != nil {
		return nil, err
	}
	if _, err := manager.GetSpace(ctx, organization, space); err != nil {
		return nil, err
	}
	org, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization)
	if err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "postgres-archive")
	if err != nil {
		return nil, err
	}
	result := []PostgresBackup{}
	for _, record := range records {
		var archive postgresArchive
		if err := json.Unmarshal(record.Value, &archive); err != nil {
			return nil, err
		}
		if archive.Organization != organization || archive.OrganizationID != org.ID || archive.Space != space || archive.Name != name {
			continue
		}
		config, err := manager.ResolveManagementS3(ctx, archive.managementS3())
		if err != nil {
			return nil, err
		}
		catalogue, err := postgres.Catalogue(ctx, config, archive.ServerName)
		if err != nil {
			return nil, err
		}
		for _, backup := range catalogue {
			entry := PostgresBackup{ID: stableId("postgres-backup/" + archive.ID + "/" + backup.ID), ArchiveID: archive.ID, SourceID: archive.ResourceID, SourceName: archive.Name, BackupID: backup.ID, CompletedAt: backup.CompletedAt, MajorVersion: backup.MajorVersion, SizeBytes: backup.SizeBytes, RecoveryStart: backup.CompletedAt, RecoveryEnd: backup.RecoveryEnd, Eligible: backup.Eligible && backup.MajorVersion == archive.MajorVersion, Reason: backup.Reason}
			if backup.MajorVersion != archive.MajorVersion {
				entry.Reason = "physical PostgreSQL major does not match the registered source archive"
			}
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CompletedAt.After(result[j].CompletedAt) })
	return result, nil
}

// RestorePostgres reserves a fresh destination and freezes an approved source identity before asynchronous work.
func (manager Manager) RestorePostgres(ctx context.Context, organization, space, name string, input PostgresRestoreInput) (Postgres, error) {
	if input.BackupID == "" {
		return Postgres{}, postgresValidationError{message: "an approved backup ID is required"}
	}
	archive, backup, err := manager.findPostgresBackup(ctx, organization, input.BackupID)
	if err != nil {
		return Postgres{}, err
	}
	input.Specification = inheritPostgresRestore(input.Specification, archive.Specification)
	specification, err := normalizePostgres(input.Specification)
	if err != nil {
		return Postgres{}, err
	}
	if postgresMajor(specification.Image) != backup.MajorVersion {
		return Postgres{}, postgresValidationError{message: fmt.Sprintf("physical restore must retain PostgreSQL major version %d", backup.MajorVersion)}
	}
	if !input.RecoveryTarget.IsZero() && (input.RecoveryTarget.Before(backup.CompletedAt) || input.RecoveryTarget.After(backup.RecoveryEnd)) {
		return Postgres{}, postgresValidationError{message: "recovery target is outside the verified available WAL range"}
	}
	task := postgresTask{SourceArchive: archive.ID, BackupID: backup.ID, Fingerprint: backup.Fingerprint, RecoveryTarget: input.RecoveryTarget}
	return manager.acceptPostgres(ctx, organization, space, name, specification, "restore", task)
}

func inheritPostgresRestore(request, source PostgresInput) PostgresInput {
	if request.Image == "" {
		request.Image = source.Image
	}
	if request.Database == "" {
		request.Database = source.Database
	}
	if request.Username == "" {
		request.Username = source.Username
	}
	if request.Storage == "" {
		request.Storage = source.Storage
	}
	return request
}

func (manager Manager) findPostgresBackup(ctx context.Context, organization, id string) (postgresArchive, postgres.BackupMetadata, error) {
	org, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization)
	if err != nil {
		return postgresArchive{}, postgres.BackupMetadata{}, err
	}
	records, err := manager.database.List(ctx, "postgres-archive")
	if err != nil {
		return postgresArchive{}, postgres.BackupMetadata{}, err
	}
	for _, record := range records {
		var archive postgresArchive
		if err := json.Unmarshal(record.Value, &archive); err != nil {
			return postgresArchive{}, postgres.BackupMetadata{}, err
		}
		if archive.Organization != organization || archive.OrganizationID != org.ID {
			continue
		}
		config, err := manager.ResolveManagementS3(ctx, archive.managementS3())
		if err != nil {
			return postgresArchive{}, postgres.BackupMetadata{}, err
		}
		catalogue, err := postgres.Catalogue(ctx, config, archive.ServerName)
		if err != nil {
			return postgresArchive{}, postgres.BackupMetadata{}, err
		}
		for _, backup := range catalogue {
			if stableId("postgres-backup/"+archive.ID+"/"+backup.ID) != id {
				continue
			}
			if !backup.Eligible || backup.MajorVersion != archive.MajorVersion {
				return postgresArchive{}, postgres.BackupMetadata{}, postgresValidationError{message: "selected backup is not eligible for physical recovery"}
			}
			return archive, backup, nil
		}
	}
	return postgresArchive{}, postgres.BackupMetadata{}, fmt.Errorf("%w: approved PostgreSQL backup not found in this organization", ErrNotFound)
}

func (manager Manager) revalidatePostgresBackup(ctx context.Context, organization string, task postgresTask) (postgresArchive, postgres.BackupMetadata, error) {
	archive, backup, err := manager.findPostgresBackup(ctx, organization, stableId("postgres-backup/"+task.SourceArchive+"/"+task.BackupID))
	if err != nil {
		return archive, backup, err
	}
	if archive.ID != task.SourceArchive || backup.ID != task.BackupID || backup.Fingerprint != task.Fingerprint {
		return archive, backup, fmt.Errorf("%w: selected PostgreSQL backup metadata changed after acceptance", ErrConflict)
	}
	if !task.RecoveryTarget.IsZero() && (task.RecoveryTarget.Before(backup.CompletedAt) || task.RecoveryTarget.After(backup.RecoveryEnd)) {
		return archive, backup, postgresValidationError{message: "requested PostgreSQL recovery target is no longer available"}
	}
	return archive, backup, nil
}

// RestoreCheckPostgres authorizes and durably accepts one Job-bound isolated recovery verification.
func (manager Manager) RestoreCheckPostgres(ctx context.Context, organization, space, name string, input PostgresCheckInput) (Operation, error) {
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(organization, space, name))
	if err != nil {
		return Operation{}, err
	}
	if stored.Postgres.State == "removed" {
		return Operation{}, ErrNotFound
	}
	client, clusterInput, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return Operation{}, err
	}
	if input.JobUID == "" || input.Token == "" {
		return Operation{}, postgresValidationError{message: "Job-bound restore check credentials are required"}
	}
	if err := client.AuthenticateCheck(ctx, clusterInput, input.Token, input.JobUID); err != nil {
		return Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	id := stableId("postgres-check/" + stored.ID + "/" + input.JobUID)
	if previous, err := manager.GetPostgresOperation(ctx, id); err == nil {
		return previous, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	backups, err := manager.ListPostgresBackups(ctx, organization, space, name)
	if err != nil {
		return Operation{}, err
	}
	selected := PostgresBackup{}
	for _, backup := range backups {
		if backup.Eligible && backup.SourceID == stored.ID {
			selected = backup
			break
		}
	}
	if selected.ID == "" {
		return Operation{}, fmt.Errorf("%w: no completed full backup is available for this resource", ErrConflict)
	}
	archive, backup, err := manager.findPostgresBackup(ctx, organization, selected.ID)
	if err != nil {
		return Operation{}, err
	}
	checkID, err := newId()
	if err != nil {
		return Operation{}, err
	}
	checkArchive, err := newId()
	if err != nil {
		return Operation{}, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Operation{}, errors.New("application manager is stopping")
	}
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(organization, space, name))
	if err != nil {
		return Operation{}, err
	}
	if stored.ActiveOperation != "" || stored.ActiveCheck != "" {
		return Operation{}, fmt.Errorf("%w: PostgreSQL operation or restore check already active", ErrConflict)
	}
	now := time.Now().UTC()
	task := postgresTask{Key: addonKey(organization, space, name), Action: "check", Deadline: now.Add(time.Duration(stored.Postgres.Specification.RestoreCheck.TimeoutSeconds) * time.Second), SourceArchive: archive.ID, BackupID: backup.ID, Fingerprint: backup.Fingerprint, CheckID: checkID, CheckName: "pg-check-" + checkID[:20], CheckArchive: checkArchive, CheckJobUID: input.JobUID, Cleanup: "pending"}
	operation := Operation{ID: id, SpaceID: clusterInput.SpaceID, Status: "pending", Phase: "postgres-check", Message: "isolated PostgreSQL recovery verification accepted", StartedAt: now, UpdatedAt: now}
	stored.ActiveCheck = id
	stored.Postgres.RestoreCheck = PostgresRestoreCheck{Status: "pending", Operation: id, Cleanup: "pending", StartedAt: now}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
		revision        int64
	}{{"postgres", task.Key, stored, record.Revision}, {"postgres-task", id, task, 0}, {"postgres-operation", id, operation, 0}} {
		write, err := documentWrite(item.collection, item.key, item.value, item.revision)
		if err != nil {
			return Operation{}, err
		}
		writes = append(writes, write)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Operation{}, translateStoreError(err)
	}
	manager.startPostgresOperation(operation)
	return operation, nil
}

// GetPostgresCheckOperation authorizes the same live Job before reading its resource-bound verification result.
func (manager Manager) GetPostgresCheckOperation(ctx context.Context, organization, space, name, id string, input PostgresCheckInput) (Operation, error) {
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(organization, space, name))
	if err != nil {
		return Operation{}, err
	}
	client, clusterInput, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return Operation{}, err
	}
	if err := client.AuthenticateCheck(ctx, clusterInput, input.Token, input.JobUID); err != nil {
		return Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	task, _, err := loadDocument[postgresTask](ctx, manager.database, "postgres-task", id)
	if err != nil {
		return Operation{}, err
	}
	if task.Key != addonKey(organization, space, name) || task.CheckJobUID != input.JobUID || task.Action != "check" {
		return Operation{}, ErrNotFound
	}
	return manager.GetPostgresOperation(ctx, id)
}
