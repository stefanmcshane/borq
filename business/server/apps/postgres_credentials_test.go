package apps

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPostgresCredentialStatePersists protects enrollment progress and bootstrap retirement across a manager restart.
func TestPostgresCredentialStatePersists(t *testing.T) {
	check := is.New(t)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	manager := openTestManager(t, path, 0)
	key := "acme/production/orders"
	stored := postgresRecord{Organization: "acme", Space: "production", ID: "database-id", CredentialsRef: "managed-postgres/database-id#1", ActiveOperation: "enroll-operation", Postgres: Postgres{Name: "orders", State: "pending", Message: "waiting for database"}}
	write, err := documentWrite("postgres", key, stored, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	stored.Credentials = postgresCredentialState{Phase: "managed", Namespace: "borq-system", RootRotatedAt: time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC), KvMount: "borq"}
	stored.CredentialsRef = ""
	stored.Postgres.Message = "stale caller message"
	check.NoErr(manager.savePostgresCredentialState(t.Context(), stored))
	check.NoErr(manager.Close())
	reopened := openTestManager(t, path, 0)
	persisted, _, err := loadDocument[postgresRecord](t.Context(), reopened.database, "postgres", key)
	check.NoErr(err)
	check.Equal(persisted.Credentials, stored.Credentials)
	check.Equal(persisted.CredentialsRef, "")
	check.Equal(persisted.ActiveOperation, "enroll-operation")
	check.Equal(persisted.Postgres.Message, "waiting for database")
}

// TestPostgresCredentialStateRejectsStaleWrites prevents enrollment from reviving replaced, deleted, or newly owned database work.
func TestPostgresCredentialStateRejectsStaleWrites(t *testing.T) {
	for _, change := range []string{"replaced identity", "changed operation", "deleting", "removed"} {
		t.Run(change, func(t *testing.T) {
			check := is.New(t)
			manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), 0)
			key := "acme/production/orders"
			stale := postgresRecord{Organization: "acme", Space: "production", ID: "database-id", ActiveOperation: "old-operation", CredentialsRef: "managed-postgres/database-id#1", Postgres: Postgres{Name: "orders", State: "pending"}}
			current := stale
			switch change {
			case "replaced identity":
				current.ID = "replacement-id"
			case "changed operation":
				current.ActiveOperation = "new-operation"
			default:
				current.Postgres.State = change
			}
			write, err := documentWrite("postgres", key, current, 0)
			check.NoErr(err)
			check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			stale.Credentials.Phase = "managed"
			stale.CredentialsRef = ""
			check.True(errors.Is(manager.savePostgresCredentialState(t.Context(), stale), ErrConflict))
			persisted, _, err := loadDocument[postgresRecord](t.Context(), manager.database, "postgres", key)
			check.NoErr(err)
			check.Equal(persisted, current)
		})
	}
}
