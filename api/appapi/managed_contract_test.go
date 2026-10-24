package appapi

import (
	"encoding/json"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestManagementDocumentSchema protects the public named hierarchy and shared space fragment at the HTTP decoding boundary.
func TestManagementDocumentSchema(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, input := range []struct {
		schema, body string
		valid        bool
	}{
		{"ManagementConfiguration", `{"components":{"cloudnative-pg":{"repo":"https://cloudnative-pg.github.io/charts","chart":"cloudnative-pg","version":"0.29.1","values":{"replicaCount":1}}},"s3":{"region":"us-east-1","bucket":"shared","pathStyle":true,"accessKey":"private-key","secretKey":"private-secret"},"organizations":[{"name":"team","managedAddons":true,"clusters":[{"name":"local","kubeconfig":"private-kubeconfig"}],"spaces":[{"name":"dev","cluster":"local","apps":[{"name":"api","replicas":0}],"customAddons":[{"name":"cache","repository":"https://charts.example.com","chart":"redis","version":"1.2.3"}],"managedAddons":{"postgres":[{"name":"database","restoreCheck":{"enabled":false}}]}}]}]}`, true},
		{"SpaceConfiguration", `{"apps":[],"customAddons":[],"managedAddons":{"postgres":[{"name":"database","storage":"2Gi"}]}}`, true},
		{"ManagementConfiguration", `{"version":1}`, false},
		{"ManagementConfiguration", `{"profile":"development"}`, false},
		{"SpaceConfiguration", `{"managedAddons":{"postgres":[{"name":"database","unrecognized":true}]}}`, false},
		{"ManagementConfiguration", `{"s3":{"region":"one","region":"two","bucket":"shared","pathStyle":true}}`, false},
	} {
		writer := httptest.NewRecorder()
		var output any
		accepted := server.decodeBody(writer, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(input.body)), input.schema, &output)
		if accepted != input.valid {
			t.Fatalf("%s accepted=%v want=%v: %s", input.schema, accepted, input.valid, writer.Body.String())
		}
		if !accepted {
			assert.True(!strings.Contains(writer.Body.String(), "private-secret"))
		}
	}
}

// TestManagedRoutesRejectInvalidRequestsBeforeManager protects path binding, strict bodies, bounded logs and secret cache headers.
func TestManagedRoutesRejectInvalidRequestsBeforeManager(t *testing.T) {
	assert := is.New(t)
	api, err := NewHandler(apps.Manager{})
	assert.NoErr(err)
	base := "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/"
	for _, input := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPut, base + "database", `{"password":"private-password"}`, 400},
		{http.MethodPut, base + "database?apply=invalid", `{}`, 400},
		{http.MethodPut, base + "INVALID", `{}`, 400},
		{http.MethodGet, base + "database/logs?limit=0", "", 400},
		{http.MethodGet, base + "database/logs?limit=1001", "", 400},
		{http.MethodPost, base + "database/restore-check", `{"jobUid":"job"}`, 401},
		{http.MethodGet, base + "database/restore-check/operation?jobUid=job", "", 401},
	} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(input.method, input.path, strings.NewReader(input.body)))
		assert.Equal(response.Code, input.status)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
		assert.True(!strings.Contains(response.Body.String(), "private-password"))
	}
}

// TestManagementStorageResponseUsesSafeModel prevents inline credentials from surviving response conversion.
func TestManagementStorageResponseUsesSafeModel(t *testing.T) {
	assert := is.New(t)
	writer := httptest.NewRecorder()
	respond[ManagementS3](writer, http.StatusOK, map[string]any{"region": "us-east-1", "bucket": "shared", "pathStyle": true, "credentialsRef": "vault:shared/1", "accessKey": "private-access", "secretKey": "private-secret", "sessionToken": "private-session"}, nil)
	assert.Equal(writer.Code, http.StatusOK)
	var output map[string]any
	assert.NoErr(json.Unmarshal(writer.Body.Bytes(), &output))
	assert.Equal(output["credentialsRef"], "vault:shared/1")
	for _, key := range []string{"accessKey", "secretKey", "sessionToken"} {
		_, exists := output[key]
		assert.True(!exists)
	}
}
