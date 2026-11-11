package appapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestManagementSecretsRoutes owns generated route binding, schema validation,
// cache protection and organization scope without requiring a deployment space.
func TestManagementSecretsRoutes(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: filepath.Join(t.TempDir(), "absent-kubeconfig")})
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	base := "/api/v1/organizations/team/management-secrets"
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, base, "", http.StatusOK},
		{http.MethodGet, base + "/smtp", "", http.StatusNotFound},
		{http.MethodGet, base + "/smtp/versions/latest", "", http.StatusNotFound},
		{http.MethodGet, base + "/smtp/versions/0", "", http.StatusBadRequest},
		{http.MethodGet, base + "/INVALID", "", http.StatusBadRequest},
		{http.MethodGet, strings.Replace(base, "team", "missing", 1), "", http.StatusNotFound},
		{http.MethodPut, base + "/smtp", `{"type":"registry","data":{"password":"private-api-secret"}}`, http.StatusBadRequest},
		{http.MethodPut, base + "/smtp", `{"type":"secret-group","data":{"PASSWORD":1}}`, http.StatusBadRequest},
		{http.MethodPut, base + "/smtp", `{"type":"secret-group","data":null}`, http.StatusBadRequest},
		{http.MethodPut, base + "/smtp", `{"type":"secret-group","data":{},"baseVersion":-1}`, http.StatusBadRequest},
		{http.MethodPut, strings.Replace(base, "team", "missing", 1) + "/smtp", `{"type":"secret-group","data":{"PASSWORD":"private-api-secret"}}`, http.StatusNotFound},
		{http.MethodPut, base + "/smtp", `{"type":"secret-group","data":{"PASSWORD":"private-api-secret"}}`, http.StatusInternalServerError},
	} {
		t.Run(test.method+" "+test.path+" "+strconv.Itoa(test.status), func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			transport.ServeHTTP(response, request)
			assert.Equal(response.Code, test.status)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			assert.True(!strings.Contains(response.Body.String(), "private-api-secret"))
		})
	}
}
