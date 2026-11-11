package appmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolDiscovery verifies that clients receive independently resolvable JSON
// Schemas for every tool's inputs and status-specific response bodies.
func TestToolDiscovery(t *testing.T) {
	assert := is.New(t)
	ctx, session := connectTools(t, http.NotFoundHandler())
	listed, err := session.ListTools(ctx, nil)
	assert.NoErr(err)
	assert.True(len(listed.Tools) > 0)
	schemas := map[string]*jsonschema.Resolved{}
	outputs := map[string]*jsonschema.Resolved{}
	tools := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			schemas[tool.Name] = resolveToolSchema(t, tool.InputSchema)
			outputs[tool.Name] = resolveToolSchema(t, tool.OutputSchema)
		})
		tools[tool.Name] = tool
		assert.True(tool.Description != "")
	}
	if t.Failed() {
		return
	}
	for _, input := range []struct {
		name      string
		arguments string
		valid     bool
	}{
		{"PutSecret", `{"path":{"organization":"team","space":"dev","secret":"github-packages"},"body":{"type":"registry","data":{"server":"ghcr.io","username":"user","password":"private"}}}`, true},
		{"ApplyManagementConfiguration", `{"body":{"registries":{"hub":{"server":"docker.io","username":"user","token":"private"}}}}`, true},
		{"PutSecret", `{"path":{"organization":"team","space":"dev","secret":"ghcr.io"},"body":{"type":"registry","data":{"username":"user","token":"private"},"baseVersion":0}}`, true},
		{"PutSecret", `{"path":{"organization":"team","space":"dev","secret":"shared"},"body":{"type":"secret-group","data":{"TOKEN":"value","EMPTY":""}}}`, true},
		{"PutSecret", `{"path":{"organization":"team","space":"dev","secret":"shared"},"body":{"type":"future","data":{}}}`, false},
		{"PutSecret", `{"path":{"organization":"team","space":"dev","secret":"shared"},"body":{"type":"secret-group","data":{"TOKEN":1}}}`, false},
		{"DeleteSecret", `{"path":{"organization":"team","space":"dev","secret":"ghcr.io"},"query":{"version":2}}`, true},
		{"DeleteSecret", `{"path":{"organization":"team","space":"dev","secret":"ghcr.io"},"query":{"version":0}}`, false},
		{"PutPocketID", `{"path":{"organization":"team","space":"dev","pocketid":"identity"},"body":{"issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}}`, true},
		{"PutPocketID", `{"path":{"organization":"team","space":"dev","pocketid":"identity"},"body":{"password":"private"}}`, false},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"database"},"body":{"backup":{"enabled":false,"restoreCheck":{"schedule":"0 3 * * 0"}}}}`, true},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"database"},"body":{"backup":{"restoreCheck":{"enabled":false}}}}`, false},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"database"},"body":{"snapshots":"disabled"}}`, false},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"database"},"body":{"password":"secret"}}`, false},
		{"ApplyManagementConfiguration", `{"body":{"components":{"cloudnative-pg":{"version":"0.29.1"}},"organizations":[{"name":"team","managedAddons":true,"spaces":[{"name":"dev","managedAddons":{"postgres":[{"name":"database"}]}}]}]}}`, true},
		{"PutManagementS3", `{"body":{"endpoint":"https://storage.example.com","region":"us-east-1","bucket":"management-data","pathStyle":true,"accessKey":"private-access","secretKey":"private-secret"}}`, true},
		{"GetManagementOperation", `{"path":{"operation":"storage-test-1"}}`, true},
		{"RestorePostgres", `{"path":{"organization":"team","space":"dev","postgres":"restored"},"body":{"backupId":"approved"}}`, true},
		{"PutCustomChart", `{"path":{"organization":"team","space":"dev","customChart":"cache"},"body":{"repository":"oci://registry.example.com/charts","chart":"redis","version":"1.2.3","values":{"replicas":0},"logs":false}}`, true},
		{"PutCustomChart", `{"path":{"organization":"team","space":"dev","customChart":"cache"},"body":{"repository":"https://charts.example.com","chart":"redis"}}`, false},
		{"GetChartDefaults", `{"body":{"repository":"https://charts.example.com","name":"redis","version":"1.2.3"}}`, true},
		{"GetAppMetrics", `{"path":{"organization":"team","space":"dev","application":"web"},"query":{"range":"3d"}}`, true},
		{"GetAppMetrics", `{"path":{"organization":"team","space":"dev","application":"web"},"query":{"range":"90d"}}`, false},
		{"GetAppLogs", `{"path":{"organization":"team","space":"dev","application":"web"},"query":{"search":"error","limit":1001}}`, false},
		{"PlanAppRollback", `{"path":{"organization":"team","space":"dev","application":"web"},"body":{"deployment":"release-1"}}`, true},
		{"ListOrganizations", `{}`, true},
		{"ListOrganizations", `{"extra":true}`, false},
		{"PutOrganization", `{"path":{"organization":"team"}}`, true},
		{"PutOrganization", `{}`, false},
		{"PutOrganization", `{"path":{"organization":"../escape"}}`, false},
		{"PutSpace", `{"path":{"organization":"team","space":"dev"},"body":{"cluster":"local"}}`, true},
		{"PutSpace", `{"path":{"organization":"team","space":"dev"},"body":{}}`, false},
		{"PutSpace", `{"path":{"organization":"team","space":"dev"},"body":{"cluster":""}}`, false},
		{"PutSpace", `{"path":{"organization":"team","space":"dev"},"body":{"cluster":"local","namespace":"custom"}}`, false},
		{"PutSpace", `{"path":{"organization":"team","space":"dev"},"body":{"cluster":"local","certificateIssuer":"custom"}}`, false},
		{"ExpandApps", `{"body":{"apps":[{"name":"api","image":"nginx","replicas":0,"predeploy":{}}]}}`, true},
		{"ExpandApps", `{"body":{"apps":[{"name":"api","replicas":-1}]}}`, false},
		{"ExpandApps", `{"body":null}`, false},
		{"DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":true}}`, true},
		{"DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":"true"}}`, false},
	} {
		t.Run(input.name+input.arguments, func(t *testing.T) {
			assert := is.New(t)
			assert.True(schemas[input.name] != nil)
			var arguments any
			assert.NoErr(json.Unmarshal([]byte(input.arguments), &arguments))
			assert.Equal(schemas[input.name].Validate(arguments) == nil, input.valid)
		})
	}
	for _, output := range []struct {
		name, tool, response string
		valid                bool
	}{
		{"empty collection", "ListOrganizations", `{"status":200,"body":[]}`, true},
		{"organization collection", "ListOrganizations", `{"status":200,"body":[{"id":"org-1","name":"team","managedAddons":false}]}`, true},
		{"missing organization identity", "ListOrganizations", `{"status":200,"body":[{"name":"team","managedAddons":false}]}`, false},
		{"wrong collection type", "ListOrganizations", `{"status":200,"body":{}}`, false},
		{"missing status", "ListOrganizations", `{"body":[]}`, false},
		{"missing body", "ListOrganizations", `{"status":200}`, false},
		{"string status", "ListOrganizations", `{"status":"200","body":[]}`, false},
		{"wrong success status", "ListOrganizations", `{"status":201,"body":[]}`, false},
		{"api problem", "ListOrganizations", `{"status":400,"body":{"message":"invalid request"}}`, true},
		{"problem with success status", "ListOrganizations", `{"status":200,"body":{"message":"invalid request"}}`, false},
		{"invalid problem", "ListOrganizations", `{"status":400,"body":{"message":42}}`, false},
		{"plain api error", "GetPlan", `{"status":503,"body":"unavailable"}`, true},
		{"management readiness error", "GetPlan", `{"status":503,"body":{"message":"Management components are not ready"}}`, true},
		{"plain success", "ListOrganizations", `{"status":200,"body":"unexpected"}`, false},
		{"queued operation", "ApplyPlan", `{"status":202,"body":{"id":"operation-1","planId":"plan-1","spaceId":"space-1","status":"queued","phase":"secrets","message":"","resources":[],"startedAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z"}}`, true},
		{"invalid operation state", "ApplyPlan", `{"status":202,"body":{"id":"operation-1","planId":"plan-1","spaceId":"space-1","status":"unknown","phase":"secrets","message":"","resources":[],"startedAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z"}}`, false},
		{"nested secret values", "GetSecret", `{"status":200,"body":{"name":"shared","type":"secret-group","data":{"TOKEN":"value"},"version":1}}`, true},
		{"invalid nested secret value", "GetSecret", `{"status":200,"body":{"name":"shared","type":"secret-group","data":{"TOKEN":42},"version":1}}`, false},
	} {
		t.Run(output.name, func(t *testing.T) {
			assert := is.New(t)
			var response any
			assert.NoErr(json.Unmarshal([]byte(output.response), &response))
			assert.Equal(outputs[output.tool].Validate(response) == nil, output.valid)
		})
	}
	assert.True(tools["RestoreCheckPostgres"] == nil)
	assert.True(tools["ListSecrets"].Annotations.ReadOnlyHint)
	assert.True(tools["GetSecret"].Annotations.ReadOnlyHint)
	assert.True(*tools["DeleteSecret"].Annotations.DestructiveHint)
	assert.True(tools["GetPostgresEnv"].Annotations.ReadOnlyHint)
	assert.True(tools["ListOrganizations"].Annotations.ReadOnlyHint)
	assert.True(tools["CreateCluster"] == nil)
	assert.True(tools["ListClusterCreations"] == nil)
	assert.True(!tools["ApplyPlan"].Annotations.ReadOnlyHint)
	assert.True(tools["ApplyPlan"].Annotations.IdempotentHint)
	assert.True(*tools["RestartApp"].Annotations.DestructiveHint)
	assert.True(!tools["RestartApp"].Annotations.IdempotentHint)
	assert.True(!*tools["PlanAppRollback"].Annotations.DestructiveHint)
	assert.True(tools["GetAppMetrics"].Annotations.ReadOnlyHint)
	assert.True(tools["GetChartDefaults"].Annotations.ReadOnlyHint)
	assert.True(*tools["DeleteCustomChart"].Annotations.DestructiveHint)
	assert.True(*tools["DeleteApp"].Annotations.DestructiveHint)
	assert.True(tools["DeleteApp"].Annotations.IdempotentHint)
	assert.True(!tools["DeleteApp"].Annotations.ReadOnlyHint)
}

// resolveToolSchema requires a declared schema and resolves its references using
// the client-visible document alone. The library owns the returned pointer.
func resolveToolSchema(t *testing.T, value any) *jsonschema.Resolved {
	t.Helper()
	assert := is.New(t)
	assert.True(value != nil)
	data, err := json.Marshal(value)
	assert.NoErr(err)
	var schema jsonschema.Schema
	assert.NoErr(json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(nil)
	assert.NoErr(err)
	return resolved
}

// TestToolRequests owns the MCP-to-HTTP mapping, raw patch preservation, array
// results, accepted operations, and API errors without duplicating domain tests.
func TestToolRequests(t *testing.T) {
	for _, input := range []struct {
		name, tool, arguments, method, uri, body string
		status                                   int
		response                                 string
	}{
		{"registry deletion", "DeleteSecret", `{"path":{"organization":"team","space":"dev","secret":"ghcr.io"},"query":{"version":2}}`, "DELETE", "/api/v1/organizations/team/spaces/dev/secrets/ghcr.io?version=2", "", 200, `{"name":"ghcr.io"}`},
		{"app deletion", "DeleteApp", `{"path":{"organization":"team","space":"dev","application":"web"}}`, "DELETE", "/api/v1/organizations/team/spaces/dev/apps/web", "", 202, `{"id":"delete-web","phase":"deleting","status":"pending"}`},
		{"app deletion conflict", "DeleteApp", `{"path":{"organization":"team","space":"dev","application":"web"}}`, "DELETE", "/api/v1/organizations/team/spaces/dev/apps/web", "", 409, `{"message":"wait for the active operation"}`},
		{"managed restore", "RestorePostgres", `{"path":{"organization":"team","space":"dev","postgres":"restored"},"body":{"backupId":"approved"}}`, "POST", "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/restored/restore", `{"backupId":"approved"}`, 202, `{"operation":"restore-1"}`},
		{"custom chart install", "PutCustomChart", `{"path":{"organization":"team","space":"dev","customChart":"cache"},"body":{"repository":"oci://registry.example.com/charts","chart":"redis","version":"1.2.3","logs":false,"values":{"replicas":0}}}`, "PUT", "/api/v1/organizations/team/spaces/dev/custom-charts/cache", `{"repository":"oci://registry.example.com/charts","chart":"redis","version":"1.2.3","logs":false,"values":{"replicas":0}}`, 202, `{"operation":"addon-1"}`},
		{"custom chart apply", "PutCustomChart", `{"path":{"organization":"team","space":"dev","customChart":"cache"},"query":{"apply":true},"body":{"repository":"oci://registry.example.com/charts","chart":"redis","version":"1.2.4"}}`, "PUT", "/api/v1/organizations/team/spaces/dev/custom-charts/cache?apply=true", `{"repository":"oci://registry.example.com/charts","chart":"redis","version":"1.2.4"}`, 202, `{"operation":"addon-2"}`},
		{"app logs", "GetAppLogs", `{"path":{"organization":"team","space":"dev","application":"web"},"query":{"range":"12h","search":"error timeout","limit":20}}`, "GET", "/api/v1/organizations/team/spaces/dev/apps/web/logs?limit=20&range=12h&search=error+timeout", "", 200, `{"entries":[]}`},
		{"app restart", "RestartApp", `{"path":{"organization":"team","space":"dev","application":"web"}}`, "POST", "/api/v1/organizations/team/spaces/dev/apps/web/restart", "", 202, `{"state":"running"}`},
		{"no arguments", "ListOrganizations", ``, "GET", "/api/v1/organizations", "", 200, `[]`},
		{"path", "PutOrganization", `{"path":{"organization":"team"}}`, "PUT", "/api/v1/organizations/team", "", 200, `{"name":"team"}`},
		{"raw patch", "ExpandApps", `{"body":{"apps":[{"name":"api","replicas":0,"predeploy":{},"env":{}}]}}`, "POST", "/api/v1/apps/expand", `{"apps":[{"name":"api","replicas":0,"predeploy":{},"env":{}}]}`, 200, `{"apps":[]}`},
		{"query", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":true}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff?upgrade=true", "", 200, `{}`},
		{"explicit false", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":false}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff?upgrade=false", "", 200, `{}`},
		{"omitted query", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff", "", 200, `{}`},
		{"queued operation", "ApplyPlan", `{"path":{"plan":"plan-1"}}`, "POST", "/api/v1/plans/plan-1/apply", "", 202, `{"id":"operation-1","status":"queued"}`},
		{"primary storage test", "PutManagementS3", `{"body":{"endpoint":"https://storage.example.com","region":"us-east-1","bucket":"management-data","pathStyle":true,"accessKey":"private-access","secretKey":"private-secret"}}`, "PUT", "/api/v1/management/s3", `{"endpoint":"https://storage.example.com","region":"us-east-1","bucket":"management-data","pathStyle":true,"accessKey":"private-access","secretKey":"private-secret"}`, 202, `{"id":"storage-test-1","status":"queued","message":"management configuration accepted"}`},
		{"offsite storage test", "ApplyManagementConfiguration", `{"body":{"offsite":{"enabled":true,"repository":"s3:https://storage.example.com/offsite/borq","region":"us-east-1","interval":"6h","keepLast":14,"password":"private-password","accessKey":"private-access","secretKey":"private-secret"}}}`, "PUT", "/api/v1/management/configuration", `{"offsite":{"enabled":true,"repository":"s3:https://storage.example.com/offsite/borq","region":"us-east-1","interval":"6h","keepLast":14,"password":"private-password","accessKey":"private-access","secretKey":"private-secret"}}`, 202, `{"id":"offsite-test-1","status":"queued"}`},
		{"storage test progress", "GetManagementOperation", `{"path":{"operation":"storage-test-1"}}`, "GET", "/api/v1/management/operations/storage-test-1", "", 200, `{"id":"storage-test-1","status":"failed","message":"replacement S3 connection failed normal list/write/read/delete operations"}`},
		{"api error", "GetPlan", `{"path":{"plan":"missing"}}`, "GET", "/api/v1/plans/missing", "", 404, `{"message":"not found"}`},
		{"plain error", "GetPlan", `{"path":{"plan":"missing"}}`, "GET", "/api/v1/plans/missing", "", 503, `unavailable`},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			type capturedRequest struct{ method, uri, body, contentType string }
			captured := make(chan capturedRequest, 1)
			ctx, session := connectTools(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					http.Error(writer, "read request", http.StatusInternalServerError)
					return
				}
				captured <- capturedRequest{request.Method, request.URL.RequestURI(), string(body), request.Header.Get("Content-Type")}
				writer.WriteHeader(input.status)
				_, _ = io.WriteString(writer, input.response)
			}))
			parameters := &mcp.CallToolParams{Name: input.tool}
			if input.arguments != "" {
				parameters.Arguments = json.RawMessage(input.arguments)
			}
			result, err := session.CallTool(ctx, parameters)
			assert.NoErr(err)
			assert.Equal(result.IsError, input.status >= 400)
			assert.Equal(result.Content[0].(*mcp.TextContent).Text, input.response)
			structured := result.StructuredContent.(map[string]any)
			assert.Equal(structured["status"], float64(input.status))
			var expected any
			if json.Unmarshal([]byte(input.response), &expected) != nil {
				expected = input.response
			}
			assert.Equal(structured["body"], expected)
			select {
			case request := <-captured:
				assert.Equal(request.method, input.method)
				assert.Equal(request.uri, input.uri)
				assert.Equal(request.body, input.body)
				contentType := ""
				if input.body != "" {
					contentType = "application/json"
				}
				assert.Equal(request.contentType, contentType)
			default:
				t.Fatal("tool did not reach the API")
			}
		})
	}
}

// TestInvalidToolArguments prevents invalid paths, unexpected fields, and null
// bodies from reaching the API, while returning errors the client can correct.
func TestInvalidToolArguments(t *testing.T) {
	ctx, session := connectTools(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("invalid arguments reached the API")
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	for _, input := range []struct{ tool, arguments, guidance string }{
		{"PutOrganization", `{}`, ""},
		{"PutOrganization", `{"path":{"organization":"../escape"}}`, ""},
		{"PutOrganization", `{"path":{"organization":"team"},"body":{"unexpected":true}}`, ""},
		{"ExpandApps", `{"body":null}`, ""},
		{"ExpandApps", `{"body":{"unexpected":true}}`, ""},
		{"ExpandApps", `{"body":{"apps":[{"name":"api","replicas":-1}]}}`, ""},
		{"ListOrganizations", `[]`, ""},
		{"ListOrganizations", `null`, ""},
		{"DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":"true"}}`, ""},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"Uppercase"},"body":{}}`, "PostgreSQL names must contain 1 to 47 lowercase letters"},
		{"PutPostgres", `{"path":{"organization":"team","space":"dev","postgres":"database"},"body":{"private-vault-token":"private-vault-token"}}`, "Passwords are generated automatically."},
	} {
		t.Run(input.tool+input.arguments, func(t *testing.T) {
			assert := is.New(t)
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: input.tool, Arguments: json.RawMessage(input.arguments)})
			assert.NoErr(err)
			assert.True(result.IsError)
			assert.True(len(result.Content) > 0)
			text := result.Content[0].(*mcp.TextContent).Text
			assert.True(strings.Contains(text, input.guidance))
			assert.True(!strings.Contains(text, "private-vault-token"))
		})
	}
}

// connectTools uses the real SDK transport; the returned session is a pointer
// because the SDK owns its connection and cleanup lifecycle.
func connectTools(t *testing.T, api http.Handler) (context.Context, *mcp.ClientSession) {
	t.Helper()
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	handler, err := NewHandler(api)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "borq-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.Client()}, nil)
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(session.Close()) })
	return ctx, session
}
