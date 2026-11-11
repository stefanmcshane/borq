package apps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestLegacyHierarchyMigrationPreservesDeploymentArtifacts verifies grouping
// changes cannot alter pinned manifests, vault identities, or the durable queue.
func TestLegacyHierarchyMigrationPreservesDeploymentArtifacts(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	database, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "borq.sqlite"))
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(database.Close()) })
	manager := Manager{database: database}
	previous := legacySpace{Space: Space{ID: "original-identity", Organization: "acme", Name: "production", Cluster: "local", Namespace: "existing-namespace", CertificateIssuer: "borq", Revision: 4, LastSuccessfulRevision: 2, Configuration: Configuration{Apps: []App{}}}, Project: "shop"}
	manifests := []map[string]any{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "unchanged", "namespace": "existing-namespace"}}}
	snapshots := []EnvSnapshot{{AppName: "backend", ConfigName: "shop-production-backend", Path: "borq/env-configs/original-identity/shop-production-backend", Version: 7, SnapshotID: "original-identity/shop-production-backend/7"}}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
	}{
		{"project", "acme/shop", map[string]string{"organization": "acme", "name": "shop"}},
		{"cluster", "acme/local", Cluster{Organization: "acme", Name: "local"}},
		{"environment", "acme/shop/production", map[string]any{"environment": previous, "manifests": manifests, "envSnapshots": snapshots, "activeOperation": "first", "queue": []string{"second"}}},
		{"plan", "first", map[string]any{"plan": map[string]any{"id": "first", "environment": previous, "manifests": manifests, "baseRevision": 3}, "environmentSecrets": []plannedEnv{{Snapshot: snapshots[0]}}}},
		{"operation", "first", map[string]string{"id": "first", "planId": "first", "environmentId": previous.ID, "status": "waiting"}},
		{"namespace", "acme/local/existing-namespace", map[string]string{"environmentId": previous.ID}},
		{"env-config", previous.ID + "/shop-production-backend", map[string]string{"environmentId": previous.ID, "name": "shop-production-backend"}},
	} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		is.NoErr(err)
		writes = append(writes, write)
	}
	is.NoErr(database.Commit(ctx, writes))
	is.NoErr(manager.migrateLegacyHierarchy(ctx))
	spaces, err := manager.ListSpaces(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(spaces), 1)
	converted, record, err := loadDocument[spaceRecord](ctx, database, "space", "acme/shop-production")
	is.NoErr(err)
	is.Equal(converted.Space.ID, previous.ID)
	is.Equal(converted.Space.Namespace, previous.Namespace)
	is.Equal(converted.Space.Revision, 4)
	is.Equal(converted.Space.LastSuccessfulRevision, 2)
	is.Equal(converted.Manifests, manifests)
	is.Equal(converted.EnvSnapshots, snapshots)
	is.Equal(converted.ActiveOperation, "first")
	is.Equal(converted.Queue, []string{"second"})
	is.Equal(converted.EnvironmentGroupPrefix, "shop-production")
	saved, _, err := loadDocument[planRecord](ctx, database, "plan", "first")
	is.NoErr(err)
	is.Equal(saved.Plan.Space.ID, previous.ID)
	is.Equal(saved.Plan.Space.Name, "shop-production")
	is.Equal(saved.Plan.BaseRevision, 3)
	is.Equal(saved.Plan.Manifests, manifests)
	is.Equal(saved.EnvironmentSecrets[0].Snapshot, snapshots[0])
	operation, _, err := loadDocument[Operation](ctx, database, "operation", "first")
	is.NoErr(err)
	is.Equal(operation.SpaceID, previous.ID)
	is.NoErr(manager.migrateLegacyHierarchy(ctx))
	unchanged, err := database.Get(ctx, "space", "acme/shop-production")
	is.NoErr(err)
	is.Equal(unchanged.Revision, record.Revision)
}

// TestManagerMigratesLegacyHierarchyOnOpen checks the upgrade runs before workers
// read persistent deployment targets and survives another server restart.
func TestManagerMigratesLegacyHierarchyOnOpen(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, path)
	is.NoErr(err)
	previous := legacySpace{Space: Space{ID: "old-id", Organization: "acme", Name: "dev", Namespace: "kept", Configuration: Configuration{Apps: []App{}}}, Project: "demo"}
	write, err := documentWrite("environment", "acme/demo/dev", map[string]any{"environment": previous, "manifests": []any{}}, 0)
	is.NoErr(err)
	is.NoErr(database.Commit(ctx, []sqlite.Write{write}))
	is.NoErr(database.Close())
	manager, err := NewManager(ctx, NewInput{DatabasePath: path})
	is.NoErr(err)
	converted, err := manager.GetSpace(ctx, "acme", "demo-dev")
	is.NoErr(err)
	is.Equal(converted.ID, "old-id")
	is.NoErr(manager.Close())
	reopened, err := NewManager(ctx, NewInput{DatabasePath: path})
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(reopened.Close()) })
	again, err := reopened.GetSpace(ctx, "acme", "demo-dev")
	is.NoErr(err)
	is.Equal(again, converted)
}

// TestLegacySpaceNameCollisionsPreserveBothTargets ensures flattening project
// boundaries and collision suffixes never merge independently deployed spaces.
func TestLegacySpaceNameCollisionsPreserveBothTargets(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	database, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "borq.sqlite"))
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(database.Close()) })
	writes := []sqlite.Write{}
	for _, old := range []legacySpace{
		{Space: Space{ID: "first", Organization: "acme", Name: "b-c", Namespace: "one"}, Project: "a"},
		{Space: Space{ID: "second", Organization: "acme", Name: "c", Namespace: "two"}, Project: "a-b"},
		{Space: Space{ID: "third", Organization: "acme", Name: "b-c-" + stableId("first")[:10], Namespace: "three"}, Project: "a"},
	} {
		value, err := json.Marshal(map[string]any{"environment": old})
		is.NoErr(err)
		writes = append(writes, sqlite.Write{Collection: "environment", Key: old.Organization + "/" + old.Project + "/" + old.Name, Value: value})
	}
	is.NoErr(database.Commit(ctx, writes))
	manager := Manager{database: database}
	is.NoErr(manager.migrateLegacyHierarchy(ctx))
	spaces, err := manager.ListSpaces(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(spaces), 3)
	identities := map[string]string{}
	for _, space := range spaces {
		is.Equal(identities[space.Name], "")
		identities[space.Name] = space.ID
		is.NoErr(validateNames(space.Name))
	}
	is.Equal(identities["a-b-c-"+stableId("first")[:10]], "third")
}
