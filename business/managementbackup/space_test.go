package managementbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSpaceRebindBucketIsolation reaches each offline bucket-ownership guard before
// any provider credentials are replaced, including endpoint aliases and implicit defaults.
func TestSpaceRebindBucketIsolation(t *testing.T) {
	for _, test := range []struct {
		name, bucket, message string
		management            bool
	}{
		{name: "other-space-alias", bucket: "other-space", management: true, message: "must have its own bucket"},
		{name: "management-alias", bucket: "management", management: true, message: "cannot share the management bucket"},
		{name: "implicit-management", bucket: "fresh-space", message: "rebind management storage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			directory := t.TempDir()
			filename := filepath.Join(directory, "borq.sqlite")
			assert.NoErr(os.WriteFile(filepath.Join(directory, "RECOVERY_REQUIRED"), []byte("paused"), 0600))
			database, err := sqlite.New(t.Context(), filename)
			assert.NoErr(err)
			identity := strings.Repeat("a", 32)
			previous := recoveryStorage{Endpoint: "https://storage.example", Region: "us-east-1", Bucket: "own-space", CredentialsRef: "borq/management/s3/" + identity}
			storage, _ := json.Marshal(map[string]any{"storage": previous})
			other, _ := json.Marshal(map[string]any{"storage": recoveryStorage{Endpoint: "https://storage.example", Bucket: "other-space"}})
			writes := []sqlite.Write{{Collection: "space-storage", Key: identity, Value: storage}, {Collection: "space-storage", Key: strings.Repeat("b", 32), Value: other}}
			if test.management {
				management, _ := json.Marshal(recoveryStorage{Endpoint: "https://storage.example", Bucket: "management"})
				writes = append(writes, sqlite.Write{Collection: "management-s3", Key: "shared", Value: management})
			}
			assert.NoErr(database.Commit(t.Context(), writes))
			assert.NoErr(database.Close())
			digest := sha256.Sum256([]byte(previous.Endpoint + "\n" + previous.Region + "\n" + previous.Bucket + "\n"))
			err = RebindSpace(t.Context(), directory, SpaceRecovery{SpaceID: identity, Source: hex.EncodeToString(digest[:16]), Storage: objectstore.Config{Endpoint: previous.Endpoint + "/", Region: "us-east-1", Bucket: test.bucket, Prefix: "recovered", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "admin", SecretKey: "secret"}}})
			assert.True(err != nil && strings.Contains(err.Error(), test.message))
			database, err = sqlite.New(t.Context(), filename)
			assert.NoErr(err)
			defer database.Close()
			record, err := database.Get(t.Context(), "space-storage", identity)
			assert.NoErr(err)
			assert.Equal(string(record.Value), string(storage))
		})
	}
}
