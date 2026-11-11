package appapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestComponentCatalogTransport protects the typed client, public schema, read cache policy, and query validation.
func TestComponentCatalogTransport(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "catalog.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	api, err := NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(api)
	defer server.Close()
	client, err := NewClientWithResponses(server.URL)
	assert.NoErr(err)
	response, err := client.GetComponentCatalogWithResponse(context.Background(), nil)
	assert.NoErr(err)
	assert.Equal(response.StatusCode(), http.StatusOK)
	assert.Equal(response.HTTPResponse.Header.Get("Cache-Control"), "no-store")
	assert.True(response.JSON200 != nil)
	assert.Equal(string(response.JSON200.Scope), "management")
	var value any
	assert.NoErr(json.Unmarshal(response.Body, &value))
	specification, err := GetSwagger()
	assert.NoErr(err)
	assert.NoErr(specification.Components.Schemas["ComponentCatalog"].Value.VisitJSON(value))
	for _, query := range []string{"scope=invalid", "scope=target", "scope=target&organization=team"} {
		writer := httptest.NewRecorder()
		api.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/v1/management/component-catalog?"+query, nil))
		assert.Equal(writer.Code, http.StatusBadRequest)
		assert.Equal(writer.Header().Get("Cache-Control"), "no-store")
	}
}
