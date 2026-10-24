package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/matryer/is"
)

// TestPersistence keeps accepted state and revision identities intact across server restarts.
func TestPersistence(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "borq.sqlite")
	database, err := New(ctx, path)
	assert.NoErr(err)
	assert.NoErr(database.Commit(ctx, []Write{{Collection: "environment", Key: "one", Value: json.RawMessage(`{"revision":1}`)}}))
	for _, suffix := range []string{"", "-wal", "-shm", ".lock"} {
		information, err := os.Stat(path + suffix)
		assert.NoErr(err)
		assert.Equal(information.Mode().Perm(), os.FileMode(0o600))
	}
	assert.NoErr(database.Close())
	database, err = New(ctx, path)
	assert.NoErr(err)
	t.Cleanup(func() { _ = database.Close() })
	record, err := database.Get(ctx, "environment", "one")
	assert.NoErr(err)
	assert.Equal(record.Revision, int64(1))
	assert.Equal(string(record.Value), `{"revision":1}`)
	information, err := os.Stat(path)
	assert.NoErr(err)
	assert.Equal(information.Mode().Perm(), os.FileMode(0o600))
	_, err = database.Get(ctx, "environment", "missing")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestAtomicRevisionChecks prevents a losing apply from leaving half of its operation recorded.
func TestAtomicRevisionChecks(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	database, err := New(ctx, filepath.Join(t.TempDir(), "borq.sqlite"))
	assert.NoErr(err)
	t.Cleanup(func() { _ = database.Close() })
	assert.NoErr(database.Commit(ctx, []Write{{Collection: "environment", Key: "one", Value: json.RawMessage(`{}`)}}))
	err = database.Commit(ctx, []Write{
		{Collection: "operation", Key: "loser", Value: json.RawMessage(`{}`)},
		{Collection: "environment", Key: "one", ExpectedRevision: 2, Value: json.RawMessage(`{}`)},
	})
	assert.True(errors.Is(err, ErrConflict))
	_, err = database.Get(ctx, "operation", "loser")
	assert.True(errors.Is(err, ErrNotFound))
	assert.NoErr(database.Commit(ctx, []Write{
		{Collection: "operation", Key: "winner", Value: json.RawMessage(`{}`)},
		{Collection: "environment", Key: "one", ExpectedRevision: 1, Value: json.RawMessage(`{"accepted":true}`)},
	}))
	record, err := database.Get(ctx, "environment", "one")
	assert.NoErr(err)
	assert.Equal(record.Revision, int64(2))
	assert.Equal(string(record.Value), `{"accepted":true}`)
	records, err := database.List(ctx, "operation")
	assert.NoErr(err)
	assert.Equal(len(records), 1)
	assert.Equal(records[0].Key, "winner")
	err = database.Commit(ctx, []Write{
		{Collection: "operation", Key: "rolled-back-delete", Value: json.RawMessage(`{}`)},
		{Collection: "environment", Key: "one", ExpectedRevision: 1, Delete: true},
	})
	assert.True(errors.Is(err, ErrConflict))
	_, err = database.Get(ctx, "operation", "rolled-back-delete")
	assert.True(errors.Is(err, ErrNotFound))
	assert.NoErr(database.Commit(ctx, []Write{{Collection: "environment", Key: "one", ExpectedRevision: 2, Delete: true}}))
	_, err = database.Get(ctx, "environment", "one")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestConcurrentCompareAndSwap proves concurrent callers cannot accept the same base revision twice.
func TestConcurrentCompareAndSwap(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	database, err := New(ctx, filepath.Join(t.TempDir(), "borq.sqlite"))
	assert.NoErr(err)
	t.Cleanup(func() { _ = database.Close() })
	assert.NoErr(database.Commit(ctx, []Write{{Collection: "environment", Key: "one", Value: json.RawMessage(`{}`)}}))
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- database.Commit(ctx, []Write{{Collection: "environment", Key: "one", ExpectedRevision: 1, Value: json.RawMessage(`{}`)}})
		}()
	}
	workers.Wait()
	close(results)
	successes, conflicts := 0, 0
	for result := range results {
		if result == nil {
			successes++
			continue
		}
		assert.True(errors.Is(result, ErrConflict))
		conflicts++
	}
	assert.Equal(successes, 1)
	assert.Equal(conflicts, 1)
}

// TestInvalidDocumentsFailWithoutWrites prevents malformed requests from corrupting stored snapshots.
func TestInvalidDocumentsFailWithoutWrites(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	database, err := New(ctx, filepath.Join(t.TempDir(), "borq.sqlite"))
	assert.NoErr(err)
	t.Cleanup(func() { _ = database.Close() })
	for _, write := range []Write{
		{Collection: "", Key: "one", Value: json.RawMessage(`{}`)},
		{Collection: "environment", Key: "", Value: json.RawMessage(`{}`)},
		{Collection: "environment", Key: "one", Value: json.RawMessage(`bad`)},
		{Collection: "environment", Key: "one", Value: json.RawMessage(`{}`), ExpectedRevision: -1},
		{Collection: "environment", Key: "one", Delete: true},
		{Collection: "environment", Key: "one", Delete: true, ExpectedRevision: 1, Value: json.RawMessage(`{}`)},
	} {
		assert.True(database.Commit(ctx, []Write{write}) != nil)
	}
	records, err := database.List(ctx, "environment")
	assert.NoErr(err)
	assert.Equal(len(records), 0)
	_, err = New(ctx, "")
	assert.True(err != nil)
}

// TestNewerSchemaIsRejected avoids running an older binary against an incompatible database.
func TestNewerSchemaIsRejected(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := New(ctx, path)
	assert.NoErr(err)
	_, err = database.connection.ExecContext(ctx, "PRAGMA user_version = 2")
	assert.NoErr(err)
	assert.NoErr(database.Close())
	_, err = New(ctx, path)
	assert.True(err != nil)
}

// TestOnlyOneServerOwnsDatabase prevents duplicate worker recovery from the same operation journal.
func TestOnlyOneServerOwnsDatabase(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := New(ctx, path)
	assert.NoErr(err)
	_, err = New(ctx, path)
	assert.True(err != nil)
	assert.NoErr(database.Ping(ctx))
	assert.NoErr(database.Close())
	reopened, err := New(ctx, path)
	assert.NoErr(err)
	assert.NoErr(reopened.Close())
	assert.True(reopened.Ping(ctx) != nil)
}
