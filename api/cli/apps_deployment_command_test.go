package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

// TestAppsHistoryUsesProfileAndScope preserves server ordering, release details,
// and operation IDs in YAML while explicit scope flags override profile defaults.
func TestAppsHistoryUsesProfileAndScope(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	accepted := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	deployments := []appapi.Deployment{
		{Revision: "revision-2", Operation: appapi.Operation{ID: "deployment-2", PlanID: "plan-2", Status: appapi.Failed, StartedAt: accepted, Message: "predeploy failed"}, Configuration: appapi.App{Name: "api", Image: "example/api:v2"}},
		{Revision: "revision-1", Operation: appapi.Operation{ID: "deployment-1", PlanID: "plan-1", Status: appapi.Succeeded, StartedAt: accepted.Add(-time.Hour)}, Configuration: appapi.App{Name: "api", Image: "example/api:v1"}},
	}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		paths = append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(deployments)
	}))
	t.Cleanup(server.Close)
	_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization", "team", "--space", "dev")
	assert.NoErr(err)
	for _, arguments := range [][]string{
		{"apps", "history", "api", "--profile", "work"},
		{"apps", "history", "api", "--profile", "work", "--organization", "other", "--space", "preview"},
	} {
		output, diagnostics, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
		assert.Equal(diagnostics, "")
		var actual []appapi.Deployment
		assert.NoErr(yaml.Unmarshal([]byte(output), &actual))
		assert.Equal(actual, deployments)
	}
	assert.Equal(paths, []string{
		"/api/v1/organizations/team/spaces/dev/apps/api/deployments",
		"/api/v1/organizations/other/spaces/preview/apps/api/deployments",
	})
	deployments = []appapi.Deployment{}
	output, _, err := runConfiguredApplicationCommand("apps", "history", "api", "--profile", "work")
	assert.NoErr(err)
	assert.Equal(output, "[]\n")
}

// TestAppsCancelUsesProfileWithoutSpace prints the asynchronous acknowledgment
// without polling or requiring unrelated organization and space settings.
func TestAppsCancelUsesProfileWithoutSpace(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	operation := appapi.Operation{ID: "deployment-2", PlanID: "plan-2", Status: appapi.Canceling, Message: "stopping predeploy tasks"}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/operations/deployment-2/cancel")
		body, err := io.ReadAll(request.Body)
		assert.NoErr(err)
		assert.Equal(len(body), 0)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(operation)
	}))
	t.Cleanup(server.Close)
	_, _, err := runConfiguredApplicationCommand("config", "set", "--profile", "work", "--api-url", server.URL, "--organization=", "--space=")
	assert.NoErr(err)
	output, diagnostics, err := runConfiguredApplicationCommand("apps", "cancel", operation.ID, "--profile", "work")
	assert.NoErr(err)
	assert.Equal(diagnostics, "")
	var actual appapi.Operation
	assert.NoErr(yaml.Unmarshal([]byte(output), &actual))
	assert.Equal(actual, operation)
	assert.Equal(requests, 1)
}

// TestAppsDeploymentCommandsReportApiErrors keeps missing history and rejected
// cancellation failures visible to scripts without writing success output.
func TestAppsDeploymentCommandsReportApiErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		method    string
		path      string
		status    int
		message   string
	}{
		{"history", []string{"apps", "history", "missing", "--organization", "team", "--space", "dev"}, http.MethodGet, "/api/v1/organizations/team/spaces/dev/apps/missing/deployments", http.StatusNotFound, "application not found"},
		{"cancel", []string{"apps", "cancel", "deployment-2"}, http.MethodPost, "/api/v1/operations/deployment-2/cancel", http.StatusConflict, "cancellation rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				assert.Equal(request.Method, test.method)
				assert.Equal(request.URL.Path, test.path)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: test.message})
			}))
			t.Cleanup(server.Close)
			output, _, err := runApplicationCommand(t, append(test.arguments, "--api-url", server.URL)...)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			assert.Equal(output, "")
			assert.Equal(requests, 1)
		})
	}
}
