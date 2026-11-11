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

// TestPocketIdConfigurationRoute owns the generated privileged read contract,
// organization path validation and cache protection without a live cluster.
func TestPocketIdConfigurationRoute(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	for _, test := range []struct {
		organization string
		status       int
	}{
		{"team", http.StatusOK}, {"missing", http.StatusNotFound}, {"INVALID", http.StatusBadRequest},
	} {
		t.Run(test.organization, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			transport.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/"+test.organization+"/pocketid/configuration", nil))
			assert.Equal(response.Code, test.status)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			if test.status == http.StatusOK {
				var result OrganizationPocketIDConfiguration
				assert.NoErr(json.Unmarshal(response.Body.Bytes(), &result))
				assert.Equal(result.Values["APP_NAME"], "team")
				assert.True(result.Revision != "")
				assert.Equal(result.DefaultCertificateIssuer, "")
				assert.Equal(result.CertificateIssuers, []string{})
			}
		})
	}
}
