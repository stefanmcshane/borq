package appserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementDestroyIdentityRoutes verifies that custom authentication
// handlers honor a restored fence while administrator recovery stays available.
func TestManagementDestroyIdentityRoutes(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	database, err := sqlite.New(t.Context(), filepath.Join(directory, "borq.sqlite"))
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{
		{Collection: "management-destroy", Key: "active", Value: json.RawMessage(`{"operationId":"failed-before-cleanup"}`)},
		{Collection: "management-destroy-task", Key: "failed-before-cleanup", Value: json.RawMessage(`{"plan":{},"executionPlanned":false}`)},
		{Collection: "management-operation", Key: "failed-before-cleanup", Value: json.RawMessage(`{"id":"failed-before-cleanup","status":"failed"}`)},
	}))
	assert.NoErr(database.Close())
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	defer server.Close()
	credential, err := os.ReadFile(filepath.Join(directory, "admin-token"))
	assert.NoErr(err)
	token := strings.TrimSpace(string(credential))
	base := "https://borq.test"
	login := authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/login", url.Values{"token": {token}}.Encode(), "")
	assert.Equal(login.Code, http.StatusSeeOther)
	cookies := login.Result().Cookies()
	assert.Equal(len(cookies), 1)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/oidc/login"},
		{http.MethodGet, "/api/auth/oidc/callback/fixture"},
		{http.MethodPost, "/api/auth/pocketid/admin/fixture"},
		{http.MethodPost, "/api/auth/tokens"},
		{http.MethodPost, "/account/tokens"},
	} {
		response := authenticationCall(server.http.Handler, route.method, base+route.path, "", token, cookies...)
		assert.Equal(response.Code, http.StatusConflict)
	}
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/auth/session", "", token).Code, http.StatusOK)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodGet, base+"/api/readyz", "", "").Code, http.StatusOK)
	assert.Equal(authenticationCall(server.http.Handler, http.MethodPost, base+"/api/auth/logout", "", "", cookies...).Code, http.StatusSeeOther)
}
