package apps

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPostgresDefaults protects safe sizing, mandatory backup, and automatic verification defaults.
func TestPostgresDefaults(t *testing.T) {
	check := is.New(t)
	input, err := normalizePostgres(PostgresInput{})
	check.NoErr(err)
	check.Equal(input.Instances, 1)
	check.Equal(input.Storage, "1Gi")
	check.Equal(input.Backup.Schedule, "0 0 * * *")
	check.Equal(input.Backup.RecoveryWindowDays, 30)
	check.True(*input.Backup.Enabled)
	check.Equal(input.Backup.RestoreCheck.Schedule, "0 3 * * 0")
	check.Equal(input.Backup.RestoreCheck.TimeoutSeconds, 1800)
	check.Equal(input.Credentials, PostgresCredentialPolicy{RotationPeriod: "720h", Port: 5432})
	for _, bad := range []struct {
		input   PostgresInput
		message string
	}{
		{PostgresInput{Instances: -1}, "PostgreSQL instances must be between 1 and 16"},
		{PostgresInput{Storage: "0Gi"}, "PostgreSQL storage must be a positive Kubernetes quantity"},
		{PostgresInput{Image: "postgres:latest"}, "PostgreSQL image must pin a supported 14–18 major and exact minor tag"},
		{PostgresInput{Database: "app; DROP DATABASE postgres"}, "database and application username must be lowercase SQL identifiers; postgres and borq_openbao are reserved"},
		{PostgresInput{Username: "postgres"}, "database and application username must be lowercase SQL identifiers; postgres and borq_openbao are reserved"},
		{PostgresInput{Username: "borq_openbao"}, "database and application username must be lowercase SQL identifiers; postgres and borq_openbao are reserved"},
		{PostgresInput{Backup: PostgresBackupPolicy{Schedule: "0 0 0 * * *"}}, "backup schedule requires five cron fields"},
		{PostgresInput{Backup: PostgresBackupPolicy{RestoreCheck: PostgresRestoreCheckInput{Schedule: "0 0 0 * * *"}}}, "restore check schedule requires five cron fields"},
		{PostgresInput{Resources: PostgresResources{Requests: map[string]string{"cpu": "-1"}}}, "PostgreSQL resource quantities must be positive"},
	} {
		_, err := normalizePostgres(bad.input)
		check.True(errors.Is(err, ErrInvalid))
		check.Equal(PostgresRequestMessage(err), bad.message)
	}
	disabled := false
	input, err = normalizePostgres(PostgresInput{Backup: PostgresBackupPolicy{Enabled: &disabled, RecoveryWindowDays: 60}})
	check.NoErr(err)
	check.True(!*input.Backup.Enabled)
	check.Equal(input.Backup.RecoveryWindowDays, 60)
	source, err := normalizePostgres(PostgresInput{Storage: "20Gi", Database: "orders", Username: "orders", Backup: PostgresBackupPolicy{RecoveryWindowDays: 60}})
	check.NoErr(err)
	merged, err := normalizePostgres(mergePostgresInput(source, PostgresInput{Instances: 2, Backup: PostgresBackupPolicy{Enabled: &disabled}}))
	check.NoErr(err)
	check.Equal(merged.Storage, "20Gi")
	check.Equal(merged.Database, "orders")
	check.Equal(merged.Username, "orders")
	check.Equal(merged.Backup.RecoveryWindowDays, 60)
	check.Equal(merged.Instances, 2)
	check.True(!*merged.Backup.Enabled)

}

// TestPostgresCredentialPolicy validates rotation bounds and the private SQL route before accepting database changes.
func TestPostgresCredentialPolicy(t *testing.T) {
	for _, policy := range []PostgresCredentialPolicy{
		{RotationPeriod: "1m", Host: "pg.internal", Port: 1},
		{RotationPeriod: "61s", Host: "pg-rw.production.svc", Port: 5432},
		{RotationPeriod: "1.5m", Host: "private-pg.internal", Port: 65535},
		{RotationPeriod: "720h"},
	} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			check := is.New(t)
			input, err := normalizePostgres(PostgresInput{Credentials: policy})
			check.NoErr(err)
			check.Equal(input.Credentials.RotationPeriod, policy.RotationPeriod)
			check.Equal(input.Credentials.Host, policy.Host)
			if policy.Port != 0 {
				check.Equal(input.Credentials.Port, policy.Port)
			}
		})
	}
	for _, invalid := range []struct {
		policy  PostgresCredentialPolicy
		message string
	}{
		{PostgresCredentialPolicy{RotationPeriod: "disabled"}, "credential rotationPeriod must be a whole-second duration between 1m and 720h"},
		{PostgresCredentialPolicy{RotationPeriod: "0s"}, "credential rotationPeriod must be a whole-second duration between 1m and 720h"},
		{PostgresCredentialPolicy{RotationPeriod: "59s"}, "credential rotationPeriod must be a whole-second duration between 1m and 720h"},
		{PostgresCredentialPolicy{RotationPeriod: "60.5s"}, "credential rotationPeriod must be a whole-second duration between 1m and 720h"},
		{PostgresCredentialPolicy{RotationPeriod: "720h1s"}, "credential rotationPeriod must be a whole-second duration between 1m and 720h"},
		{PostgresCredentialPolicy{Port: -1}, "credential port must be between 1 and 65535"},
		{PostgresCredentialPolicy{Port: 65536}, "credential port must be between 1 and 65535"},
		{PostgresCredentialPolicy{Host: "postgresql://pg.internal"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "pg.internal:5432"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "pg internal"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "pg..internal"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "-pg"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "10.0.0.5"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
		{PostgresCredentialPolicy{Host: "2001:db8::1"}, "credential host must be a private SQL DNS hostname reachable from OpenBao"},
	} {
		t.Run(fmt.Sprint(invalid.policy), func(t *testing.T) {
			check := is.New(t)
			_, err := normalizePostgres(PostgresInput{Credentials: invalid.policy})
			check.True(errors.Is(err, ErrInvalid))
			check.Equal(PostgresRequestMessage(err), invalid.message)
		})
	}
}

// TestPostgresCredentialPolicyMerge preserves omitted credential settings when applying a partial database update.
func TestPostgresCredentialPolicyMerge(t *testing.T) {
	for _, test := range []struct {
		name   string
		patch  PostgresCredentialPolicy
		wanted PostgresCredentialPolicy
	}{
		{"omitted", PostgresCredentialPolicy{}, PostgresCredentialPolicy{RotationPeriod: "2h", Host: "old.internal", Port: 6432}},
		{"period", PostgresCredentialPolicy{RotationPeriod: "1m"}, PostgresCredentialPolicy{RotationPeriod: "1m", Host: "old.internal", Port: 6432}},
		{"host", PostgresCredentialPolicy{Host: "new.internal"}, PostgresCredentialPolicy{RotationPeriod: "2h", Host: "new.internal", Port: 6432}},
		{"port", PostgresCredentialPolicy{Port: 5432}, PostgresCredentialPolicy{RotationPeriod: "2h", Host: "old.internal", Port: 5432}},
		{"all", PostgresCredentialPolicy{RotationPeriod: "3h", Host: "new.internal", Port: 7432}, PostgresCredentialPolicy{RotationPeriod: "3h", Host: "new.internal", Port: 7432}},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			previous, err := normalizePostgres(PostgresInput{Credentials: PostgresCredentialPolicy{RotationPeriod: "2h", Host: "old.internal", Port: 6432}})
			check.NoErr(err)
			merged, err := normalizePostgres(mergePostgresInput(previous, PostgresInput{Credentials: test.patch}))
			check.NoErr(err)
			check.Equal(merged.Credentials, test.wanted)
		})
	}
}

// TestPostgresRestartDeadline proves durable write-close-reopen recovery without resetting an original deadline.
func TestPostgresRestartDeadline(t *testing.T) {
	check := is.New(t)
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	manager, err := NewManager(ctx, NewInput{DatabasePath: databasePath, PollInterval: time.Millisecond})
	check.NoErr(err)
	id := "0123456789abcdef0123456789abcdef"
	now := time.Now().UTC()
	deadline := now.Add(-time.Minute)
	key := "acme/production/db"
	stored := postgresRecord{Organization: "acme", Space: "production", Cluster: "missing", ID: "resource-original", ArchiveID: "archive-original", ActiveOperation: id, Postgres: Postgres{ID: "resource-original", Name: "db", State: "pending", Operation: id}}
	task := postgresTask{Key: key, Action: "create", Deadline: deadline}
	operation := Operation{ID: id, Status: "pending", StartedAt: now.Add(-time.Hour), Phase: "postgres-create"}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
	}{{"postgres", key, stored}, {"postgres-task", id, task}, {"postgres-operation", id, operation}, {"postgres-archive", "archive-original", postgresArchive{ID: "archive-original", ResourceID: "resource-original"}}} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		check.NoErr(err)
		writes = append(writes, write)
	}
	check.NoErr(manager.database.Commit(ctx, writes))
	check.NoErr(manager.Close())
	manager, err = NewManager(ctx, NewInput{DatabasePath: databasePath, PollInterval: time.Millisecond, RolloutTimeout: time.Hour})
	check.NoErr(err)
	defer manager.Close()
	expires := time.Now().Add(3 * time.Second)
	for {
		operation, err = manager.GetPostgresOperation(ctx, id)
		check.NoErr(err)
		if operation.Status == "failed" {
			break
		}
		if time.Now().After(expires) {
			t.Fatal("resumed operation did not terminate at its original deadline")
		}
		time.Sleep(time.Millisecond)
	}
	persisted, _, err := loadDocument[postgresTask](ctx, manager.database, "postgres-task", id)
	check.NoErr(err)
	check.True(persisted.Deadline.Equal(deadline))
	check.True(operation.StartedAt.Equal(now.Add(-time.Hour)))
	archive, _, err := loadDocument[postgresArchive](ctx, manager.database, "postgres-archive", "archive-original")
	check.NoErr(err)
	check.Equal(archive.ResourceID, "resource-original")
}

// TestPostgresSafeErrors keeps private dependency diagnostics out of public and
// durable operation messages, including when they wrap approved guidance.
func TestPostgresSafeErrors(t *testing.T) {
	check := is.New(t)
	_, invalid := normalizePostgres(PostgresInput{Storage: "private-vault-token"})
	for _, input := range []struct {
		err     error
		message string
	}{
		{fmt.Errorf("private-vault-token: %w", invalid), "PostgreSQL storage must be a positive Kubernetes quantity"},
		{fmt.Errorf("private-vault-token: %w", context.DeadlineExceeded), "PostgreSQL operation exceeded its original durable deadline"},
		{fmt.Errorf("private-vault-token: %w", ErrComponentsNotReady), "PostgreSQL components are not ready. Inspect cluster components or run cluster reconcile, then retry."},
		{fmt.Errorf("private-vault-token: %w", ErrInvalid), "PostgreSQL operation failed. Check database events, cluster components, and shared storage access, then retry."},
		{errors.New("https://user:private-vault-token@internal.example"), "PostgreSQL operation failed. Check database events, cluster components, and shared storage access, then retry."},
	} {
		check.Equal(safePostgresError(input.err), input.message)
	}
	check.Equal(PostgresRequestMessage(fmt.Errorf("private-vault-token: %w", ErrInvalid)), "")
}

// TestPostgresExistingName preserves the conflict category and actionable guidance
// for both create and restore when the destination is already reserved.
func TestPostgresExistingName(t *testing.T) {
	check := is.New(t)
	ctx := context.Background()
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), 0)
	spaceWrite, err := documentWrite("space", "acme/production", spaceRecord{Space: Space{Name: "production"}}, 0)
	check.NoErr(err)
	postgresWrite, err := documentWrite("postgres", "acme/production/db", postgresRecord{Postgres: Postgres{Name: "db", State: "ready"}}, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(ctx, []sqlite.Write{spaceWrite, postgresWrite}))
	for _, action := range []string{"create", "restore"} {
		_, err := manager.acceptPostgres(ctx, "acme", "production", "db", PostgresInput{}, action, postgresTask{})
		check.True(errors.Is(err, ErrConflict))
		check.Equal(err.Error(), "A PostgreSQL database with this name already exists in this space. Choose a new name to create or restore a database, or use apply to update the existing database.")
		check.Equal(PostgresRequestMessage(fmt.Errorf("private-vault-token: %w", err)), err.Error())
	}
}

// TestPostgresBackupPolicyMerge preserves cadence and restore-test settings across disable/resume patches.
func TestPostgresBackupPolicyMerge(t *testing.T) {
	check := is.New(t)
	disabled, enabled := false, true
	original, err := normalizePostgres(PostgresInput{Backup: PostgresBackupPolicy{Schedule: "15 2 * * *", RecoveryWindowDays: 45, RestoreCheck: PostgresRestoreCheckInput{Schedule: "0 4 * * 2", TimeoutSeconds: 900, Resources: PostgresResources{Requests: map[string]string{"memory": "256Mi"}}}}})
	check.NoErr(err)
	paused, err := normalizePostgres(mergePostgresInput(original, PostgresInput{Backup: PostgresBackupPolicy{Enabled: &disabled}}))
	check.NoErr(err)
	check.True(!postgresBackupsEnabled(paused))
	resumed, err := normalizePostgres(mergePostgresInput(paused, PostgresInput{Backup: PostgresBackupPolicy{Enabled: &enabled}}))
	check.NoErr(err)
	check.Equal(resumed, original)
	omitted := mergePostgresInput(paused, PostgresInput{Instances: 2})
	check.True(!postgresBackupsEnabled(omitted))
	check.Equal(omitted.Backup.RestoreCheck, original.Backup.RestoreCheck)
}

// TestPostgresProtection requires current-generation evidence and keeps the last success during bounded recurring work.
func TestPostgresProtection(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	specification, err := normalizePostgres(PostgresInput{})
	is.New(t).NoErr(err)
	success := now.Add(-time.Hour)
	for _, test := range []struct {
		name, archive, latest, cleanup, expected string
		checked                                  time.Time
		disabled                                 bool
	}{
		{name: "never", expected: "unverified"},
		{name: "previous archive", archive: "old", checked: success, latest: "succeeded", expected: "unverified"},
		{name: "verified", archive: "current", checked: success, latest: "succeeded", expected: "ready"},
		{name: "recurring running", archive: "current", checked: success, latest: "pending", cleanup: "pending", expected: "ready"},
		{name: "failed", archive: "current", checked: success, latest: "failed", expected: "degraded"},
		{name: "overdue", archive: "current", checked: now.Add(-8 * 24 * time.Hour), latest: "succeeded", expected: "degraded"},
		{name: "cleanup failed", archive: "current", checked: success, latest: "succeeded", cleanup: "failed", expected: "degraded"},
		{name: "disabled", disabled: true, expected: "disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			stored := postgresRecord{ArchiveID: "current", LastTestArchive: test.archive, LastTestAt: test.checked, Postgres: Postgres{Specification: specification, RestoreCheck: PostgresRestoreCheck{Status: test.latest, Cleanup: test.cleanup, StartedAt: now.Add(-time.Minute)}}}
			if test.disabled {
				enabled := false
				stored.Postgres.Specification.Backup.Enabled = &enabled
			}
			observed := postgres.Observation{Backups: "ready", Wal: "ready", LastBackupAt: now.Add(-2 * time.Hour)}
			check.Equal(postgresProtection(stored, observed, now).Status, test.expected)
		})
	}
}

// TestPostgresPolicyMigrationRestart proves old tuning survives actual write/close/reopen and mandatory apply is durable.
func TestPostgresPolicyMigrationRestart(t *testing.T) {
	check := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	input := componentManagerInput(t, writeComponentKubeconfig(t, "https://unused.invalid"))
	input.DatabasePath = path
	manager, err := NewManager(ctx, input)
	check.NoErr(err)
	legacy := []byte(`{"organization":"acme","space":"production","id":"resource","archiveId":"archive","postgres":{"name":"db","state":"ready","specification":{"backup":{"schedule":"15 2 * * *","recoveryWindowDays":45},"restoreCheck":{"enabled":false,"schedule":"0 4 * * 2","timeoutSeconds":900,"resources":{"requests":{"memory":"256Mi"}}},"snapshots":"disabled"}}}`)
	write, err := documentWrite("space", "acme/production", spaceRecord{Space: Space{ID: "space-id", Name: "production", Organization: "acme"}}, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(ctx, []sqlite.Write{write, {Collection: "postgres", Key: "acme/production/db", Value: legacy}}))
	check.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	check.NoErr(err)
	check.NoErr(manager.migratePostgresPolicy(ctx))
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", "acme/production/db")
	check.NoErr(err)
	operationID := stored.ActiveOperation
	check.True(operationID != "")
	check.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	check.NoErr(err)
	defer manager.Close()
	check.NoErr(manager.migratePostgresPolicy(ctx))
	stored, _, err = loadDocument[postgresRecord](ctx, manager.database, "postgres", "acme/production/db")
	check.NoErr(err)
	check.True(postgresBackupsEnabled(stored.Postgres.Specification))
	check.Equal(stored.Postgres.Specification.Backup.Schedule, "15 2 * * *")
	check.Equal(stored.Postgres.Specification.Backup.RestoreCheck, PostgresRestoreCheckInput{Schedule: "0 4 * * 2", TimeoutSeconds: 900, Resources: PostgresResources{Requests: map[string]string{"memory": "256Mi"}}})
	check.Equal(stored.ActiveOperation, operationID)
	operation, err := manager.GetPostgresOperation(ctx, operationID)
	check.NoErr(err)
	check.Equal(operation.SpaceID, "space-id")
}

// TestPostgresVerificationEvidenceRestart proves durable current-archive success survives newer attempts and process restart.
func TestPostgresVerificationEvidenceRestart(t *testing.T) {
	check := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	input := componentManagerInput(t, writeComponentKubeconfig(t, "https://unused.invalid"))
	input.DatabasePath = path
	manager, err := NewManager(ctx, input)
	check.NoErr(err)
	specification, err := normalizePostgres(PostgresInput{})
	check.NoErr(err)
	key, operationID := "acme/production/db", "test-operation"
	started := time.Now().UTC().Add(-time.Minute)
	stored := postgresRecord{PolicyVersion: 1, ArchiveID: "current", ActiveCheck: operationID, Postgres: Postgres{Specification: specification}}
	operation := Operation{ID: operationID, StartedAt: started, Status: "waiting"}
	for _, item := range []struct {
		collection, key string
		value           any
	}{{"postgres", key, stored}, {"postgres-operation", operationID, operation}} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		check.NoErr(err)
		check.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	}
	check.NoErr(manager.finishPostgresCheck(operation, postgresTask{Key: key, SourceArchive: "current", Cleanup: "succeeded"}))
	check.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	check.NoErr(err)
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	check.NoErr(err)
	check.Equal(stored.LastTestArchive, "current")
	check.True(stored.LastTestAt.After(started))
	lastSuccess := stored.LastTestAt
	// Accept a later attempt, then finish it with a recovery failure.
	failedOperation := Operation{ID: "failed-check", Status: "pending", StartedAt: time.Now().UTC()}
	stored.ActiveCheck = failedOperation.ID
	stored.Postgres.RestoreCheck = PostgresRestoreCheck{Status: "pending"}
	write, err := documentWrite("postgres", key, stored, record.Revision)
	check.NoErr(err)
	operationWrite, err := documentWrite("postgres-operation", failedOperation.ID, failedOperation, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(ctx, []sqlite.Write{write, operationWrite}))
	check.NoErr(manager.finishPostgresCheck(failedOperation, postgresTask{Key: key, SourceArchive: "current", Cleanup: "succeeded", VerificationError: "restore failed"}))
	stored, record, err = loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	check.NoErr(err)
	observed := postgres.Observation{Backups: "ready", Wal: "ready"}
	check.Equal(postgresProtection(stored, observed, time.Now().UTC()).Status, "degraded")
	// A retry replaces active-attempt display, while completed failure remains durable.
	retry := Operation{ID: "retry-check", Status: "pending", StartedAt: time.Now().UTC()}
	stored.ActiveCheck = retry.ID
	stored.Postgres.RestoreCheck = PostgresRestoreCheck{Status: "pending"}
	write, err = documentWrite("postgres", key, stored, record.Revision)
	check.NoErr(err)
	operationWrite, err = documentWrite("postgres-operation", retry.ID, retry, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(ctx, []sqlite.Write{write, operationWrite}))
	check.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	check.NoErr(err)
	defer manager.Close()
	stored, _, err = loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	check.NoErr(err)
	check.Equal(stored.Postgres.RestoreCheck.Status, "pending")
	check.True(stored.LastTestAt.Equal(lastSuccess))
	check.Equal(postgresProtection(stored, observed, time.Now().UTC()).Status, "degraded")
	check.NoErr(manager.finishPostgresCheck(retry, postgresTask{Key: key, SourceArchive: "current", Cleanup: "succeeded"}))
	stored, _, err = loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	check.NoErr(err)
	check.Equal(postgresProtection(stored, observed, time.Now().UTC()).Status, "ready")
	next, err := nextPostgresArchive(postgresArchive{ID: "current", Prefix: "backups/current", ServerName: "archive-current"}, specification)
	check.NoErr(err)
	check.True(next.ID != "current")
	check.Equal(next.Prefix, "backups/"+next.ID)
	stored.ArchiveID = next.ID
	check.Equal(postgresProtection(stored, postgres.Observation{Backups: "ready", Wal: "ready"}, time.Now().UTC()).Status, "unverified")
}
