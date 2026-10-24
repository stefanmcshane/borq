package appapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestEnvironmentGroupPublicationPresence preserves the difference between an omitted
// optimistic guard and an explicit requirement that no previous version exists.
func TestEnvironmentGroupPublicationPresence(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, source := range []string{`{"values":[]}`, `{"values":[],"baseVersion":0}`} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(source))
		var body PublishEnvironmentGroupInput
		assert.True(server.decodeBody(writer, request, "PublishEnvironmentGroupInput", &body))
		assert.True(body.Values != nil)
		assert.Equal(body.BaseVersion != nil, strings.Contains(source, "baseVersion"))
		if body.BaseVersion != nil {
			assert.Equal(*body.BaseVersion, 0)
		}
	}
}

// TestEnvironmentGroupInvalidRequests enforces JSON-only publication, valid config
// identities, and value shapes before any vault or manager connection is used.
func TestEnvironmentGroupInvalidRequests(t *testing.T) {
	assert := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	assert.NoErr(err)
	base := "/api/v1/organizations/team/spaces/dev/environment-groups"
	for _, input := range []struct {
		method, path, mediaType, body string
		status                        int
	}{
		{http.MethodGet, base + "/INVALID", "", "", 400},
		{http.MethodGet, base + "/backend/versions/0", "", "", 400},
		{http.MethodGet, base + "/backend/versions/01", "", "", 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{"values":null}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{"values":[],"baseVersion":null}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{"values":[{"name":"TOKEN","value":true}]}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{"values":[{"name":"TOKEN","value":"private-value","extra":true}]}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/json", `{"values":[],"values":[]}`, 400},
		{http.MethodPost, base + "/backend/versions", "application/yaml", "values: []", 415},
		{http.MethodPost, base + "/backend-next/clone", "application/json", `{}`, 400},
		{http.MethodPost, base + "/backend-next/clone", "application/json", `{"sourceName":"backend","sourceVersion":"0"}`, 400},
	} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(input.method, input.path, strings.NewReader(input.body))
		if input.mediaType != "" {
			request.Header.Set("Content-Type", input.mediaType)
		}
		transport.ServeHTTP(writer, request)
		assert.Equal(writer.Code, input.status)
		assert.True(!strings.Contains(writer.Body.String(), "private-value"))
	}
}
