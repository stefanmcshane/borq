package appmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestApplicationRoutePolicyTransportParity owns API/MCP agreement on a real
// manager rejection, including the actionable domain guidance returned to clients.
func TestApplicationRoutePolicyTransportParity(t *testing.T) {
	assert := is.New(t)
	cluster := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/apis/cert-manager.io/v1/clusterissuers/pinned")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"apiVersion":"cert-manager.io/v1","kind":"ClusterIssuer","metadata":{"name":"pinned"},"spec":{"acme":{"solvers":[{"selector":{"dnsZones":["example.test"]},"http01":{"ingress":{}}}]}}}`))
	}))
	t.Cleanup(cluster.Close)
	configuration := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(configuration, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    server: "+cluster.URL+"\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: test\ncurrent-context: target\nusers:\n- name: test\n  user: {}\n"), 0600))
	file := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := sqlite.New(t.Context(), file)
	assert.NoErr(err)
	space := apps.Space{ID: strings.Repeat("a", 32), Organization: "team", Name: "dev", Cluster: "local", Namespace: "borq-space", CertificateIssuer: "pinned", Configuration: apps.Configuration{Apps: []apps.App{}}}
	storedSpace, err := json.Marshal(map[string]any{"space": space})
	assert.NoErr(err)
	storedCluster, err := json.Marshal(apps.Cluster{ID: platform.ClusterID("team", "local"), Organization: "team", Name: "local", KubeconfigPath: configuration})
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{{Collection: "space", Key: "team/dev", Value: storedSpace}, {Collection: "cluster", Key: "team/local", Value: storedCluster}}))
	assert.NoErr(database.Close())
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: file})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	ctx, session := connectTools(t, api)
	body := `{"apps":[{"name":"web","image":"traefik/whoami","port":80,"ingresses":[{"name":"public","host":"web.outside.test","path":"/"}]}]}`
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/organizations/team/spaces/dev/plans", strings.NewReader(body)))
	assert.Equal(response.Code, http.StatusBadRequest)
	var problem appapi.Problem
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
	assert.True(strings.Contains(problem.Message, "pinned"))
	assert.True(strings.Contains(problem.Message, "example.test"))
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "PostPlan", Arguments: json.RawMessage(`{"path":{"organization":"team","space":"dev"},"body":` + body + `}`)})
	assert.NoErr(err)
	assert.True(result.IsError)
	assert.Equal(result.Content[0].(*mcp.TextContent).Text, response.Body.String())
}
