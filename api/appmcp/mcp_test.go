package appmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolDiscovery verifies that clients receive independently resolvable JSON
// Schemas, including nested request bodies and required path/query parameters.
func TestToolDiscovery(t *testing.T) {
	assert := is.New(t)
	ctx, session := connectTools(t, http.NotFoundHandler())
	listed, err := session.ListTools(ctx, nil)
	assert.NoErr(err)
	assert.True(len(listed.Tools) > 0)
	schemas := map[string]*jsonschema.Resolved{}
	tools := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		data, err := json.Marshal(tool.InputSchema)
		assert.NoErr(err)
		var schema jsonschema.Schema
		assert.NoErr(json.Unmarshal(data, &schema))
		resolved, err := schema.Resolve(nil)
		assert.NoErr(err)
		schemas[tool.Name] = resolved
		tools[tool.Name] = tool
		assert.True(tool.Description != "")
	}
	for _, input := range []struct {
		name      string
		arguments string
		valid     bool
	}{
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
	assert.True(tools["ListOrganizations"].Annotations.ReadOnlyHint)
	assert.True(tools["CreateCluster"] == nil)
	assert.True(tools["ListClusterCreations"] == nil)
	assert.True(!tools["ApplyPlan"].Annotations.ReadOnlyHint)
}

// TestToolRequests owns the MCP-to-HTTP mapping, raw patch preservation, array
// results, accepted operations, and API errors without duplicating domain tests.
func TestToolRequests(t *testing.T) {
	for _, input := range []struct {
		name, tool, arguments, method, uri, body string
		status                                   int
		response                                 string
	}{
		{"no arguments", "ListOrganizations", ``, "GET", "/api/v1/organizations", "", 200, `[]`},
		{"path", "PutOrganization", `{"path":{"organization":"team"}}`, "PUT", "/api/v1/organizations/team", "", 200, `{"name":"team"}`},
		{"raw patch", "ExpandApps", `{"body":{"apps":[{"name":"api","replicas":0,"predeploy":{},"env":{}}]}}`, "POST", "/api/v1/apps/expand", `{"apps":[{"name":"api","replicas":0,"predeploy":{},"env":{}}]}`, 200, `{"apps":[]}`},
		{"query", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":true}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff?upgrade=true", "", 200, `{}`},
		{"explicit false", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":false}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff?upgrade=false", "", 200, `{}`},
		{"omitted query", "DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"}}`, "GET", "/api/v1/organizations/team/clusters/local/components/diff", "", 200, `{}`},
		{"queued operation", "ApplyPlan", `{"path":{"plan":"plan-1"}}`, "POST", "/api/v1/plans/plan-1/apply", "", 202, `{"id":"operation-1","status":"queued"}`},
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
	for _, input := range []struct{ tool, arguments string }{
		{"PutOrganization", `{}`},
		{"PutOrganization", `{"path":{"organization":"../escape"}}`},
		{"PutOrganization", `{"path":{"organization":"team"},"body":{}}`},
		{"ExpandApps", `{"body":null}`},
		{"ExpandApps", `{"body":{"unexpected":true}}`},
		{"ExpandApps", `{"body":{"apps":[{"name":"api","replicas":-1}]}}`},
		{"ListOrganizations", `[]`},
		{"ListOrganizations", `null`},
		{"DiffClusterComponents", `{"path":{"organization":"team","cluster":"local"},"query":{"upgrade":"true"}}`},
	} {
		t.Run(input.tool+input.arguments, func(t *testing.T) {
			assert := is.New(t)
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: input.tool, Arguments: json.RawMessage(input.arguments)})
			assert.NoErr(err)
			assert.True(result.IsError)
			assert.True(len(result.Content) > 0)
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
