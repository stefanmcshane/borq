package managementbackup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSharedRebindPreservesSpaceArchives owns durable isolation between management
// recovery and space recovery, including installations using implicit management defaults.
func TestSharedRebindPreservesSpaceArchives(t *testing.T) {
	for _, test := range []struct {
		name              string
		explicit, overlap bool
	}{
		{name: "implicit-default"}, {name: "explicit-management", explicit: true}, {name: "shared-space-bucket", overlap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			var mutex sync.Mutex
			var payload []byte
			vaultWrites := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutex.Lock()
				defer mutex.Unlock()
				if strings.HasPrefix(r.URL.Path, "/v1/") {
					w.Header().Set("Content-Type", "application/json")
					if strings.Contains(r.URL.Path, "/metadata/") {
						_, _ = w.Write([]byte(`{"data":{"current_version":1}}`))
						return
					}
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte(`{"data":{"id":"root"}}`))
						return
					}
					vaultWrites++
					_, _ = w.Write([]byte(`{"data":{"version":1}}`))
					return
				}
				if r.URL.Query().Get("list-type") != "" {
					_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
					return
				}
				switch r.Method {
				case http.MethodPut:
					payload, _ = io.ReadAll(r.Body)
				case http.MethodGet:
					_, _ = w.Write(payload)
				case http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()
			directory := t.TempDir()
			assert.NoErr(os.WriteFile(filepath.Join(directory, "RECOVERY_REQUIRED"), []byte("paused"), 0600))
			filename := filepath.Join(directory, "borq.sqlite")
			database, err := sqlite.New(t.Context(), filename)
			assert.NoErr(err)
			spaceRef := "borq/management/s3/" + strings.Repeat("a", 32)
			space, _ := json.Marshal(map[string]any{"storage": recoveryStorage{Endpoint: server.URL, Bucket: "space-bucket", CredentialsRef: spaceRef}})
			prefix := "organizations/" + strings.Repeat("b", 32) + "/postgres/" + strings.Repeat("c", 32)
			archive, _ := json.Marshal(map[string]any{"endpoint": server.URL, "bucket": "space-bucket", "prefix": prefix, "credentialsRef": spaceRef})
			sharedArchive, _ := json.Marshal(map[string]any{"endpoint": server.URL, "bucket": "management", "prefix": prefix, "credentialsRef": "borq/platform/rustfs"})
			writes := []sqlite.Write{{Collection: "space-storage", Key: "space", Value: space}, {Collection: "postgres-archive", Key: "space-archive", Value: archive}, {Collection: "postgres-archive", Key: "shared-archive", Value: sharedArchive}}
			if test.explicit {
				old, _ := json.Marshal(recoveryStorage{Endpoint: server.URL, Bucket: "management"})
				writes = append(writes, sqlite.Write{Collection: "management-s3", Key: "shared", Value: old})
			}
			assert.NoErr(database.Commit(t.Context(), writes))
			assert.NoErr(database.Close())
			bucket := "new-management"
			if test.overlap {
				bucket = "space-bucket"
			}
			err = Rebind(t.Context(), directory, objectstore.Config{Endpoint: server.URL + "/", Region: "us-east-1", Bucket: bucket, Prefix: "recovered", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "admin", SecretKey: "test-secret"}}, OpenBaoRecovery{Address: server.URL, OriginalToken: "root"})
			if test.overlap {
				assert.True(err != nil && strings.Contains(err.Error(), "cannot share a space bucket"))
			} else {
				assert.NoErr(err)
			}
			database, err = sqlite.New(t.Context(), filename)
			assert.NoErr(err)
			defer database.Close()
			retained, err := database.Get(t.Context(), "postgres-archive", "space-archive")
			assert.NoErr(err)
			assert.Equal(string(retained.Value), string(archive))
			updated, err := database.Get(t.Context(), "postgres-archive", "shared-archive")
			assert.NoErr(err)
			mutex.Lock()
			count := vaultWrites
			mutex.Unlock()
			if test.overlap {
				assert.Equal(count, 0)
				assert.Equal(string(updated.Value), string(sharedArchive))
				return
			}
			assert.Equal(count, 1)
			var actual recoveryStorage
			assert.NoErr(json.Unmarshal(updated.Value, &actual))
			assert.Equal(actual.Bucket, "new-management")
			assert.Equal(actual.Prefix, "recovered/"+prefix)
		})
	}
}
