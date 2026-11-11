package appapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementDestroyAdmissionRoutes proves a restored maintenance fence
// blocks ordinary API and projected-job dispatch while retaining the three
// administrator teardown routes needed for status and recovery.
func TestManagementDestroyAdmissionRoutes(t *testing.T) {
	assert := is.New(t)
	file := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(t.Context(), file)
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{
		{Collection: "management-destroy", Key: "active", Value: json.RawMessage(`{"operationId":"failed-teardown"}`)},
		{Collection: "management-destroy-task", Key: "failed-teardown", Value: json.RawMessage(`{"plan":{},"executionPlanned":false}`)},
		{Collection: "management-operation", Key: "failed-teardown", Value: json.RawMessage(`{"id":"failed-teardown","status":"failed"}`)},
	}))
	assert.NoErr(database.Close())
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: file})
	assert.NoErr(err)
	defer manager.Close()
	var dispatched int
	handler, err := NewAuthorizationHandler(manager, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dispatched++
		writer.WriteHeader(http.StatusNoContent)
	}))
	assert.NoErr(err)
	for _, test := range []struct {
		method, path string
		admitted     bool
		anonymous    bool
	}{
		{http.MethodGet, "/api/v1/management/destroy", true, false},
		{http.MethodPost, "/api/v1/management/destroy", true, false},
		{http.MethodGet, "/api/v1/management/destroy/operations/failed-teardown", true, false},
		{http.MethodGet, "/api/v1/organizations", false, false},
		{http.MethodPut, "/api/v1/organizations/new-team", false, false},
		{http.MethodPut, "/api/v1/management/configuration", false, false},
		{http.MethodPost, "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/restore-check", false, true},
		{http.MethodGet, "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/restore-check/job-operation", false, true},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		if !test.anonymous {
			request = request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true}))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if test.admitted {
			assert.Equal(response.Code, http.StatusNoContent)
		} else {
			assert.Equal(response.Code, http.StatusConflict)
		}
	}
	assert.Equal(dispatched, 3)
}
