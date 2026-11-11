package apps

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSqliteStorageAcceptance rejects first-use plans made obsolete by a storage move or rotation.
func TestSqliteStorageAcceptance(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	input := managerPlanInput(t, "production", `{"apps":[{"name":"web","image":"example/app","env":{"version":"0"},"sqlite":{"enabled":true}}]}`)
	plan, err := manager.Plan(t.Context(), input)
	assert.NoErr(err)
	storage, err := manager.GetManagementS3(t.Context())
	assert.NoErr(err)
	storage.Endpoint = "https://replacement.example"
	_, err = manager.activateManagementS3(t.Context(), storage)
	assert.NoErr(err)
	_, err = manager.Apply(t.Context(), plan.ID)
	assert.True(errors.Is(err, ErrConflict) && strings.Contains(err.Error(), "SQLite backup storage changed"))
	space, err := manager.GetSpace(t.Context(), "acme", "production")
	assert.NoErr(err)
	assert.Equal(space.Revision, 0)
	fresh, err := manager.Plan(t.Context(), input)
	assert.NoErr(err)
	rotation, record, err := loadDocument[storageRotation](t.Context(), manager.database, "management-s3-rotation", "shared")
	assert.NoErr(err)
	rotation.Status = "staged"
	write, err := documentWrite("management-s3-rotation", "shared", rotation, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.Apply(t.Context(), fresh.ID)
	assert.True(errors.Is(err, ErrConflict) && strings.Contains(err.Error(), "rotation is still pending"))
	assert.Equal(cluster.namespaceAttempts(), 0)
}

// TestSqliteProvisioningFailure leaves first-use storage editable when its runtime probe fails.
func TestSqliteProvisioningFailure(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Profile: "dev", Namespace: "borq"}})
	assert.NoErr(err)
	defer manager.Close()
	createManagerEnvironment(t, manager, cluster, "production")
	storage, err := manager.prepareManagementS3(t.Context(), ManagementS3Input{Endpoint: fixture.s3.URL, Region: "us-east-1", Bucket: "primary", PathStyle: true, AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	assert.NoErr(err)
	write, err := documentWrite("management-s3", "shared", storage, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"web","image":"example/app","env":{"version":"0"},"sqlite":{"enabled":true}}]}`))
	assert.NoErr(err)
	_, err = manager.Apply(t.Context(), plan.ID)
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "SQLite backup storage could not be provisioned"))
	retained, _, err := loadDocument[spaceRecord](t.Context(), manager.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	assert.Equal(retained.Space.Revision, 0)
	assert.Equal(retained.SqliteStorage, ManagementS3{})
	assert.Equal(len(retained.Space.Configuration.Apps), 0)
	storage.Endpoint = "https://corrected.example"
	_, err = manager.activateManagementS3(t.Context(), storage)
	assert.NoErr(err)
}
