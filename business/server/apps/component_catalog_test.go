package apps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestComponentCatalogSavedContext reads reopened overrides and registration state without cluster access or queued work.
func TestComponentCatalogSavedContext(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "catalog.sqlite"), PlatformSettings: platform.Settings{ApiUrl: "https://management.example"}}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, "team")
	assert.NoErr(err)
	fixture := newManagerCluster(t, managerClusterOptions{})
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: "team", Name: "remote", KubeconfigPath: fixture.kubeconfig})
	assert.NoErr(err)
	assert.NoErr(manager.putComponentOverrides(ctx, map[string]platform.ComponentOverride{"alloy": {Version: "9.8.7", Values: map[string]any{"saved": "custom"}}}))
	assert.NoErr(manager.Close())
	fixture.server.Close()
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	defer manager.Close()
	before := map[string][]sqlite.Record{}
	for _, collection := range []string{"management-components", "management-task", "management-operation", "components"} {
		records, err := manager.database.List(ctx, collection)
		assert.NoErr(err)
		before[collection] = records
	}
	catalog, err := manager.GetComponentCatalog(ctx, platform.ComponentScopeTarget, "team", "remote")
	assert.NoErr(err)
	assert.Equal(catalog.ClusterID, platform.ClusterID("team", "remote"))
	var found bool
	for _, component := range catalog.Components {
		if component.Name == "alloy" {
			found = true
			assert.Equal(component.Version, "9.8.7")
			assert.Equal(component.Values["saved"], "custom")
		}
	}
	assert.True(found)
	for collection, expected := range before {
		records, err := manager.database.List(ctx, collection)
		assert.NoErr(err)
		assert.Equal(records, expected)
	}
	for _, test := range []struct {
		scope                 platform.ComponentScope
		organization, cluster string
		want                  error
	}{
		{platform.ComponentScopeTarget, "", "", ErrInvalid},
		{platform.ComponentScopeTarget, "team", "", ErrInvalid},
		{platform.ComponentScopeTarget, "missing", "remote", ErrNotFound},
		{platform.ComponentScopeTarget, "team", "missing", ErrNotFound},
		{"unknown", "", "", ErrInvalid},
	} {
		_, err := manager.GetComponentCatalog(ctx, test.scope, test.organization, test.cluster)
		assert.True(errors.Is(err, test.want))
	}
}
