package appserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestMcpApiWorkflow proves that the server exposes MCP tools backed by the same
// application state and validation as HTTP, including management readiness gates.
func TestMcpApiWorkflow(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	directory := t.TempDir()
	runtime, err := New(ctx, NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	defer func() { assert.NoErr(runtime.Close()) }()
	_, err = runtime.manager.CreateOrganization(ctx, "tools")
	assert.NoErr(err)
	server := httptest.NewServer(runtime.http.Handler)
	defer server.Close()
	token, err := os.ReadFile(filepath.Join(directory, "admin-token"))
	assert.NoErr(err)
	httpClient := server.Client()
	httpClient.Transport = mcpAdminTestTransport{base: httpClient.Transport, token: strings.TrimSpace(string(token))}
	client := mcp.NewClient(&mcp.Implementation{Name: "borq-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/api/mcp", HTTPClient: httpClient}, nil)
	assert.NoErr(err)
	defer func() { assert.NoErr(session.Close()) }()

	created, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "PutOrganization", Arguments: map[string]any{"path": map[string]any{"organization": "tools"}}})
	assert.NoErr(err)
	assert.True(!created.IsError)
	response, err := httpClient.Get(server.URL + "/api/v1/organizations/tools/spaces")
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.StatusCode, http.StatusOK)
	var spaces []appapi.Space
	assert.NoErr(json.NewDecoder(response.Body).Decode(&spaces))
	assert.Equal(spaces, []appapi.Space{})

	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ListOrganizations"})
	assert.NoErr(err)
	assert.True(!listed.IsError)
	var organizations []appapi.Organization
	assert.NoErr(json.Unmarshal([]byte(listed.Content[0].(*mcp.TextContent).Text), &organizations))
	assert.Equal(len(organizations), 1)
	assert.Equal(organizations[0].Name, "tools")

	duplicate, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ExpandApps", Arguments: json.RawMessage(`{"body":{"apps":[],"apps":[]}}`)})
	assert.NoErr(err)
	assert.True(duplicate.IsError)
	assert.Equal(duplicate.StructuredContent.(map[string]any)["status"], float64(http.StatusBadRequest))
	assert.True(strings.Contains(duplicate.Content[0].(*mcp.TextContent).Text, "unique keys"))

	// A valid plan identifier must reach the readiness guard before lookup.
	runtime.management.mutex.Lock()
	runtime.management.enabled = true
	runtime.management.state.Ready = false
	runtime.management.mutex.Unlock()
	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "GetPlan", Arguments: map[string]any{"path": map[string]any{"plan": "missing"}}})
	assert.NoErr(err)
	assert.True(blocked.IsError)
	assert.Equal(blocked.StructuredContent.(map[string]any)["status"], float64(http.StatusServiceUnavailable))
	assert.True(strings.Contains(blocked.Content[0].(*mcp.TextContent).Text, "Management components are not ready"))
}

// TestMcpRequestProtection covers browser-origin and DNS-rebinding rejection at
// the actual listener, and prevents caching of secret-bearing MCP responses.
func TestMcpRequestProtection(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	runtime, err := New(context.Background(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	defer func() { assert.NoErr(runtime.Close()) }()
	server := httptest.NewServer(runtime.http.Handler)
	defer server.Close()
	for _, input := range []struct {
		name, origin, host string
		status             int
	}{
		{"trusted client", "", "", http.StatusOK},
		{"cross origin", "https://untrusted.example", "", http.StatusForbidden},
		{"rebinding", "", "untrusted.example", http.StatusForbidden},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			request, err := http.NewRequest(http.MethodPost, server.URL+"/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			assert.NoErr(err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			authorizeAdminTestRequest(t, directory, request)
			if input.origin != "" {
				request.Header.Set("Origin", input.origin)
			}
			if input.host != "" {
				request.Host = input.host
			}
			response, err := server.Client().Do(request)
			assert.NoErr(err)
			defer response.Body.Close()
			assert.Equal(response.StatusCode, input.status)
			if input.status == http.StatusOK {
				assert.Equal(response.Header.Get("Cache-Control"), "no-store")
				_, err := io.Copy(io.Discard, response.Body)
				assert.NoErr(err)
			}
		})
	}
}

type mcpAdminTestTransport struct {
	base  http.RoundTripper
	token string
}

func (transport mcpAdminTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.base.RoundTrip(request)
}
