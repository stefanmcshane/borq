package apps

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSpaceStorageWaitsForRotation prevents background provisioning from freezing
// a new bucket to the old parent while a durable credential rotation is pending.
func TestSpaceStorageWaitsForRotation(t *testing.T) {
	assert := is.New(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	rotation, err := documentWrite("management-s3-rotation", "shared", storageRotation{Status: "staged"}, 0)
	assert.NoErr(err)
	space := Space{ID: "0123456789abcdef0123456789abcdef"}
	storage, err := newSpaceStorageWrite(space)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{rotation, storage}))
	stored, _, err := loadDocument[spaceStorageRecord](t.Context(), manager.database, "space-storage", space.ID)
	assert.NoErr(err)
	_, _, err = manager.provisionSpaceStorage(t.Context(), stored)
	assert.True(errors.Is(err, ErrConflict))
}
