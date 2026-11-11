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

// TestRequestInputWhitespace owns normalization before schema validation while
// preserving commands, credentials, literal values and arbitrary chart payloads.
func TestRequestInputWhitespace(t *testing.T) {
	check := is.New(t)
	specification, err := GetSwagger()
	check.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, input := range []struct{ schema, body, want string }{
		{"Patch", `{"apps":[{"name":" \tweb\n","image":" nginx:1.27.5 ","port":8080,"replicas":0,"command":"  echo hello  ","env":{"name":" config ","version":" latest "},"checks":{"readiness":{"path":" /ready "}},"ingresses":[{"name":" public ","host":" app.example.com ","path":" /api "}],"predeploy":{"command":"  migrate\n  "}}]}`, `{"apps":[{"name":"web","image":"nginx:1.27.5","port":8080,"replicas":0,"command":"  echo hello  ","env":{"name":"config","version":"latest"},"checks":{"readiness":{"path":"/ready"}},"ingresses":[{"name":"public","host":"app.example.com","path":"/api"}],"predeploy":{"command":"  migrate\n  "}}]}`},
		{"PublishEnvironmentGroupInput", `{"baseVersion":0,"values":[{"name":" TOKEN ","value":"  literal\n  "}]}`, `{"baseVersion":0,"values":[{"name":"TOKEN","value":"  literal\n  "}]}`},
		{"CustomChartInput", `{"repository":" https://charts.example.com ","chart":" cache ","version":" 1.2.3 ","values":{"name":" keep spaces ","image":" keep this too "},"podSelector":{"name":" keep selector "}}`, `{"repository":"https://charts.example.com","chart":"cache","version":"1.2.3","values":{"name":" keep spaces ","image":" keep this too "},"podSelector":{"name":" keep selector "}}`},
		{"ManagementConfiguration", `{"registries":{"ghcr.io":{"username":" user ","token":" token with spaces "}},"components":{"alloy":{"repo":" https://charts.example.com ","version":" 1.2.3 ","values":{"image":" untouched "}}},"organizations":[{"name":" team ","spaces":[{"name":" dev ","cluster":" target ","apps":[{"name":" api ","image":" nginx:1.27.5 "}]}]}]}`, `{"registries":{"ghcr.io":{"username":"user","token":" token with spaces "}},"components":{"alloy":{"repo":"https://charts.example.com","version":"1.2.3","values":{"image":" untouched "}}},"organizations":[{"name":"team","spaces":[{"name":"dev","cluster":"target","apps":[{"name":"api","image":"nginx:1.27.5"}]}]}]}`},
		{"ManagementOffsiteInput", `{"enabled":true,"repository":" s3:https://s3.example.com/archive ","region":" us-east-1 ","password":" passphrase ","accessKey":" access ","secretKey":" secret ","sessionToken":" session "}`, `{"enabled":true,"repository":"s3:https://s3.example.com/archive","region":"us-east-1","password":" passphrase ","accessKey":" access ","secretKey":" secret ","sessionToken":" session "}`},
		{"PostgresInput", `{"storage":" 2Gi ","storageClass":" fast ","image":" ghcr.io/cloudnative-pg/postgresql:18.0 ","database":" app ","username":" user ","resources":{"requests":{"cpu":" 100m ","memory":" 128Mi "}},"credentials":{"host":" database.local ","port":5432,"rotationPeriod":" 24h "}}`, `{"storage":"2Gi","storageClass":"fast","image":"ghcr.io/cloudnative-pg/postgresql:18.0","database":"app","username":"user","resources":{"requests":{"cpu":"100m","memory":"128Mi"}},"credentials":{"host":"database.local","port":5432,"rotationPeriod":"24h"}}`},
	} {
		t.Run(input.schema, func(t *testing.T) {
			check := is.New(t)
			response := httptest.NewRecorder()
			var actual, want map[string]any
			check.True(server.decodeBody(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(input.body)), input.schema, &actual))
			check.NoErr(json.Unmarshal([]byte(input.want), &want))
			check.Equal(actual, want)
		})
	}
	for _, body := range []string{
		`{"apps":[{"name":"   ","image":"nginx:1.27.5"}]}`,
		`{"apps":[{"name":"my app","image":"nginx:1.27.5"}]}`,
		`{"apps":[{"name":"web","port":" 8080 "}]}`,
		`{"apps":[{"name":"web","image":null}]}`,
		`{"apps":[{"name":"web","image":"nginx:1.27.5","unknown":"value"}]}`,
		`{"apps":[{"name":"web","name":"api"}]}`,
	} {
		response := httptest.NewRecorder()
		var output Patch
		check.True(!server.decodeBody(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), "Patch", &output))
		check.Equal(response.Code, http.StatusBadRequest)
	}
}

// TestTrimmedRequestPaths checks canonical name routing and rejects attempts to
// turn padded traversal or escaped separators into a different resource route.
func TestTrimmedRequestPaths(t *testing.T) {
	check := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "names.sqlite")})
	check.NoErr(err)
	t.Cleanup(func() { check.NoErr(manager.Close()) })
	_, err = manager.CreateOrganization(t.Context(), "team")
	check.NoErr(err)
	api, err := NewHandler(manager)
	check.NoErr(err)
	for _, path := range []string{"/api/v1/organizations/%20team%20", "/api/v1/organizations/team"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPut, path, nil))
		check.Equal(response.Code, http.StatusOK)
		var organization Organization
		check.NoErr(json.Unmarshal(response.Body.Bytes(), &organization))
		check.Equal(organization.Name, "team")
	}
	for _, path := range []string{"/api/v1/organizations/%20..%20", "/api/v1/organizations/%20.%20", "/api/v1/organizations/%20%20", "/api/v1/organizations/%20bad%20name%20", "/api/v1/organizations/%20team%20%2fspaces%2fdev"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPut, path, nil))
		check.Equal(response.Code, http.StatusBadRequest)
		check.Equal(response.Header().Get("Location"), "")
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/apps/expand", strings.NewReader(`{"apps":[{"name":"web","image":"nginx:1.27.5"},{"name":" web ","image":"nginx:1.27.5"}]}`)))
	check.Equal(response.Code, http.StatusBadRequest)
}
