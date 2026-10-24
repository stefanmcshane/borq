package apps

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
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
	check.Equal(input.Snapshots, "preferred")
	check.Equal(input.SnapshotRetentionHours, 24)
	check.Equal(input.RestoreCheck.Schedule, "0 3 * * 0")
	check.Equal(input.RestoreCheck.TimeoutSeconds, 1800)
	for _, bad := range []struct {
		input   PostgresInput
		message string
	}{
		{PostgresInput{Instances: -1}, "PostgreSQL instances must be between 1 and 16"},
		{PostgresInput{Storage: "0Gi"}, "PostgreSQL storage must be a positive Kubernetes quantity"},
		{PostgresInput{Image: "postgres:latest"}, "PostgreSQL image must pin a supported 14–18 major and exact minor tag"},
		{PostgresInput{Database: "app; DROP DATABASE postgres"}, "database and application username must be lowercase SQL identifiers; postgres is reserved"},
		{PostgresInput{Username: "postgres"}, "database and application username must be lowercase SQL identifiers; postgres is reserved"},
		{PostgresInput{Snapshots: "enabled"}, "snapshots must be preferred, required, or disabled"},
		{PostgresInput{Backup: PostgresBackupPolicy{Schedule: "0 0 0 * * *"}}, "full backup schedule requires five cron fields"},
		{PostgresInput{RestoreCheck: PostgresRestoreCheckInput{Schedule: "0 0 0 * * *"}}, "restore check schedule requires five cron fields"},
		{PostgresInput{Resources: PostgresResources{Requests: map[string]string{"cpu": "-1"}}}, "PostgreSQL resource quantities must be positive"},
	} {
		_, err := normalizePostgres(bad.input)
		check.True(errors.Is(err, ErrInvalid))
		check.Equal(PostgresRequestMessage(err), bad.message)
	}
	disabled := false
	input, err = normalizePostgres(PostgresInput{RestoreCheck: PostgresRestoreCheckInput{Enabled: &disabled}, Snapshots: "disabled", Backup: PostgresBackupPolicy{RecoveryWindowDays: 60}})
	check.NoErr(err)
	check.True(!*input.RestoreCheck.Enabled)
	check.Equal(input.Backup.RecoveryWindowDays, 60)
	source, err := normalizePostgres(PostgresInput{Storage: "20Gi", Database: "orders", Username: "orders", Backup: PostgresBackupPolicy{RecoveryWindowDays: 60}})
	check.NoErr(err)
	merged, err := normalizePostgres(mergePostgresInput(source, PostgresInput{Instances: 2, RestoreCheck: PostgresRestoreCheckInput{Enabled: &disabled}}))
	check.NoErr(err)
	check.Equal(merged.Storage, "20Gi")
	check.Equal(merged.Database, "orders")
	check.Equal(merged.Username, "orders")
	check.Equal(merged.Backup.RecoveryWindowDays, 60)
	check.Equal(merged.Instances, 2)
	check.True(!*merged.RestoreCheck.Enabled)

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
