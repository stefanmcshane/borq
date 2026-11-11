package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSpaceStorageMigration preserves legacy archives across real reopen while
// preventing current and previously removed SQLite apps from receiving a new-file grant.
func TestSpaceStorageMigration(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, filename)
	assert.NoErr(err)
	legacy := initialSpace("acme", "legacy")
	legacy.Configuration.Apps = []App{{Name: "current", Sqlite: SqliteSettings{Enabled: true}}, {Name: "pending", Sqlite: SqliteSettings{Enabled: true}}}
	retained := ManagementS3{Endpoint: "https://old-storage.example", Region: "us-east-1", Bucket: "shared-sqlite", Prefix: "backups", CredentialsRef: "borq/platform/rustfs", PathStyle: true}
	empty := initialSpace("acme", "empty")
	modern := initialSpace("acme", "modern")
	modernWrite, err := newSpaceStorageWrite(modern)
	assert.NoErr(err)
	writes := []sqlite.Write{modernWrite}
	for _, item := range []struct {
		collection, key string
		value           any
	}{
		{"organization", "acme", Organization{Name: "acme"}},
		{"space", "acme/legacy", spaceRecord{Space: legacy, SqliteStorage: retained}},
		{"space", "acme/empty", spaceRecord{Space: empty}},
		{"space", "acme/modern", spaceRecord{Space: modern}},
		{"sqlite-initialization", legacy.ID + "/pending", "pending"},
		{"plan", "old-apply", planRecord{Plan: Plan{ID: "old-apply", Space: legacy, Configuration: Configuration{Apps: []App{{Name: "removed", Sqlite: SqliteSettings{Enabled: true}}}}}}},
		{"operation", "old-apply", Operation{ID: "old-apply", PlanID: "old-apply", SpaceID: legacy.ID, Status: "succeeded"}},
		{"plan", "draft", planRecord{Plan: Plan{ID: "draft", Space: legacy, Configuration: Configuration{Apps: []App{{Name: "draft", Sqlite: SqliteSettings{Enabled: true}}}}}}},
	} {
		write, err := documentWrite(item.collection, item.key, item.value, 0)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	assert.NoErr(database.Commit(ctx, writes))
	assert.NoErr(database.Close())
	input := NewInput{DatabasePath: filename}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	storage, first, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", legacy.ID)
	assert.NoErr(err)
	assert.Equal(storage.Status.State, "pending")
	assert.True(storage.Storage.Bucket != retained.Bucket)
	unused, _, err := loadDocument[spaceStorageRecord](ctx, manager.database, "space-storage", empty.ID)
	assert.NoErr(err)
	assert.True(unused.Storage.Bucket != storage.Storage.Bucket)
	planned, err := manager.plannedSpaceStorage(ctx, empty)
	assert.NoErr(err)
	assert.Equal(planned.Bucket, unused.Storage.Bucket)
	current, err := manager.currentSqliteStorage(ctx, legacy)
	assert.NoErr(err)
	assert.Equal(current, retained)
	sources, err := manager.sqliteBackupSources(ctx)
	assert.NoErr(err)
	assert.Equal(sources, []ManagementS3{retained})
	for name, expected := range map[string]string{"current": "active", "removed": "active", "pending": "pending"} {
		state, _, err := loadDocument[string](ctx, manager.database, "sqlite-initialization", legacy.ID+"/"+name)
		assert.NoErr(err)
		assert.Equal(state, expected)
	}
	_, err = manager.database.Get(ctx, "sqlite-initialization", legacy.ID+"/draft")
	assert.True(errors.Is(err, sqlite.ErrNotFound))
	unchanged, err := manager.database.Get(ctx, "space-storage", modern.ID)
	assert.NoErr(err)
	assert.Equal(string(unchanged.Value), string(modernWrite.Value))
	assert.Equal(unchanged.Revision, int64(1))
	cluster := newManagerCluster(t, managerClusterOptions{})
	client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	configuration := Configuration{Apps: []App{{Name: "current", Sqlite: SqliteSettings{Enabled: true}}, {Name: "fresh", Sqlite: SqliteSettings{Enabled: true}}}}
	assert.NoErr(manager.prepareSqliteRuntime(ctx, client, planRecord{Plan: Plan{Space: legacy, Configuration: configuration}}))
	cluster.mutex.Lock()
	oldGrant := manifestText(cluster.resources["/api/v1/namespaces/"+legacy.Namespace+"/configmaps/current-sqlite-state"], "data", "state")
	newGrant := manifestText(cluster.resources["/api/v1/namespaces/"+legacy.Namespace+"/configmaps/fresh-sqlite-state"], "data", "state")
	cluster.mutex.Unlock()
	assert.Equal(oldGrant, "initialized")
	assert.Equal(newGrant, "new")
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer reopened.Close()
	again, after, err := loadDocument[spaceStorageRecord](ctx, reopened.database, "space-storage", legacy.ID)
	assert.NoErr(err)
	assert.Equal(again, storage)
	assert.Equal(after.Revision, first.Revision)
	current, err = reopened.currentSqliteStorage(ctx, legacy)
	assert.NoErr(err)
	assert.Equal(current, retained)
}

// TestLegacyBackupCredentials exercises retained archive credential selection at
// the vault/S3 boundary. It verifies requested scopes, not simulated S3 enforcement.
func TestLegacyBackupCredentials(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	fixture := newManagementStorageFixture(t)
	upstream, err := url.Parse(fixture.s3.URL)
	assert.NoErr(err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	policies := make(chan string, 2)
	storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/rustfs/admin/v3/info-service-account"):
			writer.WriteHeader(http.StatusNotFound)
		case request.URL.Path == "/rustfs/admin/v3/add-service-account":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			policies <- string(body["policy"])
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead:
			writer.WriteHeader(http.StatusOK)
		default:
			proxy.ServeHTTP(writer, request)
		}
	}))
	defer storage.Close()
	filename := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, filename)
	assert.NoErr(err)
	space := initialSpace("acme", "legacy")
	retained := ManagementS3{Endpoint: storage.URL, Region: "us-east-1", Bucket: "shared-sqlite", Prefix: "retained", CredentialsRef: "borq/platform/rustfs", PathStyle: true}
	write, err := documentWrite("space", "acme/legacy", spaceRecord{Space: space, SqliteStorage: retained}, 0)
	assert.NoErr(err)
	assert.NoErr(database.Commit(ctx, []sqlite.Write{write}))
	assert.NoErr(database.Close())
	manager, err := NewManager(ctx, NewInput{DatabasePath: filename, ManagementEnabled: true, InfrastructureEnabled: true, ManagementKubeconfigPath: fixture.kubeconfig,
		PlatformSettings: platform.Settings{Namespace: "borq", Profile: "dev", S3Endpoint: storage.URL, OpenBaoURL: "http://vault.test", TelemetryURL: "http://borq.test"}})
	assert.NoErr(err)
	defer manager.Close()
	vault, err := platform.ManagementVault(ctx, fixture.kubeconfig, "borq")
	assert.NoErr(err)
	_, err = vault.WriteKv(ctx, "secret", retained.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": "old-root", "RUSTFS_SECRET_KEY": "old-secret"}})
	assert.NoErr(err)
	saved := planRecord{Plan: Plan{Space: space, Configuration: Configuration{Apps: []App{{Name: "web", Sqlite: SqliteSettings{Enabled: true, Backups: true}}}}}, SqliteStorage: retained}
	secrets, err := manager.sqliteBackupCredentials(ctx, saved)
	assert.NoErr(err)
	assert.Equal(len(secrets), 1)
	assert.True(manifestText(secrets[0], "stringData", "AWS_SECRET_ACCESS_KEY") != "old-secret")
	sqlitePolicy := <-policies
	assert.True(strings.Contains(sqlitePolicy, "arn:aws:s3:::shared-sqlite/retained/sqlite/"+space.ID+"/web/*"))
	assert.True(strings.Contains(sqlitePolicy, "s3:PutObject"))
	archive := postgresArchive{Endpoint: storage.URL, Region: retained.Region, Bucket: "old-management", Prefix: "organizations/owner/postgres", PathStyle: true, CredentialsRef: retained.CredentialsRef}
	recovery, err := manager.recoveryArchiveStorage(ctx, archive)
	assert.NoErr(err)
	assert.Equal(recovery.Bucket, archive.Bucket)
	assert.True(recovery.AccessKey != "old-root" && recovery.SecretKey != "old-secret")
	postgresPolicy := <-policies
	assert.True(strings.Contains(postgresPolicy, "arn:aws:s3:::old-management/organizations/owner/postgres/*"))
	assert.True(strings.Contains(postgresPolicy, "s3:GetObject"))
	assert.True(!strings.Contains(postgresPolicy, "s3:PutObject"))
}
