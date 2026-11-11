package appapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestManagementBackupCoverageRoute owns generated response conversion and the
// installation-wide read grant, without requiring backup storage to be present.
func TestManagementBackupCoverageRoute(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	api, err := NewHandler(manager)
	assert.NoErr(err)
	secured, err := NewAuthorizationHandler(manager, api)
	assert.NoErr(err)
	for _, principal := range []apps.Principal{{}, {Admin: true}} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/management/backups/status", nil)
		request = request.WithContext(apps.WithPrincipal(request.Context(), principal))
		response := httptest.NewRecorder()
		secured.ServeHTTP(response, request)
		if !principal.Admin {
			assert.Equal(response.Code, http.StatusForbidden)
			continue
		}
		assert.Equal(response.Code, http.StatusOK)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
		var coverage ManagementBackupCoverage
		assert.NoErr(json.Unmarshal(response.Body.Bytes(), &coverage))
		assert.Equal(string(coverage.State), "attention")
		assert.Equal(len(coverage.Components), 3)
		for _, component := range coverage.Components {
			assert.Equal(string(component.State), "disabled")
			assert.True(component.LastSuccess == nil)
		}
	}
}
