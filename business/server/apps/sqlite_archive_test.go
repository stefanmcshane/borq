package apps

import (
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
)

// TestSqliteArchiveKeyPersistence proves each workload keeps its vaulted age key
// through a real manager close/reopen, while SQLite records contain references
// only. A lost saved key cannot silently start a differently encrypted archive.
func TestSqliteArchiveKeyPersistence(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	cluster := newManagerCluster(t, managerClusterOptions{})
	target := createManagerEnvironment(t, manager, cluster, "archives")
	first, err := manager.sqliteArchiveKey(t.Context(), target, "web")
	assert.NoErr(err)
	assert.NoErr(first.Validate())
	second, err := manager.sqliteArchiveKey(t.Context(), target, "worker")
	assert.NoErr(err)
	assert.True(first.Identity != second.Identity)
	reference, record, err := loadDocument[string](t.Context(), manager.database, "sqlite-archive-key", target.ID+"/web")
	assert.NoErr(err)
	assert.True(!strings.Contains(string(record.Value), first.Identity))
	assert.True(!strings.Contains(string(record.Value), first.Recipient))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	retained, err := manager.sqliteArchiveKey(t.Context(), target, "web")
	assert.NoErr(err)
	assert.Equal(retained, first)
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", reference))
	_, err = manager.sqliteArchiveKey(t.Context(), target, "web")
	assert.True(err != nil && strings.Contains(err.Error(), "original vault entry"))
}

// TestSqliteArchiveKeyValidation prevents missing, swapped or malformed archive
// identities from reaching replication and never includes key material in errors.
func TestSqliteArchiveKeyValidation(t *testing.T) {
	assert := is.New(t)
	first, err := sqliteworkload.NewArchiveKey()
	assert.NoErr(err)
	second, err := sqliteworkload.NewArchiveKey()
	assert.NoErr(err)
	for _, key := range []sqliteworkload.ArchiveKey{{}, {Identity: first.Identity}, {Identity: first.Identity, Recipient: second.Recipient}, {Identity: "invalid", Recipient: first.Recipient}} {
		err := key.Validate()
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), first.Identity))
	}
}
