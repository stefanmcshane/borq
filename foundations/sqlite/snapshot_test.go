package sqlite

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
)

// TestNativeSnapshotConcurrentWrites proves committed document pairs remain consistent across native snapshot, close and reopen.
func TestNativeSnapshotConcurrentWrites(t *testing.T) {
	assert := is.New(t)
	database, err := New(t.Context(), filepath.Join(t.TempDir(), "live.sqlite"))
	assert.NoErr(err)
	for _, key := range []string{"left", "right"} {
		assert.NoErr(database.Commit(t.Context(), []Write{{Collection: "pair", Key: key, Value: json.RawMessage(`0`)}}))
	}
	stopped := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		revision := int64(1)
		for {
			select {
			case <-stopped:
				return
			default:
			}
			value := json.RawMessage(strconv.FormatInt(revision, 10))
			err := database.Commit(t.Context(), []Write{{Collection: "pair", Key: "left", Value: value, ExpectedRevision: revision}, {Collection: "pair", Key: "right", Value: value, ExpectedRevision: revision}})
			if err != nil {
				return
			}
			revision++
		}
	}()
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	assert.NoErr(database.Snapshot(t.Context(), snapshot))
	close(stopped)
	writer.Wait()
	assert.NoErr(database.Close())
	assert.NoErr(ValidateSnapshot(t.Context(), snapshot))
	restored, err := New(t.Context(), snapshot)
	assert.NoErr(err)
	defer restored.Close()
	left, err := restored.Get(t.Context(), "pair", "left")
	assert.NoErr(err)
	right, err := restored.Get(t.Context(), "pair", "right")
	assert.NoErr(err)
	assert.Equal(left.Value, right.Value)
	assert.Equal(left.Revision, right.Revision)
}

// TestBoundedRecoveryRead rejects excessive native metadata and preserves deterministic records across real close/reopen.
func TestBoundedRecoveryRead(t *testing.T) {
	assert := is.New(t)
	file := filepath.Join(t.TempDir(), "bounded.sqlite")
	database, err := New(t.Context(), file)
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []Write{{Collection: "cluster", Key: "a", Value: []byte(`{"value":"first"}`)}, {Collection: "cluster", Key: "b", Value: []byte(`{"value":"second"}`)}}))
	assert.NoErr(database.Close())
	database, err = New(t.Context(), file)
	assert.NoErr(err)
	defer database.Close()
	records, err := database.ListBounded(t.Context(), "cluster", 1, 1024)
	assert.True(err != nil && strings.Contains(err.Error(), "count or byte limits"))
	assert.Equal(len(records), 0)
	records, err = database.ListBounded(t.Context(), "cluster", 10, 8)
	assert.True(err != nil && strings.Contains(err.Error(), "count or byte limits"))
	assert.Equal(len(records), 0)
	records, err = database.ListBounded(t.Context(), "cluster", 10, 1024)
	assert.NoErr(err)
	assert.Equal(len(records), 2)
	assert.Equal(records[0].Key, "a")
	assert.Equal(string(records[1].Value), `{"value":"second"}`)
}
