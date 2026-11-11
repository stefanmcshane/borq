package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (archive postgresArchive) managementS3() ManagementS3 {
	return ManagementS3{Endpoint: archive.Endpoint, Region: archive.Region, Bucket: archive.Bucket, Prefix: archive.Prefix, PathStyle: archive.PathStyle, CredentialsRef: archive.CredentialsRef}
}

func (manager Manager) archiveStorage(ctx context.Context, archive postgresArchive) (postgres.Storage, error) {
	config, err := manager.ResolveManagementS3(ctx, archive.managementS3())
	if err != nil {
		return postgres.Storage{}, err
	}
	return postgres.Storage{Endpoint: archive.Endpoint, Region: archive.Region, Bucket: archive.Bucket, Prefix: archive.Prefix, ServerName: archive.ServerName, PathStyle: archive.PathStyle, AccessKey: config.Credentials.AccessKey, SecretKey: config.Credentials.SecretKey, SessionToken: config.Credentials.SessionToken}, nil
}

func (manager Manager) recoveryArchiveStorage(ctx context.Context, archive postgresArchive) (postgres.Storage, error) {
	storage, err := manager.archiveStorage(ctx, archive)
	if err != nil {
		return postgres.Storage{}, err
	}
	records, err := manager.database.List(ctx, "space-storage")
	if err != nil {
		return postgres.Storage{}, err
	}
	// Pre-space archives retain their original administrative reference. New
	// buckets use a separate parent; neither parent is projected into recovery pods.
	parent := archive.managementS3()
	for _, record := range records {
		var source spaceStorageRecord
		if err := json.Unmarshal(record.Value, &source); err != nil {
			return postgres.Storage{}, err
		}
		if source.Storage.Bucket != archive.Bucket || source.Storage.Endpoint != archive.Endpoint {
			continue
		}
		parent = source.Parent
		break
	}
	config, err := manager.ResolveManagementS3(ctx, parent)
	if err != nil {
		return postgres.Storage{}, err
	}
	config.Bucket, config.Prefix = archive.Bucket, archive.Prefix
	credentials, err := objectstore.RustfsReadCredentials(ctx, config)
	if err != nil {
		return postgres.Storage{}, err
	}
	storage.AccessKey, storage.SecretKey, storage.SessionToken = credentials.AccessKey, credentials.SecretKey, credentials.SessionToken
	return storage, nil
}

func (manager Manager) postgresClient(ctx context.Context, stored postgresRecord, credentials bool) (postgres.Client, postgres.Input, error) {
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	client, err := postgres.New(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster})
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	specification := stored.Postgres.Specification
	target, err := manager.GetSpace(ctx, stored.Organization, stored.Space)
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	input := postgres.Input{Namespace: stored.Postgres.Namespace, Name: stored.Postgres.ClusterName, Owner: stored.ID, Organization: stored.Organization, SpaceID: target.ID, Instances: specification.Instances, StorageSize: specification.Storage, StorageClass: specification.StorageClass, Image: specification.Image, Database: specification.Database, Username: specification.Username, Requests: specification.Resources.Requests, Limits: specification.Resources.Limits, RetentionDays: specification.Backup.RecoveryWindowDays, BackupSchedule: "0 " + specification.Backup.Schedule, BackupEnabled: postgresBackupsEnabled(specification), SnapshotSchedule: "0 0 * * * *"}
	input.Credentials = postgres.CredentialSource{Mount: "database-" + cluster.ID, Role: "pg-" + stored.ID, Namespace: stored.Credentials.Namespace, Host: specification.Credentials.Host}
	if stored.Postgres.EnvironmentGroup != "" {
		input.EnvironmentExport = postgres.EnvironmentExport{Name: stored.Postgres.EnvironmentGroup, VaultPath: environmentGroupPath(target.ID, stored.Postgres.EnvironmentGroup)}
	}
	input.SnapshotClass = stored.SnapshotClass
	input.Check = postgres.Check{Enabled: input.BackupEnabled, Schedule: specification.Backup.RestoreCheck.Schedule, TimeoutSeconds: specification.Backup.RestoreCheck.TimeoutSeconds}
	input.Storage.ServerName = "archive-" + stored.ArchiveID
	if manager.runtime.platformSettings.ApiUrl != "" {
		input.Check.ApiURL = strings.TrimRight(manager.runtime.platformSettings.ApiUrl, "/") + "/api/v1/organizations/" + url.PathEscape(stored.Organization) + "/spaces/" + url.PathEscape(stored.Space) + "/managed-addons/postgres/" + url.PathEscape(stored.Postgres.Name) + "/restore-check"
	}
	if !credentials {
		return client, input, nil
	}
	archive, _, err := loadDocument[postgresArchive](ctx, manager.database, "postgres-archive", stored.ArchiveID)
	if err != nil {
		return postgres.Client{}, input, err
	}
	input.Storage, err = manager.archiveStorage(ctx, archive)
	if err != nil {
		return postgres.Client{}, input, err
	}
	if stored.Credentials.Phase != "" {
		return client, input, nil
	}
	targetClient, err := newClusterClient(cluster)
	if err != nil {
		return postgres.Client{}, input, err
	}
	vault, mount, err := openEnvironmentVault(ctx, targetClient)
	if err != nil {
		return postgres.Client{}, input, err
	}
	path, versionText, ok := strings.Cut(stored.CredentialsRef, "#")
	version, parseErr := strconv.Atoi(versionText)
	if !ok || parseErr != nil || version < 1 {
		return postgres.Client{}, input, errors.New("invalid PostgreSQL credential reference")
	}
	entry, err := vault.ReadKv(ctx, mount, path, version)
	if err != nil {
		return postgres.Client{}, input, err
	}
	input.BootstrapPassword = entry.Values["password"]
	if input.BootstrapPassword == "" {
		return postgres.Client{}, input, errors.New("PostgreSQL credential reference has no password")
	}
	return client, input, nil
}

func (manager Manager) resumePostgresOperations(ctx context.Context) error {
	if err := manager.migratePostgresPolicy(ctx); err != nil {
		return err
	}
	records, err := manager.database.List(ctx, "postgres-operation")
	if err != nil {
		return err
	}
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		if !operationFinished(operation.Status) {
			manager.startPostgresOperation(operation)
		}
	}
	manager.runtime.workers.Add(1)
	go func() { defer manager.runtime.workers.Done(); manager.postgresMaintenance() }()
	return nil
}

func (manager Manager) startPostgresOperation(operation Operation) {
	manager.runtime.workers.Add(1)
	go func() { defer manager.runtime.workers.Done(); manager.runPostgresOperation(operation) }()
}

func (manager Manager) runPostgresOperation(operation Operation) {
	select {
	case manager.runtime.postgresSlots <- struct{}{}:
		defer func() { <-manager.runtime.postgresSlots }()
	case <-manager.runtime.context.Done():
		return
	}
	task, _, err := loadDocument[postgresTask](manager.runtime.context, manager.database, "postgres-task", operation.ID)
	if err != nil {
		return
	}
	if !task.NextRetry.IsZero() {
		delay := time.Until(task.NextRetry)
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-manager.runtime.context.Done():
				return
			}
		}
	}
	if task.Action == "check" {
		manager.runPostgresCheck(operation, task)
		return
	}
	ctx, cancel := context.WithDeadline(manager.runtime.context, task.Deadline)
	defer cancel()
	stored, _, err := loadDocument[postgresRecord](manager.runtime.context, manager.database, "postgres", task.Key)
	if err != nil || stored.ActiveOperation != operation.ID {
		return
	}
	observed, workErr := manager.executePostgres(ctx, stored, task, operation)
	if manager.runtime.context.Err() != nil {
		return
	}
	for {
		if err := manager.finishPostgres(operation, task, observed, workErr); err == nil {
			return
		}
		if manager.waitForPoll(manager.runtime.context) != nil {
			return
		}
	}
}

func (manager Manager) executePostgres(ctx context.Context, stored postgresRecord, task postgresTask, operation Operation) (postgres.Observation, error) {
	observed := postgres.Observation{}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	client, input, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return observed, err
	}
	if task.Action == "delete" {
		if err := manager.removePostgresCredentials(ctx, stored, input); err != nil {
			return observed, err
		}
		for {
			complete, err := client.DeleteOwned(ctx, input)
			if err != nil {
				return observed, err
			}
			if complete {
				return observed, nil
			}
			if err := manager.waitForPoll(ctx); err != nil {
				return observed, err
			}
		}
	}
	if task.Action == "backup" {
		for {
			observed, err = client.Observe(ctx, input)
			if err == nil && observed.Ready && observed.PolicyReady {
				break
			}
			if err := manager.waitForPoll(ctx); err != nil {
				return observed, err
			}
		}
		if err := client.RequestBackup(ctx, input, operation.ID); err != nil {
			return observed, err
		}
		for {
			complete, err := client.BackupComplete(ctx, input, operation.ID)
			if err != nil {
				return observed, err
			}
			if complete {
				return client.Observe(ctx, input)
			}
			if err := manager.waitForPoll(ctx); err != nil {
				return observed, err
			}
		}
	}
	if task.Action == "rotate" {
		return manager.executePostgresRotation(ctx, stored, client, input, operation)
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return observed, err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return observed, err
	}

	manager.runtime.platformMutex.Lock()
	_, err = platform.ReconcilePostgresComponents(ctx, platform.TargetInput{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster, Organization: stored.Organization, Name: cluster.Name, ID: cluster.ID, Settings: settings, RecoverPending: true})
	manager.runtime.platformMutex.Unlock()
	if err != nil {
		return observed, err
	}
	client, input, err = manager.reconcilePostgresRuntime(ctx, stored, task)
	if err != nil {
		return observed, err
	}
	stored, _, err = loadDocument[postgresRecord](ctx, manager.database, "postgres", task.Key)
	if err != nil {
		return observed, err
	}

	for {
		observed, err = client.Observe(ctx, input)
		if err == nil && observed.Ready && observed.PolicyReady {
			input, err = manager.reconcilePostgresCredentials(ctx, stored, client, input)
			if err != nil {
				var dependency postgresCredentialError
				if !errors.As(err, &dependency) {
					return observed, err
				}
				operation.Status, operation.Message, operation.UpdatedAt = "waiting", dependency.message, time.Now().UTC()
				if err := manager.savePostgresOperation(ctx, operation); err != nil {
					return observed, err
				}
				if err := manager.waitForPoll(ctx); err != nil {
					return observed, dependency
				}
				continue
			}
			exported, exportErr := manager.postgresEnvironmentReady(ctx, stored, client, input)
			if exportErr != nil {
				return observed, exportErr
			}
			if exported {
				consumers, consumerErr := manager.refreshPostgresConsumers(ctx, stored)
				if consumerErr != nil {
					return observed, consumerErr
				}
				if consumers {
					if err := client.ActivateBackupSchedules(ctx, input); err != nil {
						return observed, err
					}
					if input.Recovery != nil {
						if err := client.RetireRecoveryAccess(ctx, input); err != nil {
							return observed, err
						}
					}
					return observed, nil
				}
			}
		}
		operation.Status = "waiting"
		operation.Message = "waiting for SQL, accepted backup policy, OpenBao credentials, and application credential refresh"
		operation.UpdatedAt = time.Now().UTC()
		if err := manager.savePostgresOperation(ctx, operation); err != nil {
			return observed, err
		}
		if err := manager.waitForPoll(ctx); err != nil {
			return observed, err
		}
	}
}

func (manager Manager) savePostgresOperation(ctx context.Context, operation Operation) error {
	_, record, err := loadDocument[Operation](ctx, manager.database, "postgres-operation", operation.ID)
	if err != nil {
		return err
	}
	write, err := documentWrite("postgres-operation", operation.ID, operation, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) finishPostgres(operation Operation, task postgresTask, observed postgres.Observation, workErr error) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", task.Key)
	if err != nil {
		return err
	}
	if stored.ActiveOperation != operation.ID {
		return nil
	}
	_, operationRecord, err := loadDocument[Operation](ctx, manager.database, "postgres-operation", operation.ID)
	if err != nil {
		return err
	}
	stored.ActiveOperation = ""
	if workErr == nil && (task.Action == "apply" || task.Action == "create" || task.Action == "restore") {
		stored.NeedsPolicyApply = false
	}
	operation.Status = "succeeded"
	stored.Postgres.State = "ready"
	stored.Postgres.Message = "PostgreSQL accepts SQL; backup protection is reported separately"
	stored.Postgres.Protection = postgresProtection(stored, observed, time.Now().UTC())
	stored.Postgres.Protection.RecoveryWindowDays = stored.Postgres.Specification.Backup.RecoveryWindowDays
	if task.Action == "delete" {
		stored.Postgres.State = "removed"
		stored.Postgres.Message = "PostgreSQL runtime and owned volumes removed; source archives preserved"
	}
	if workErr != nil {
		operation.Status = "failed"
		stored.Postgres.State = "failed"
		if observed.Ready {
			stored.Postgres.State = "ready"
		}
		stored.Postgres.Message = safePostgresError(workErr)
	}
	now := time.Now().UTC()
	if workErr == nil && task.Action != "delete" && stored.LastTestArchive != stored.ArchiveID && postgresBackupsEnabled(stored.Postgres.Specification) {
		stored.InitialCheck = operation.ID
	}
	stored.Postgres.UpdatedAt = now
	operation.Message = stored.Postgres.Message
	operation.UpdatedAt = now
	databaseWrite, err := documentWrite("postgres", task.Key, stored, record.Revision)
	if err != nil {
		return err
	}
	operationWrite, err := documentWrite("postgres-operation", operation.ID, operation, operationRecord.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{databaseWrite, operationWrite})
}

func safePostgresError(err error) string {
	var credentialFailure postgresCredentialError
	if errors.As(err, &credentialFailure) {
		return credentialFailure.message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "PostgreSQL operation exceeded its original durable deadline"
	}
	if message := PostgresRequestMessage(err); message != "" {
		return message
	}
	if errors.Is(err, ErrComponentsNotReady) {
		return "PostgreSQL components are not ready. Inspect cluster components or run cluster reconcile, then retry."
	}
	return "PostgreSQL operation failed. Check database events, cluster components, and shared storage access, then retry."
}

func (manager Manager) postgresMaintenance() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if manager.runtime.context.Err() != nil {
			return
		}
		records, err := manager.database.List(manager.runtime.context, "postgres")
		if err != nil {
			slog.ErrorContext(manager.runtime.context, "postgres_environment_scan_failed")
		}
		for _, record := range records {
			var stored postgresRecord
			if json.Unmarshal(record.Value, &stored) != nil || stored.Postgres.State == "removed" || stored.ActiveOperation != "" {
				continue
			}
			ctx, cancel := context.WithTimeout(manager.runtime.context, 30*time.Second)
			if stored.NeedsPolicyApply && stored.ActiveCheck == "" {
				_, _ = manager.PutPostgres(ctx, stored.Organization, stored.Space, stored.Postgres.Name, PostgresInput{}, true)
				cancel()
				continue
			}

			if err := manager.maintainPostgresEnvironment(ctx, record.Key, stored.ID); err != nil {
				slog.ErrorContext(ctx, "postgres_environment_reconciliation_failed", "postgres_id", stored.ID, "organization", stored.Organization, "space", stored.Space, "database", stored.Postgres.Name, "reason", safePostgresEnvironmentError(err))
			}
			client, input, err := manager.postgresClient(ctx, stored, false)
			if err == nil && input.BackupEnabled && stored.ActiveCheck == "" {
				_ = client.PruneSnapshots(ctx, input, 24*time.Hour)
				if stored.InitialCheck != "" {
					observed, err := client.Observe(ctx, input)
					if err == nil && observed.Ready && observed.PolicyReady {
						if observed.LastBackupAt.IsZero() {
							_ = client.RequestBackup(ctx, input, stored.ArchiveID)
						} else if observed.Backups == "ready" && observed.Wal == "ready" {
							_ = client.RequestCheck(ctx, input, stored.InitialCheck)
						}
					}
				}
			}
			cancel()
		}
		select {
		case <-manager.runtime.context.Done():
			return
		case <-ticker.C:
		}
	}
}

func (manager Manager) rotatePostgresStorage(ctx context.Context, before, after ManagementS3) error {
	config, err := manager.ResolveManagementS3(ctx, after)
	if err != nil {
		return err
	}
	records, err := manager.database.List(ctx, "postgres")
	if err != nil {
		return err
	}
	for _, record := range records {
		var stored postgresRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return err
		}
		if stored.Postgres.State == "removed" {
			continue
		}
		client, input, err := manager.postgresClient(ctx, stored, false)
		if err != nil {
			return err
		}
		input.Storage = postgres.Storage{AccessKey: config.Credentials.AccessKey, SecretKey: config.Credentials.SecretKey, SessionToken: config.Credentials.SessionToken, Region: config.Region}
		if err := client.RefreshStorage(ctx, input); err != nil {
			return err
		}
		if stored.ActiveCheck != "" {
			task, _, err := loadDocument[postgresTask](ctx, manager.database, "postgres-task", stored.ActiveCheck)
			if err != nil {
				return err
			}
			for _, check := range []struct{ name, owner string }{{task.CheckName, task.CheckID}, {task.CheckName + "-snap", task.CheckID + "s"}} {
				temporary := input
				temporary.Name, temporary.Owner = check.name, check.owner
				if err := client.RefreshStorage(ctx, temporary); err != nil {
					return err
				}
			}
		}
	}
	archives, err := manager.database.List(ctx, "postgres-archive")
	if err != nil {
		return err
	}
	writes := []sqlite.Write{}
	for _, record := range archives {
		var archive postgresArchive
		if err := json.Unmarshal(record.Value, &archive); err != nil {
			return err
		}
		if archive.CredentialsRef != before.CredentialsRef {
			continue
		}
		archive.CredentialsRef = after.CredentialsRef
		write, err := documentWrite("postgres-archive", record.Key, archive, record.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return fmt.Errorf("persist PostgreSQL storage credential generation: %w", err)
	}
	return nil
}

func (manager Manager) reconcilePostgresRuntime(ctx context.Context, stored postgresRecord, task postgresTask) (postgres.Client, postgres.Input, error) {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	current, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", task.Key)
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	if current.ID != stored.ID || current.ActiveOperation != stored.ActiveOperation || current.Postgres.State == "removed" || current.Postgres.State == "deleting" {
		return postgres.Client{}, postgres.Input{}, ErrConflict
	}
	target, err := manager.GetSpace(ctx, current.Organization, current.Space)
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	previousSnapshotClass := current.SnapshotClass
	current.SnapshotClass = ""
	if postgresBackupsEnabled(current.Postgres.Specification) {
		client, clusterInput, err := manager.postgresClient(ctx, current, false)
		if err != nil {
			return postgres.Client{}, postgres.Input{}, err
		}
		current.SnapshotClass, err = client.SnapshotClass(ctx, clusterInput.StorageClass, "")
		if err != nil {
			return postgres.Client{}, postgres.Input{}, err
		}
	}
	previousName := current.Postgres.EnvironmentGroup
	current, writes, err := manager.claimPostgresEnvironment(ctx, current, target)
	if err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	if previousName != current.Postgres.EnvironmentGroup || previousSnapshotClass != current.SnapshotClass {
		write, err := documentWrite("postgres", task.Key, current, record.Revision)
		if err != nil {
			return postgres.Client{}, postgres.Input{}, err
		}
		writes = append(writes, write)
	}
	if len(writes) > 0 {
		if err := manager.database.Commit(ctx, writes); err != nil {
			return postgres.Client{}, postgres.Input{}, translateStoreError(err)
		}
	}
	stored = current
	if err := manager.postgresEnvironmentRetention(ctx, stored); err != nil {
		return postgres.Client{}, postgres.Input{}, err
	}
	client, input, err := manager.postgresClient(ctx, stored, true)
	if err != nil {
		return client, input, err
	}
	if task.SourceArchive != "" {
		source, metadata, err := manager.revalidatePostgresBackup(ctx, stored.Organization, task)
		if err != nil {
			return client, input, err
		}
		storage, err := manager.recoveryArchiveStorage(ctx, source)
		if err != nil {
			return client, input, err
		}
		input.Recovery = &postgres.Recovery{Storage: storage, BackupID: metadata.ID, Target: task.RecoveryTarget}
	}
	return client, input, client.Reconcile(ctx, input)
}
