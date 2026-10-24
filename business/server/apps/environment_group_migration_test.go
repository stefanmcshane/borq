package apps

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestEnvironmentGroupMigrationPreservesHistory exercises the real write, close,
// reopen and migration path without rewriting a saved plan or its vault versions.
func TestEnvironmentGroupMigrationPreservesHistory(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, path)
	is.NoErr(err)
	space := Space{ID: "existing-space", Organization: "acme", Name: "production", Namespace: "existing-namespace", Configuration: Configuration{Apps: []App{}}}
	savedPlan := map[string]any{"plan": map[string]any{"id": "reviewed", "manifests": []any{map[string]any{"metadata": map[string]any{"annotations": map[string]string{"borq.run/env-config": `{"name":"backend","version":"7"}`}}}}}}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
	}{
		{"env-config", space.ID + "/backend", map[string]string{"spaceId": space.ID, "name": "backend"}},
		{"space", "acme/production", map[string]any{"space": space, "envConfigPrefix": "shop-production"}},
		{"plan", "reviewed", savedPlan},
	} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		is.NoErr(err)
		writes = append(writes, write)
	}
	is.NoErr(database.Commit(ctx, writes))
	originalPlan, err := database.Get(ctx, "plan", "reviewed")
	is.NoErr(err)
	is.NoErr(database.Close())

	manager, err := NewManager(ctx, NewInput{DatabasePath: path})
	is.NoErr(err)
	t.Cleanup(func() { _ = manager.Close() })
	group, record, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", space.ID+"/backend")
	is.NoErr(err)
	is.Equal(group.SpaceID, space.ID)
	is.Equal(group.Name, "backend")
	is.Equal(group.VaultPath, "borq/env-configs/existing-space/backend")
	resolved, err := manager.resolveEnvironmentGroupPath(ctx, space.ID, "backend")
	is.NoErr(err)
	is.Equal(resolved, group.VaultPath)
	newPath, err := manager.resolveEnvironmentGroupPath(ctx, space.ID, "new-group")
	is.NoErr(err)
	is.Equal(newPath, "borq/environment-groups/existing-space/new-group")
	converted, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", "acme/production")
	is.NoErr(err)
	is.Equal(converted.EnvironmentGroupPrefix, "shop-production")
	plan, err := manager.database.Get(ctx, "plan", "reviewed")
	is.NoErr(err)
	is.Equal(plan, originalPlan)
	is.NoErr(manager.Close())

	reopened, err := NewManager(ctx, NewInput{DatabasePath: path})
	is.NoErr(err)
	t.Cleanup(func() { is.NoErr(reopened.Close()) })
	again, err := reopened.database.Get(ctx, "environment-group", space.ID+"/backend")
	is.NoErr(err)
	is.Equal(again, record)
}
