package objectstore

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
)

// TestEnsureBucket distinguishes absence from forbidden access and reuses existing buckets.
func TestEnsureBucket(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := is.New(t)
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(status)
					return
				}
				if r.Method == http.MethodPut {
					creates++
					return
				}
				w.WriteHeader(http.StatusMethodNotAllowed)
			}))
			defer server.Close()
			client, err := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "apps", PathStyle: true, Credentials: Credentials{AccessKey: "fixture", SecretKey: "fixture-secret"}})
			assert.NoErr(err)
			err = client.EnsureBucket(t.Context())
			if status == http.StatusForbidden {
				assert.True(err != nil)
			} else {
				assert.NoErr(err)
			}
			if status == http.StatusNotFound {
				assert.Equal(creates, 1)
			} else {
				assert.Equal(creates, 0)
			}
		})
	}
}
