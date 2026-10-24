package apps

import (
	"context"
	"errors"
	"time"

	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) runPostgresCheck(operation Operation, task postgresTask) {
	stored, _, err := loadDocument[postgresRecord](manager.runtime.context, manager.database, "postgres", task.Key)
	if err != nil || stored.ActiveCheck != operation.ID {
		return
	}
	client, input, err := manager.postgresClient(manager.runtime.context, stored, false)
	if err != nil {
		manager.retryPostgresCheck(operation, task)
		return
	}
	sourceInput := input
	input.Name, input.Owner = task.CheckName, task.CheckID
	input.Instances = 1
	input.Check.Enabled = false
	input.SnapshotClass = ""
	input.Isolated = true
	if resources := stored.Postgres.Specification.RestoreCheck.Resources; len(resources.Requests) > 0 || len(resources.Limits) > 0 {
		input.Requests, input.Limits = resources.Requests, resources.Limits
	}
	snapshotInput := input
	snapshotInput.Name = task.CheckName + "-snap"
	snapshotInput.Owner = task.CheckID + "s"
	if !task.Verified {
		ctx, cancel := context.WithDeadline(manager.runtime.context, task.Deadline)

		source, backup, verificationErr := manager.revalidatePostgresBackup(ctx, stored.Organization, task)
		if verificationErr == nil {
			input.Recovery = &postgres.Recovery{BackupID: backup.ID}
			verificationErr = manager.reconcilePostgresCheck(ctx, stored, source, input)
		}

		operation.Status = "waiting"
		operation.Message = "recovering an isolated copy and checking actual SQL"
		operation.UpdatedAt = time.Now().UTC()
		_ = manager.savePostgresOperation(ctx, operation)
		if verificationErr == nil {
			verificationErr = manager.verifyPostgresSql(ctx, client, input)
		}
		task.SnapshotStatus = stored.Postgres.Protection.Snapshots
		if verificationErr == nil && stored.Postgres.Protection.Snapshots == "supported" {
			task.SnapshotStatus = "failed"
			verificationErr = client.RequestSnapshot(ctx, sourceInput, operation.ID)
			snapshotName := ""
			for verificationErr == nil && snapshotName == "" {
				snapshotName, verificationErr = client.CompletedSnapshot(ctx, sourceInput, operation.ID)
				if verificationErr == nil && snapshotName == "" {
					verificationErr = manager.waitForPoll(ctx)
				}
			}
			if verificationErr == nil {
				snapshotInput.Recovery = &postgres.Recovery{Storage: input.Recovery.Storage, SnapshotName: snapshotName}
				verificationErr = manager.reconcilePostgresCheck(ctx, stored, source, snapshotInput)
			}
			if verificationErr == nil {
				verificationErr = manager.verifyPostgresSql(ctx, client, snapshotInput)
			}
			if verificationErr == nil {
				task.SnapshotStatus = "succeeded"
			}
		}
		cancel()
		if manager.runtime.context.Err() != nil {
			return
		}
		task.Verified = true
		if verificationErr != nil {
			task.VerificationError = safePostgresError(verificationErr)
		}
		if manager.savePostgresTask(manager.runtime.context, operation.ID, task) != nil {
			manager.retryPostgresCheck(operation, task)
			return
		}
	}
	operation.Status = "waiting"
	operation.Message = "cleaning up isolated recovery resources and volumes"
	operation.UpdatedAt = time.Now().UTC()
	_ = manager.savePostgresOperation(manager.runtime.context, operation)
	cleanupCtx, cancel := context.WithTimeout(manager.runtime.context, 10*time.Minute)
	defer cancel()
	for {
		complete, cleanupErr := client.DeleteOwned(cleanupCtx, input)
		snapshotComplete, snapshotErr := client.DeleteOwned(cleanupCtx, snapshotInput)
		snapshotRequestComplete := true
		if complete && snapshotComplete && task.SnapshotStatus != "unsupported" && task.SnapshotStatus != "disabled" {
			snapshotRequestComplete, snapshotErr = client.CleanupSnapshotRequest(cleanupCtx, sourceInput, operation.ID)
		}
		if cleanupErr == nil && snapshotErr == nil && complete && snapshotComplete && snapshotRequestComplete {
			task.Cleanup = "succeeded"
			break
		}
		if manager.runtime.context.Err() != nil {
			return
		}
		if manager.waitForPoll(cleanupCtx) != nil {
			task.Cleanup = "pending"
			manager.retryPostgresCheck(operation, task)
			return
		}
	}
	if manager.savePostgresTask(manager.runtime.context, operation.ID, task) != nil {
		manager.retryPostgresCheck(operation, task)
		return
	}
	for {
		if manager.finishPostgresCheck(operation, task) == nil {
			return
		}
		if manager.waitForPoll(manager.runtime.context) != nil {
			return
		}
	}
}

func (manager Manager) verifyPostgresSql(ctx context.Context, client postgres.Client, input postgres.Input) error {
	for {
		observed, err := client.Observe(ctx, input)
		if err == nil && observed.Ready {
			_, err = client.Query(ctx, input.Namespace, input.Name, input.Database, `BEGIN; CREATE TEMP TABLE borq_restore_probe(value text NOT NULL); INSERT INTO borq_restore_probe VALUES ('borq restore ✓'); DO $$ BEGIN IF (SELECT value FROM borq_restore_probe) <> 'borq restore ✓' THEN RAISE EXCEPTION 'restore probe mismatch'; END IF; END $$; SELECT count(*) FROM pg_catalog.pg_class; ROLLBACK;`)
			return err
		}
		if err := manager.waitForPoll(ctx); err != nil {
			return err
		}
	}
}

func (manager Manager) savePostgresTask(ctx context.Context, id string, task postgresTask) error {
	_, record, err := loadDocument[postgresTask](ctx, manager.database, "postgres-task", id)
	if err != nil {
		return err
	}
	write, err := documentWrite("postgres-task", id, task, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) retryPostgresCheck(operation Operation, task postgresTask) {
	if manager.runtime.context.Err() != nil {
		return
	}
	task.NextRetry = time.Now().UTC().Add(time.Minute)
	if manager.savePostgresTask(manager.runtime.context, operation.ID, task) != nil {
		return
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if !manager.runtime.closing {
		manager.startPostgresOperation(operation)
	}
}

func (manager Manager) finishPostgresCheck(operation Operation, task postgresTask) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", task.Key)
	if err != nil {
		return err
	}
	if stored.ActiveCheck != operation.ID {
		return nil
	}
	_, operationRecord, err := loadDocument[Operation](ctx, manager.database, "postgres-operation", operation.ID)
	if err != nil {
		return err
	}
	if task.Cleanup != "succeeded" {
		return errors.New("restore-check cleanup has not completed")
	}
	stored.ActiveCheck = ""
	now := time.Now().UTC()
	status, message := "succeeded", "isolated recovery passed SQL read, write and rollback checks; temporary resources removed"
	if task.VerificationError != "" {
		status, message = "failed", task.VerificationError+"; temporary resources removed"
	}
	stored.Postgres.RestoreCheck = PostgresRestoreCheck{Status: status, Operation: operation.ID, Message: message, SnapshotStatus: task.SnapshotStatus, Cleanup: task.Cleanup, StartedAt: operation.StartedAt, CompletedAt: now, DurationSeconds: now.Sub(operation.StartedAt).Seconds()}
	operation.Status = status
	operation.Message = message
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

func (manager Manager) reconcilePostgresCheck(ctx context.Context, stored postgresRecord, source postgresArchive, input postgres.Input) error {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	client, credentials, err := manager.postgresClient(ctx, stored, true)
	if err != nil {
		return err
	}
	input.Password = credentials.Password
	source, _, err = loadDocument[postgresArchive](ctx, manager.database, "postgres-archive", source.ID)
	if err != nil {
		return err
	}
	input.Storage, err = manager.archiveStorage(ctx, source)
	if err != nil {
		return err
	}
	input.Recovery.Storage = input.Storage
	return client.Reconcile(ctx, input)
}
