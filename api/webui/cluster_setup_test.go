package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestClusterSetupDisclosure presents the server's component purpose, version
// and destination without exposing internal errors when catalog loading fails.
func TestClusterSetupDisclosure(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "catalog", true: "unavailable"}[unavailable], func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/cluster-setup")
				if unavailable {
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = writer.Write([]byte(`{"message":"private-catalog-error"}`))
					return
				}
				assert.NoErr(json.NewEncoder(writer).Encode(appapi.ClusterSetup{Namespace: "custom-target", Components: []appapi.ClusterSetupComponent{{Name: "cert-manager", Version: "test-version", Description: "Issues HTTPS certificates."}}}))
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/cluster-setup", nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			assert.True(!strings.Contains(response.Body.String(), "private-catalog-error"))
			if unavailable {
				assert.True(strings.Contains(response.Body.String(), "Required component details could not be loaded."))
				assert.True(strings.Contains(response.Body.String(), "Try again"))
				return
			}
			for _, text := range []string{"custom-target", "cert-manager", "test-version", "Issues HTTPS certificates.", "cluster-wide permissions and resource definitions"} {
				assert.True(strings.Contains(response.Body.String(), text))
			}
		})
	}
}
