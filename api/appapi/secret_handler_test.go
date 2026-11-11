package appapi

import (
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretRequestsRequireExistingSpace checks CRUD route binding, JSON shape
// validation, and scope guards before allowing access to any secret backend.
func TestSecretRequestsRequireExistingSpace(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	base := "/api/v1/organizations/team/spaces/missing/secrets"
	for _, input := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", base, "", 404}, {"GET", base + "/shared", "", 404}, {"GET", base + "/shared/versions/latest", "", 404},
		{"DELETE", base + "/ghcr.io?version=2", "", 404},
		{"PUT", base + "/ghcr.io", `{"type":"registry","data":{"username":"user","token":"private-credential"}}`, 404},
		{"PUT", base + "/shared", `{"type":"secret-group","data":{"TOKEN":"private-credential"}}`, 404},
		{"PUT", base + "/shared", `{"type":"future","data":{"TOKEN":"private-credential"}}`, 400},
		{"PUT", base + "/shared", `{"type":"secret-group","data":{"TOKEN":1}}`, 400},
		{"PUT", base + "/shared", `{"type":"registry","data":null}`, 400},
		{"DELETE", base + "/ghcr.io?version=0", "", 400},
		{"GET", base + "/INVALID", "", 400},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(input.method, input.path, strings.NewReader(input.body))
		if input.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		transport.ServeHTTP(response, request)
		assert.Equal(response.Code, input.status)
		assert.True(!strings.Contains(response.Body.String(), "private-credential"))
	}
}
