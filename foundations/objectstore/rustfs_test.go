package objectstore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestRustfsAccountRetry covers RustFS's distinct creation and update responses and safe errors.
func TestRustfsAccountRetry(t *testing.T) {
	assert := is.New(t)
	exists, denied := false, false
	created, updated := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=management/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if denied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("private-management-secret"))
			return
		}
		switch r.Method {
		case http.MethodGet:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodPut:
			created++
			exists = true
		case http.MethodPost:
			updated++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	config := Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "applications", Prefix: "sqlite/space/app", PathStyle: true, Credentials: Credentials{AccessKey: "management", SecretKey: "private-management-secret"}}
	first, err := RustfsCredentials(t.Context(), config)
	assert.NoErr(err)
	second, err := RustfsCredentials(t.Context(), config)
	assert.NoErr(err)
	assert.Equal(first, second)
	assert.True(first.AccessKey != config.Credentials.AccessKey && first.SecretKey != config.Credentials.SecretKey)
	assert.Equal(created, 1)
	assert.Equal(updated, 1)
	denied = true
	_, err = RustfsCredentials(t.Context(), config)
	assert.True(err != nil && !strings.Contains(err.Error(), config.Credentials.SecretKey))
}
