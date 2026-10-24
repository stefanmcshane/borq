package apps

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestUnusedManagementSpaceRetirement proves startup archives only untouched,
// unreferenced placeholders and preserves that decision across database reopens.
func TestUnusedManagementSpaceRetirement(t *testing.T) {
	for _, scenario := range []string{"unused", "applications", "revision", "custom namespace", "variables", "addon", "plan", "deployment history", "different namespace owner", "explicit space"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "borq.sqlite")
			database, err := sqlite.New(ctx, path)
			assert.NoErr(err)
			stored := spaceRecord{Space: initialSpace("acme", "management"), Manifests: []map[string]any{}}
			stored.Space.Cluster = "management"
			bindingID := stored.Space.ID
			switch scenario {
			case "applications":
				stored.Space.Configuration.Apps = []App{{Name: "keep", Image: "nginx"}}
			case "revision":
				stored.Space.Revision = 1
			case "custom namespace":
				stored.Space.Namespace = "existing-namespace"
			case "different namespace owner":
				bindingID = "someone-else"
			case "explicit space":
				stored.Space.Name = "default"
			}
			key := spaceKey(stored.Space.Organization, stored.Space.Name)
			bindingKey := "acme/management/" + stored.Space.Namespace
			writes := []sqlite.Write{}
			for _, item := range []struct {
				collection, key string
				value           any
			}{
				{"organization", "acme", Organization{Name: "acme"}},
				{"cluster", "acme/management", Cluster{Organization: "acme", Name: "management"}},
				{"space", key, stored},
				{"namespace", bindingKey, map[string]string{"spaceId": bindingID}},
			} {
				write, err := documentWrite(item.collection, item.key, item.value, 0)
				assert.NoErr(err)
				writes = append(writes, write)
			}
			var collection, referenceKey string
			var reference any
			switch scenario {
			case "variables":
				collection, referenceKey, reference = "environment-group", stored.Space.ID+"/config", environmentGroupRecord{SpaceID: stored.Space.ID, Name: "config"}
			case "addon":
				collection, referenceKey, reference = "addon", "chart", addonRecord{Organization: "acme", Space: "management"}
			case "plan":
				collection, referenceKey, reference = "plan", "reviewed", planRecord{Plan: Plan{ID: "reviewed", Space: stored.Space}}
			case "deployment history":
				collection, referenceKey, reference = "operation", "previous", Operation{ID: "previous", SpaceID: stored.Space.ID, Status: "failed"}
			}
			if collection != "" {
				write, err := documentWrite(collection, referenceKey, reference, 0)
				assert.NoErr(err)
				writes = append(writes, write)
			}
			assert.NoErr(database.Commit(ctx, writes))
			assert.NoErr(database.Close())
			for range 2 {
				manager := openTestManager(t, path, 0)
				space, err := manager.GetSpace(ctx, "acme", stored.Space.Name)
				binding, bindingError := manager.database.Get(ctx, "namespace", bindingKey)
				archived, archiveError := manager.database.Get(ctx, "archived-space", key)
				if scenario == "unused" {
					assert.True(errors.Is(err, ErrNotFound))
					assert.True(errors.Is(bindingError, sqlite.ErrNotFound))
					assert.NoErr(archiveError)
					var original spaceRecord
					assert.NoErr(json.Unmarshal(archived.Value, &original))
					assert.Equal(original, stored)
				} else {
					assert.NoErr(err)
					assert.Equal(space, stored.Space)
					assert.NoErr(bindingError)
					var owner map[string]string
					assert.NoErr(json.Unmarshal(binding.Value, &owner))
					assert.Equal(owner["spaceId"], bindingID)
					assert.True(errors.Is(archiveError, sqlite.ErrNotFound))
				}
				assert.NoErr(manager.Close())
			}
		})
	}
}
