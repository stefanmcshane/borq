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
)

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
