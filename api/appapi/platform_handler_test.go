package appapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestClusterSetupCatalog exposes configured installation metadata before an
// organization or cluster exists, without requiring Kubernetes credentials.
func TestClusterSetupCatalog(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "catalog.sqlite"), PlatformSettings: platform.Settings{Namespace: "custom-target"}})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := NewHandler(manager)
	assert.NoErr(err)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/cluster-setup", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	var setup ClusterSetup
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &setup))
	assert.Equal(setup.Namespace, "borq-system")
	assert.True(len(setup.Components) > 0)
	for _, component := range setup.Components {
		assert.True(component.Name != "")
		assert.True(component.Version != "")
		assert.True(component.Description != "")
	}
}

// TestClusterReconcileRequestPresence preserves explicit registration-only
// requests without changing the default for clients that omit the option.
func TestClusterReconcileRequestPresence(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, source := range []string{`{}`, `{"reconcile":true}`, `{"reconcile":false}`} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(source))
		var body ClusterInput
		assert.True(server.decodeBody(writer, request, "ClusterInput", &body))
		input, err := convert[apps.ClusterInput](body)
		assert.NoErr(err)
		assert.Equal(input.Reconcile != nil, strings.Contains(source, "reconcile"))
		if input.Reconcile != nil {
			assert.Equal(*input.Reconcile, strings.Contains(source, "true"))
		}
	}
	writer := httptest.NewRecorder()
	var body ClusterInput
	assert.True(!server.decodeBody(writer, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"reconcile":null}`)), "ClusterInput", &body))
}

// TestComponentDiffRouteRejectsDisabledInfrastructure distinguishes a disabled
// manager from malformed query parameters rejected before reaching that manager.
func TestComponentDiffRouteRejectsDisabledInfrastructure(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	for _, input := range []struct{ query, message string }{
		{"", "request configuration is invalid"},
		{"?upgrade=true", "request configuration is invalid"},
		{"?upgrade=invalid", "invalid request path"},
	} {
		writer := httptest.NewRecorder()
		transport.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/team/clusters/target/components/diff"+input.query, nil))
		assert.Equal(writer.Code, http.StatusBadRequest)
		var problem Problem
		assert.NoErr(json.Unmarshal(writer.Body.Bytes(), &problem))
		assert.Equal(problem.Message, input.message)
	}
}
