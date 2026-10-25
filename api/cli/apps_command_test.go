package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/logger"
	"k8s.io/client-go/tools/clientcmd"
)

// TestPlanAndApplyReuseExactSavedPlan verifies the CLI saves a private plan
// reference and applies it without requesting another server-side plan.
func TestPlanAndApplyReuseExactSavedPlan(t *testing.T) {
	is := is.New(t)
	plans, applies, polls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/plans"):
			plans++
			if request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("plan must send JSON, got %s", request.Header.Get("Content-Type"))
			}
			var patch appapi.Patch
			if err := json.NewDecoder(request.Body).Decode(&patch); err != nil || patch.Apps == nil || len(*patch.Apps) != 1 {
				t.Errorf("plan did not convert its YAML file into an application JSON request: %v", err)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "plan-1", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api", Image: "example/api:v1", Replicas: 1}}}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/plans/plan-1/apply":
			applies++
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation-1", PlanID: "plan-1", Status: appapi.Pending})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/operations/operation-1":
			polls++
			status := appapi.Waiting
			if polls > 1 {
				status = appapi.Succeeded
			}
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation-1", PlanID: "plan-1", Status: status, Phase: appapi.OperationPhaseApplications, Message: "waiting for ready replicas"})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/operations/plan-1":
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "operation not found"})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "apps.yaml")
	is.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  image: example/api:v1\n"), 0o600))
	flags := []string{"--server", server.URL, "--organization", "team", "--space", "dev", "-f", file}
	output, diagnostics, err := runApplicationCommand(t, append([]string{"plan"}, flags...)...)
	is.NoErr(err)
	is.True(strings.Contains(output, "replicas: 1"))
	is.True(strings.Contains(diagnostics, "plan-1"))
	metadata, err := os.Stat(file + ".borq-plan.json")
	is.NoErr(err)
	is.Equal(metadata.Mode().Perm(), os.FileMode(0o600))
	is.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  image: example/api:v2\n"), 0o600))
	_, _, err = runApplicationCommand(t, append(append([]string{"apply"}, flags...), "--plan-file", file+".borq-plan.json")...)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "changed since planning"))
	is.Equal(plans, 1)
	is.Equal(applies, 0)
	is.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  image: example/api:v1\n"), 0o600))
	other := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("reviewed plan must reject another server before sending a request")
	}))
	defer other.Close()
	for _, scope := range []struct{ server, organization, space string }{
		{other.URL, "team", "dev"}, {server.URL, "other", "dev"}, {server.URL, "team", "production"},
	} {
		_, _, err := runApplicationCommand(t, "apply", "--server", scope.server, "--organization", scope.organization, "--space", scope.space, "-f", file, "--plan-file", file+".borq-plan.json")
		is.True(err != nil)
		is.True(strings.Contains(err.Error(), "changed since planning"))
	}
	is.Equal(plans, 1)
	is.Equal(applies, 0)
	_, diagnostics, err = runApplicationCommand(t, append(append([]string{"apply"}, flags...), "--wait")...)
	is.NoErr(err)
	is.True(strings.Contains(diagnostics, "waiting for ready replicas"))
	is.Equal(plans, 1)
	is.Equal(applies, 1)
}

// TestApplyCreatesPlanWhenAbsent checks that a first apply performs exactly one
// planning request and stores that plan before accepting the apply operation.
func TestApplyCreatesPlanWhenAbsent(t *testing.T) {
	is := is.New(t)
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/plans") {
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "plan-2", ConfigurationDiff: "+ image: example/api:v1\n", ResourceDiffs: []appapi.ResourceDiff{{Action: appapi.Create, Kind: "Deployment", Namespace: "team-dev", Name: "api"}}})
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation-2", PlanID: "plan-2", Status: appapi.Pending})
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "apps.yaml")
	is.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  image: example/api:v1\n"), 0o600))
	output, diagnostics, err := runApplicationCommand(t, "apply", "--server", server.URL, "--organization", "team", "--space", "dev", "-f", file)
	is.NoErr(err)
	is.Equal(requests, []string{"/api/v1/organizations/team/spaces/dev/plans", "/api/v1/plans/plan-2/apply"})
	is.True(strings.Contains(output, "operation-2"))
	is.True(strings.Contains(diagnostics, "+ image: example/api:v1"))
	is.True(strings.Contains(diagnostics, "create Deployment team-dev/api"))
	data, err := os.ReadFile(file + ".borq-plan.json")
	is.NoErr(err)
	var metadata savedApplicationPlan
	is.NoErr(json.Unmarshal(data, &metadata))
	is.True(metadata.Automatic)
}

// TestPlanValidatesYamlBeforeTransport ensures nulls, unknown fields, malformed
// lists, and missing or overlong app names are rejected before sending requests.
func TestPlanValidatesYamlBeforeTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("invalid YAML must fail before a request")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"id":"unexpected"}`))
	}))
	defer server.Close()
	for _, source := range []string{
		"null\n", "apps: null\n", "apps:\n- image: api:v1\n", "app: []\n",
		"apps:\n- name: api\n  env: null\n", "apps: []\napps: []\n", "apps: []\n---\napps: []\n", "apps: []\n---\n",
		"apps:\n- name: api\n  env:\n    name: backend\n    version: true\n",
		"apps:\n- name: " + strings.Repeat("a", 51) + "\n  image: api:v1\n",
		"apps:\n- name: api\n  command: [serve]\n",
		"apps:\n- name: api\n  args: []\n",
		"apps:\n- name: api\n  predeploy: []\n",
		"apps:\n- name: api\n  predeploy:\n    name: migrate\n",
		"apps:\n- name: api\n  readiness:\n    enabled: false\n",
		"apps:\n- name: api\n  startup:\n    enabled: false\n",
		"apps:\n- name: api\n  checks:\n    readiness: null\n",
		"apps:\n- name: api\n  ingresses:\n  - name: public\n    host: example.com\n    tlsSecret: app-tls\n",
	} {
		is := is.New(t)
		file := filepath.Join(t.TempDir(), "apps.yaml")
		is.NoErr(os.WriteFile(file, []byte(source), 0o600))
		_, _, err := runApplicationCommand(t, "plan", "--server", server.URL, "--organization", "team", "--space", "dev", "-f", file)
		is.True(err != nil)
		is.True(strings.Contains(err.Error(), "apps file"))
	}
}

// TestApplicationYamlRoundTrip verifies the CLI's YAML input and output preserve
// the expanded JSON API model, including explicit zeros, empty lists, and probes.
func TestApplicationYamlRoundTrip(t *testing.T) {
	assert := is.New(t)
	handler, err := appapi.NewHandler(apps.Manager{})
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := applicationClient(server.URL)
	assert.NoErr(err)
	source, err := os.ReadFile("testdata/roundtrip.yaml")
	assert.NoErr(err)
	patch, err := decodeApplicationPatch(source)
	assert.NoErr(err)
	response, err := client.ExpandAppsWithResponse(context.Background(), patch)
	assert.NoErr(err)
	if response.JSON200 == nil {
		t.Fatalf("JSON expansion failed: %s", response.Body)
	}
	configuration := *response.JSON200
	assert.Equal(configuration.Apps[0].Replicas, 0)
	assert.Equal(configuration.Apps[1].Ingresses[0].Host, "app.borq.test")
	assert.Equal(configuration.Apps[1].Checks.Startup.FailureThreshold, 60)
	assert.True(configuration.Apps[1].Command != "")
	assert.True(configuration.Apps[0].Predeploy.Command != "")
	assert.Equal(configuration.Apps[0].Predeploy.TimeoutSeconds, 120)
	var output bytes.Buffer
	assert.NoErr(printApplicationValue(&output, configuration))
	assert.True(strings.Contains(output.String(), "name: frontend-config"))
	assert.True(strings.Contains(output.String(), "replicas: 0"))
	assert.True(strings.Contains(output.String(), "enabled: false"))
	assert.True(strings.Contains(output.String(), "checks:"))
	assert.True(!strings.Contains(output.String(), "args:"))
	assert.True(!strings.Contains(output.String(), "tlsSecret:"))
	imported, err := decodeApplicationPatch(output.Bytes())
	assert.NoErr(err)
	repeated, err := client.ExpandAppsWithResponse(context.Background(), imported)
	assert.NoErr(err)
	if repeated.JSON200 == nil {
		t.Fatalf("expanded YAML could not be reimported as JSON: %s", repeated.Body)
	}
	assert.Equal(*repeated.JSON200, configuration)
}

// TestApplyWaitReturnsFailure verifies the command's exit status reflects the
// server's failed rollout when waiting, including when applying an explicit ID.
func TestApplyWaitReturnsFailure(t *testing.T) {
	for _, status := range []appapi.OperationStatus{appapi.Failed, appapi.Canceled, appapi.Superseded} {
		t.Run(string(status), func(t *testing.T) {
			is := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPost {
					writer.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation-1", Status: appapi.Pending})
					return
				}
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "operation-1", Status: status, Message: "deployment exceeded its progress deadline"})
			}))
			defer server.Close()
			_, _, err := runApplicationCommand(t, "apply", "--server", server.URL, "--plan", "plan-1", "--wait")
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "progress deadline"))
			_, _, err = runApplicationCommand(t, "apply", "--server", server.URL, "--plan", "plan-1", "--wait", "--timeout", "0s")
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "greater than zero"))
		})
	}
}

// TestOrganizationCommandsUseGeneratedClient keeps organization creation and
// discovery on the JSON API and prints the server's organization-scoped result.
func TestOrganizationCommandsUseGeneratedClient(t *testing.T) {
	assert := is.New(t)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`[{"name":"team"}]`))
			return
		}
		_, _ = writer.Write([]byte(`{"name":"team"}`))
	}))
	defer server.Close()
	for _, arguments := range [][]string{{"org", "create", "team"}, {"org", "list"}} {
		output, _, err := runApplicationCommand(t, append(arguments, "--server", server.URL)...)
		assert.NoErr(err)
		assert.True(strings.Contains(output, "name: team"))
	}
	assert.Equal(requests, []string{"PUT /api/v1/organizations/team", "GET /api/v1/organizations"})
}

// TestSpaceCreateRequiresCluster rejects unbound spaces before sending an API request.
func TestSpaceCreateRequiresCluster(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("missing cluster must fail before sending a request")
	}))
	defer server.Close()
	_, _, err := runApplicationCommand(t, "space", "create", "preview", "--organization", "team", "--server", server.URL)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "provide --cluster"))
}

// TestClusterLinkEmbedsLocalCredentials proves a remote Borq server receives
// the kubeconfig contents instead of paths that only exist on the CLI machine.
func TestClusterLinkEmbedsLocalCredentials(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	assert.NoErr(os.WriteFile(filepath.Join(directory, "client.crt"), []byte("local-certificate"), 0o600))
	assert.NoErr(os.WriteFile(filepath.Join(directory, "client.key"), []byte("local-private-key"), 0o600))
	kubeconfig := filepath.Join(directory, "kubeconfig")
	assert.NoErr(os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
current-context: local
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: local
  context:
    cluster: local
    user: local
users:
- name: local
  user:
    client-certificate: client.crt
    client-key: client.key
`), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var link appapi.LinkClusterInput
		assert.NoErr(json.NewDecoder(request.Body).Decode(&link))
		input := link.Connection
		assert.True(input.KubeconfigPath == nil)
		assert.True(input.Kubeconfig != nil)
		configuration, err := clientcmd.Load([]byte(*input.Kubeconfig))
		assert.NoErr(err)
		assert.Equal(configuration.Clusters["local"].Server, "https://127.0.0.1:6443")
		assert.Equal(configuration.AuthInfos["local"].ClientCertificateData, []byte("local-certificate"))
		assert.Equal(configuration.AuthInfos["local"].ClientKeyData, []byte("local-private-key"))
		assert.Equal(configuration.AuthInfos["local"].ClientKey, "")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"name":"local"}`))
	}))
	defer server.Close()
	output, _, err := runApplicationCommand(t, "cluster", "link", "local", "--organization", "team", "--server", server.URL, "--kubeconfig", kubeconfig, "--reconcile=false", "--diff=false")
	assert.NoErr(err)
	assert.True(!strings.Contains(output, "local-private-key"))
}

// TestClusterLinkRejectsRemovedInClusterFlag keeps obsolete credential selection
// from being silently accepted by the CLI.
func TestClusterLinkRejectsRemovedInClusterFlag(t *testing.T) {
	assert := is.New(t)
	_, _, err := runApplicationCommand(t, "cluster", "link", "local", "--organization", "team", "--in-cluster")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "unknown flag: --in-cluster"))
}

// TestHierarchyListPropagatesApiFailure keeps failed lookups distinguishable
// from a successful list containing no organizations, clusters, or spaces.
func TestHierarchyListPropagatesApiFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"organization was not found"}`))
	}))
	defer server.Close()
	for _, arguments := range [][]string{
		{"org", "list"}, {"cluster", "list", "--organization", "missing"}, {"space", "list", "--organization", "missing"},
	} {
		assert := is.New(t)
		_, _, err := runApplicationCommand(t, append(arguments, "--server", server.URL)...)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "organization was not found"))
	}
}

// TestApplicationCommandsUseSpaceScope prevents obsolete project/environment
// flags from becoming requirements after the organization/space simplification.
func TestApplicationCommandsUseSpaceScope(t *testing.T) {
	for _, arguments := range [][]string{
		{"plan", "--help"}, {"apply", "--help"}, {"space", "create", "--help"},
		{"environment-group", "list", "--help"}, {"environment-group", "create", "--help"},
		{"apps", "list", "--help"}, {"apps", "history", "--help"}, {"apps", "cancel", "--help"},
	} {
		assert := is.New(t)
		output, _, err := runApplicationCommand(t, arguments...)
		assert.NoErr(err)
		assert.True(!strings.Contains(output, "--project"))
		assert.True(!strings.Contains(output, "--environment"))
	}
	for _, arguments := range [][]string{
		{"cluster", "link", "dev"}, {"cluster", "link", "dev", "--organization="},
		{"cluster", "list"}, {"space", "list"}, {"space", "create", "dev"}, {"apps", "history", "api"},
	} {
		assert := is.New(t)
		_, _, err := runApplicationCommand(t, arguments...)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "organization"))
	}
	for _, group := range []string{"project", "environment"} {
		assert := is.New(t)
		_, _, err := runApplicationCommand(t, group, "list")
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "unknown command"))
	}
}

// TestSavedPlanRejectsLegacyProjectMetadata requires replanning rather than
// silently applying a plan reviewed under the removed project hierarchy.
func TestSavedPlanRejectsLegacyProjectMetadata(t *testing.T) {
	assert := is.New(t)
	file := filepath.Join(t.TempDir(), "apps.yaml")
	source := []byte("apps: []\n")
	assert.NoErr(os.WriteFile(file, source, 0o600))
	options := deploymentOptions{file: file, scope: applicationScope{server: "http://localhost:7833", organization: "team", space: "dev"}}
	metadata := applicationPlanMetadata(options, source, "plan-1")
	data, err := json.Marshal(metadata)
	assert.NoErr(err)
	legacy := strings.TrimSuffix(string(data), "}") + `,"project":"shop","environment":"dev"}`
	assert.NoErr(os.WriteFile(applicationPlanPath(options), []byte(legacy), 0o600))
	_, err = loadApplicationPlan(context.Background(), nil, options)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "run plan again"))
}

func runApplicationCommand(t *testing.T, arguments ...string) (string, string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return runConfiguredApplicationCommand(arguments...)
}

func runConfiguredApplicationCommand(arguments ...string) (string, string, error) {
	command := NewRootCommand(context.Background(), logger.Logger{}, Config{Port: 7833, DataDir: "/tmp/borq"})
	var output, diagnostics bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	command.SetArgs(arguments)
	err := command.Execute()
	return output.String(), diagnostics.String(), err
}
