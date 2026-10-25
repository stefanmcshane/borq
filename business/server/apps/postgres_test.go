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
