package apps

import (
	"context"
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
	for _, bad := range []PostgresInput{{Instances: -1}, {Storage: "0Gi"}, {Image: "postgres:latest"}, {Database: "app; DROP DATABASE postgres"}, {Username: "postgres"}, {Snapshots: "enabled"}, {Backup: PostgresBackupPolicy{Schedule: "0 0 0 * * *"}}, {RestoreCheck: PostgresRestoreCheckInput{Schedule: "0 0 0 * * *"}}, {Resources: PostgresResources{Requests: map[string]string{"cpu": "-1"}}}} {
		_, err := normalizePostgres(bad)
		check.True(err != nil)
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
