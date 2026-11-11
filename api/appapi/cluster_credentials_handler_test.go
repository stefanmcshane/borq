package appapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestClusterKubeconfigErrorsAreNotCached covers validation middleware, missing
// organizations, wrong organizations and unsupported methods on credential paths.
func TestClusterKubeconfigErrorsAreNotCached(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	for _, input := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/organizations/Invalid/clusters/dev/kubeconfig", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/organizations/team/clusters/Invalid/kubeconfig", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/organizations/missing/clusters/dev/kubeconfig", http.StatusNotFound},
		{http.MethodPost, "/api/v1/organizations/team/clusters/dev/kubeconfig", http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		transport.ServeHTTP(response, httptest.NewRequest(input.method, input.path, nil))
		assert.Equal(response.Code, input.status)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	}
}
