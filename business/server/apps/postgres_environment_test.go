package apps

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPostgresEnvironmentClaims rejects existing manual families and historical paths rather than adopting values.
func TestPostgresEnvironmentClaims(t *testing.T) {
	for _, collision := range []string{"manual catalog", "legacy catalog", "foreign producer", "uncatalogued history", "uncatalogued legacy history"} {
		t.Run(collision, func(t *testing.T) {
			check := is.New(t)
			manager, _, vault, target := newEnvironmentGroupManager(t)
			stored := postgresRecord{Organization: "acme", Space: "production", ID: "0123456789abcdef0123456789abcdef", Postgres: Postgres{Name: "orders"}}
			name := "postgres-orders-0123456789abcdef0123456789abcdef"
			path := environmentGroupPath(target.ID, name)
			if strings.HasPrefix(collision, "uncatalogued") {
				if collision == "uncatalogued legacy history" {
					path = legacyEnvironmentGroupPath(target.ID, name)
				}
				vault.mutex.Lock()
				vault.versions[path] = []map[string]string{{"existing": "history"}}
				vault.mutex.Unlock()
			} else {
				record := environmentGroupRecord{SpaceID: target.ID, Name: name}
				if collision == "legacy catalog" {
					record.VaultPath = legacyEnvironmentGroupPath(target.ID, name)
				}
				if collision == "foreign producer" {
					record.PostgresID = "another-database"
					record.PostgresName = "orders"
					record.VaultPath = path
				}
				write, err := documentWrite("environment-group", target.ID+"/"+name, record, 0)
				check.NoErr(err)
				check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			}
			_, _, err := manager.claimPostgresEnvironment(t.Context(), stored, target)
			check.True(errors.Is(err, ErrConflict))
			check.Equal(vault.writes(), 0)
		})
	}
}

// TestPostgresEnvironmentReservationIsAtomic proves a competing manual claim aborts database acceptance in the same transaction.
func TestPostgresEnvironmentReservationIsAtomic(t *testing.T) {
	check := is.New(t)
	manager, _, _, target := newEnvironmentGroupManager(t)
	stored := postgresRecord{Organization: "acme", Space: "production", ID: "first-incarnation", Postgres: Postgres{Name: "orders", State: "pending"}}
	stored, writes, err := manager.claimPostgresEnvironment(t.Context(), stored, target)
	check.NoErr(err)
	check.Equal(stored.Postgres.EnvironmentGroup, "postgres-orders-first-incarnation")
	check.NoErr(manager.registerEnvironmentGroup(t.Context(), target.ID, stored.Postgres.EnvironmentGroup))
	databaseWrite, err := documentWrite("postgres", "acme/production/orders", stored, 0)
	check.NoErr(err)
	err = manager.database.Commit(t.Context(), append(writes, databaseWrite))
	check.True(errors.Is(err, sqlite.ErrConflict))
	_, _, err = loadDocument[postgresRecord](t.Context(), manager.database, "postgres", "acme/production/orders")
	check.True(errors.Is(err, ErrNotFound))
}

// TestPostgresEnvironmentReadOnlyAndClone proves ownership is authoritative for current and historical reads and every manual write.
func TestPostgresEnvironmentReadOnlyAndClone(t *testing.T) {
	check := is.New(t)
	manager, cluster, vault, target := newEnvironmentGroupManager(t)
	stored := postgresRecord{Organization: "acme", Space: "production", ID: "first-incarnation", Postgres: Postgres{Name: "orders", State: "ready"}}
	stored, writes, err := manager.claimPostgresEnvironment(t.Context(), stored, target)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(t.Context(), writes))
	_, repeated, err := manager.claimPostgresEnvironment(t.Context(), stored, target)
	check.NoErr(err)
	check.Equal(len(repeated), 0)
	vault.mutex.Lock()
	vault.versions[environmentGroupPath(target.ID, stored.Postgres.EnvironmentGroup)] = []map[string]string{{"PGHOST": "old"}, {"PGHOST": "current"}}
	vault.mutex.Unlock()
	for _, version := range []string{"1", "latest"} {
		group, err := manager.GetEnvironmentGroup(t.Context(), environmentGroupTestTarget(), stored.Postgres.EnvironmentGroup, version)
		check.NoErr(err)
		check.Equal(group.ManagedBy, "postgres/orders")
	}
	cluster.mutex.Lock()
	token := cluster.resources["/api/v1/namespaces/borq-system/secrets/openbao-token"]
	delete(cluster.resources, "/api/v1/namespaces/borq-system/secrets/openbao-token")
	cluster.mutex.Unlock()
	_, err = manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: environmentGroupTestTarget(), Name: stored.Postgres.EnvironmentGroup, Values: []Variable{{Name: "PGHOST", Value: "overwrite"}}})
	check.True(errors.Is(err, ErrConflict))
	cluster.mutex.Lock()
	cluster.resources["/api/v1/namespaces/borq-system/secrets/openbao-token"] = token
	cluster.mutex.Unlock()
	_, err = manager.CloneEnvironmentGroup(t.Context(), CloneEnvironmentGroupInput{Target: environmentGroupTestTarget(), Name: stored.Postgres.EnvironmentGroup, SourceName: stored.Postgres.EnvironmentGroup, SourceVersion: "1"})
	check.True(errors.Is(err, ErrConflict))
	clone, err := manager.CloneEnvironmentGroup(t.Context(), CloneEnvironmentGroupInput{Target: environmentGroupTestTarget(), Name: "orders-custom", SourceName: stored.Postgres.EnvironmentGroup, SourceVersion: "1"})
	check.NoErr(err)
	check.Equal(clone.ManagedBy, "")
	check.Equal(clone.Values, []Variable{{Name: "PGHOST", Value: "old"}})
	_, err = manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: environmentGroupTestTarget(), Name: "orders-custom", Values: []Variable{{Name: "PGHOST", Value: "editable"}}})
	check.NoErr(err)
	recreated := stored
	recreated.ID = "second-incarnation"
	recreated.Postgres.EnvironmentGroup = ""
	recreated, writes, err = manager.claimPostgresEnvironment(t.Context(), recreated, target)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(t.Context(), writes))
	check.Equal(recreated.Postgres.EnvironmentGroup, "postgres-orders-second-incarnation")
	old, err := manager.GetEnvironmentGroup(t.Context(), environmentGroupTestTarget(), stored.Postgres.EnvironmentGroup, "1")
	check.NoErr(err)
	check.Equal(old.Values, []Variable{{Name: "PGHOST", Value: "old"}})
}

// TestPostgresEnvironmentBackfillPersists proves export-only repair writes catalog and database identity durably before retrying a missing source.
func TestPostgresEnvironmentBackfillPersists(t *testing.T) {
	check := is.New(t)
	_, cluster, vault, _ := newEnvironmentGroupManager(t)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	manager := openTestManager(t, path, time.Second)
	target := createManagerEnvironment(t, manager, cluster, "production")
	key := "acme/production/orders"
	input, err := normalizePostgres(PostgresInput{})
	check.NoErr(err)
	stored := postgresRecord{Organization: "acme", Space: "production", Cluster: "local", ID: "existing-database", Postgres: Postgres{ID: "existing-database", Name: "orders", Namespace: target.Namespace, ClusterName: "pg-existing", State: "ready", Specification: input}}
	write, err := documentWrite("postgres", key, stored, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	err = manager.maintainPostgresEnvironment(t.Context(), key, stored.ID)
	check.True(err != nil && strings.Contains(err.Error(), "application credential is unavailable"))
	check.Equal(vault.retention(), 11)
	check.Equal(vault.writes(), 0)
	check.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Second)
	persisted, _, err := loadDocument[postgresRecord](t.Context(), reopened.database, "postgres", key)
	check.NoErr(err)
	check.Equal(persisted.Postgres.EnvironmentGroup, "postgres-orders-existing-database")
	claim, _, err := loadDocument[environmentGroupRecord](t.Context(), reopened.database, "environment-group", target.ID+"/"+persisted.Postgres.EnvironmentGroup)
	check.NoErr(err)
	check.Equal(claim.PostgresID, stored.ID)
	check.Equal(claim.PostgresName, "orders")
	check.Equal(claim.VaultPath, environmentGroupPath(target.ID, persisted.Postgres.EnvironmentGroup))
}

// TestPostgresEnvironmentStaleMaintenance excludes accepted deletion, busy work, removed state, and replaced identities before any external access.
func TestPostgresEnvironmentStaleMaintenance(t *testing.T) {
	for _, state := range []string{"deleting", "removed", "active operation", "active check", "replaced identity"} {
		t.Run(state, func(t *testing.T) {
			check := is.New(t)
			manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), 0)
			stored := postgresRecord{Organization: "missing", Space: "missing", ID: "identity", Postgres: Postgres{Name: "orders", State: "ready"}}
			switch state {
			case "active operation":
				stored.ActiveOperation = "pending"
			case "active check":
				stored.ActiveCheck = "pending"
			case "replaced identity":
				stored.ID = "new-identity"
			default:
				stored.Postgres.State = state
			}
			write, err := documentWrite("postgres", "missing/missing/orders", stored, 0)
			check.NoErr(err)
			check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			check.NoErr(manager.maintainPostgresEnvironment(t.Context(), "missing/missing/orders", "identity"))
			records, err := manager.database.List(t.Context(), "environment-group")
			check.NoErr(err)
			check.Equal(len(records), 0)
		})
	}
}

// TestPostgresEnvironmentFailedDeletionKeepsIntent prevents maintenance from restarting a stopped producer after teardown fails.
func TestPostgresEnvironmentFailedDeletionKeepsIntent(t *testing.T) {
	check := is.New(t)
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), 0)
	key := "missing/missing/orders"
	stored := postgresRecord{Organization: "missing", Space: "missing", ID: "identity", ActiveOperation: "delete-operation", Postgres: Postgres{Name: "orders", State: "deleting", Operation: "delete-operation"}}
	task := postgresTask{Key: key, Action: "delete"}
	operation := Operation{ID: "delete-operation", Status: "waiting", Phase: "postgres-delete"}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
	}{{"postgres", key, stored}, {"postgres-task", operation.ID, task}, {"postgres-operation", operation.ID, operation}} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		check.NoErr(err)
		writes = append(writes, write)
	}
	check.NoErr(manager.database.Commit(t.Context(), writes))
	check.NoErr(manager.finishPostgres(operation, task, postgres.Observation{}, errors.New("runtime teardown failed")))
	failed, _, err := loadDocument[postgresRecord](t.Context(), manager.database, "postgres", key)
	check.NoErr(err)
	check.Equal(failed.Postgres.State, "failed")
	check.Equal(failed.ActiveOperation, "")
	check.NoErr(manager.maintainPostgresEnvironment(t.Context(), key, stored.ID))
	records, err := manager.database.List(t.Context(), "environment-group")
	check.NoErr(err)
	check.Equal(len(records), 0)
	deleting, err := manager.postgresDeletionAccepted(t.Context(), failed)
	check.NoErr(err)
	check.True(deleting)
}
