package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestDecodeBodyPreservesPatchPresence verifies that schema validation and model
// conversion keep omitted fields distinct from explicit empty lists and zeros.
func TestDecodeBodyPreservesPatchPresence(t *testing.T) {
	is := is.New(t)
	specification, err := GetSwagger()
	is.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, input := range []struct {
		name, body string
		assert     func(apps.Patch)
	}{
		{"omitted", `{}`, func(patch apps.Patch) { is.True(patch.Apps == nil) }},
		{"empty", `{"apps":[]}`, func(patch apps.Patch) {
			is.True(patch.Apps != nil)
			is.Equal(len(*patch.Apps), 0)
		}},
		{"explicit zero and empty", `{"apps":[{"name":"api","replicas":0,"env":{},"command":"","predeploy":{"command":"","backoffLimit":0},"checks":{"readiness":{"enabled":false}}}]}`, func(patch apps.Patch) {
			app := (*patch.Apps)[0]
			is.True(app.Image == nil)
			is.True(app.Replicas != nil)
			is.Equal(*app.Replicas, 0)
			is.True(app.Env != nil)
			is.Equal(*app.Env, apps.EnvReference{})
			is.True(app.Command != nil)
			is.Equal(*app.Command, "")
			is.True(app.Predeploy != nil)
			is.True(app.Predeploy.Command != nil)
			is.Equal(*app.Predeploy.Command, "")
			is.True(app.Predeploy.BackoffLimit != nil)
			is.Equal(*app.Predeploy.BackoffLimit, 0)
			is.True(app.Checks != nil)
			is.True(app.Checks.Readiness != nil)
			is.True(app.Checks.Readiness.Enabled != nil)
			is.Equal(*app.Checks.Readiness.Enabled, false)
			is.True(app.Checks.Startup == nil)
		}},
		{"empty nested objects", `{"apps":[{"name":"api","predeploy":{},"checks":{}}]}`, func(patch apps.Patch) {
			app := (*patch.Apps)[0]
			is.True(app.Predeploy != nil)
			is.True(app.Predeploy.Command == nil)
			is.True(app.Predeploy.TimeoutSeconds == nil)
			is.True(app.Checks != nil)
			is.True(app.Checks.Readiness == nil)
			is.True(app.Checks.Startup == nil)
		}},
	} {
		t.Run(input.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(input.body))
			var body Patch
			is.True(server.decodeBody(writer, request, "Patch", &body))
			patch, err := convert[apps.Patch](body)
			is.NoErr(err)
			input.assert(patch)
		})
	}
}

// TestDocumentationRoutes keeps the embedded contract and interactive explorer
// available without runtime files and restricts the explorer to read requests.
func TestDocumentationRoutes(t *testing.T) {
	is := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	is.NoErr(err)
	for _, input := range []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, "/api/openapi.yaml", "application/yaml", http.StatusOK},
		{http.MethodGet, "/api/docs", "text/html", http.StatusTemporaryRedirect},
		{http.MethodGet, "/api/docs/", "text/html", http.StatusOK},
		{http.MethodHead, "/api/docs/", "text/html", http.StatusOK},
		{http.MethodGet, "/api/docs/swagger-ui-bundle.js", "text/javascript", http.StatusOK},
		{http.MethodGet, "/api/docs/swagger-ui.css", "text/css", http.StatusOK},
		{http.MethodGet, "/api/docs/missing", "text/plain", http.StatusNotFound},
		{http.MethodPost, "/api/docs/", "text/plain", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/organizations/team/cluster-creations/target", "text/plain", http.StatusNotFound},
		{http.MethodGet, "/api/v1/organizations/team/cluster-creations", "text/plain", http.StatusNotFound},
	} {
		t.Run(input.method+" "+input.path, func(t *testing.T) {
			is := is.New(t)
			writer := httptest.NewRecorder()
			transport.ServeHTTP(writer, httptest.NewRequest(input.method, input.path, nil))
			is.Equal(writer.Code, input.status)
			is.True(strings.HasPrefix(writer.Header().Get("Content-Type"), input.contentType))
			if input.path == "/api/openapi.yaml" {
				is.Equal(writer.Body.Bytes(), openapiDocument)
			}
			if input.path == "/api/docs" {
				is.Equal(writer.Header().Get("Location"), "/api/docs/")
			}
			if input.method == http.MethodHead {
				is.Equal(writer.Body.Len(), 0)
			}
		})
	}
}

// TestErrorResponses distinguishes expected workflow failures from internal
// errors while ensuring secret-bearing wrapped errors never reach API clients.
func TestErrorResponses(t *testing.T) {
	check := is.New(t)
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := sqlite.New(ctx, databasePath)
	check.NoErr(err)
	check.NoErr(database.Commit(ctx, []sqlite.Write{
		{Collection: "space", Key: "team/dev", Value: json.RawMessage(`{"space":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","organization":"team","name":"dev","cluster":"target","namespace":"team-dev"}}`)},
		{Collection: "postgres", Key: "team/dev/database", Value: json.RawMessage(`{"policyVersion":1,"organization":"team","space":"dev","postgres":{"name":"database","state":"ready"}}`)},
		{Collection: "postgres-archive", Key: "preserved", Value: json.RawMessage(`{"organization":"team"}`)},
	}))
	check.NoErr(database.Close())
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: databasePath})
	check.NoErr(err)
	defer manager.Close()
	_, postgresConflict := manager.PutPostgres(ctx, "team", "dev", "database", apps.PostgresInput{}, false)
	check.True(errors.Is(postgresConflict, apps.ErrConflict))
	storageConflict := manager.DeleteManagementS3(ctx)
	check.True(errors.Is(storageConflict, apps.ErrConflict))
	_, postgresError := (apps.Manager{}).RestorePostgres(context.Background(), "team", "dev", "database", apps.PostgresRestoreInput{})
	for _, input := range []struct {
		err      error
		status   int
		guidance []string
	}{
		{apps.ErrNotFound, http.StatusNotFound, nil},
		{errors.Join(apps.ErrUnauthenticated, errors.New("private token secret")), http.StatusUnauthorized, nil},
		{errors.Join(apps.ErrForbidden, errors.New("private token secret")), http.StatusForbidden, nil},
		{apps.ErrConflict, http.StatusConflict, nil},
		{errors.Join(storageConflict, errors.New("private token secret")), http.StatusConflict, []string{"Primary S3 storage is still in use", "Offsite backups can be disabled separately"}},
		{errors.Join(apps.ErrConflict, apps.ErrEnvironmentGroupManaged, errors.New("private token secret")), http.StatusConflict, []string{"managed by PostgreSQL", "follows rotating credentials"}},
		{errors.Join(postgresConflict, errors.New("private token secret")), http.StatusConflict, []string{"PostgreSQL database with this name already exists", "Choose a new name", "use apply"}},
		{errors.Join(apps.ErrConflict, errors.New("private token secret")), http.StatusConflict, nil},
		{apps.ErrInvalid, http.StatusBadRequest, nil},
		{errors.Join(postgresError, errors.New("private token secret")), http.StatusBadRequest, []string{"approved backup ID is required"}},
		{errors.Join(apps.ErrInvalid, errors.New("private token secret")), http.StatusBadRequest, nil},
		{errors.New("private token secret"), http.StatusInternalServerError, nil},
		{errors.Join(apps.ErrComponentsNotReady, apps.ErrConflict, errors.New("private token secret")), http.StatusConflict, []string{"cluster components", "cluster reconcile"}},
	} {
		is := is.New(t)
		writer := httptest.NewRecorder()
		respond[Organization](writer, http.StatusOK, nil, input.err)
		is.Equal(writer.Code, input.status)
		is.True(!strings.Contains(writer.Body.String(), "private token secret"))
		var problem Problem
		is.NoErr(json.Unmarshal(writer.Body.Bytes(), &problem))
		is.True(problem.Message != "")
		for _, hint := range input.guidance {
			is.True(strings.Contains(problem.Message, hint))
		}
	}
}

// TestBodylessOperationsRejectPayloads prevents unrecognized configuration from
// being silently ignored on organization creation and saved-plan application.
func TestBodylessOperationsRejectPayloads(t *testing.T) {
	is := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	is.NoErr(err)
	for _, input := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/organizations/team"},
		{http.MethodPost, "/api/v1/plans/plan-1/apply"},
		{http.MethodPost, "/api/v1/operations/op-1/cancel"},
		{http.MethodDelete, "/api/v1/organizations/team/spaces/dev/apps/web"},
	} {
		writer := httptest.NewRecorder()
		transport.ServeHTTP(writer, httptest.NewRequest(input.method, input.path, strings.NewReader(`{"unexpected":true}`)))
		is.Equal(writer.Code, http.StatusBadRequest)
	}
}

// TestPathAndRegistrationValidation prevents invalid target names or incomplete
// bindings from reaching the application manager.
func TestPathAndRegistrationValidation(t *testing.T) {
	is := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	is.NoErr(err)
	for _, input := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/operations/INVALID", ""},
		{http.MethodPost, "/api/v1/operations/INVALID/cancel", ""},
		{http.MethodPut, "/api/v1/organizations/INVALID", ""},
		{http.MethodPut, "/api/v1/organizations/team/clusters/dev", `{"inCluster":null}`},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev", `{}`},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev", `{"cluster":""}`},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev", `{"cluster":"dev","namespace":"custom"}`},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev", `{"cluster":"dev","certificateIssuer":"custom"}`},
	} {
		writer := httptest.NewRecorder()
		transport.ServeHTTP(writer, httptest.NewRequest(input.method, input.path, strings.NewReader(input.body)))
		is.Equal(writer.Code, http.StatusBadRequest)
	}
}

// TestApplicationApiLifecycle exercises generated clients against real SQLite
// state, including registration, immutable planning, apply, and rollout polling.
// An empty space keeps the Kubernetes fixture focused on the API lifecycle.
func TestApplicationApiLifecycle(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager, err := apps.NewManager(ctx, apps.NewInput{
		DatabasePath: filepath.Join(t.TempDir(), "apps.db"),
		PollInterval: time.Millisecond,
	})
	is.NoErr(err)
	defer func() { is.NoErr(manager.Close()) }()
	cluster := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/version":
			writeJSON(writer, http.StatusOK, map[string]any{"gitVersion": "v1.35.3"})
		case strings.HasPrefix(request.URL.Path, "/api/v1/namespaces/"):
			writeJSON(writer, http.StatusOK, map[string]any{
				"apiVersion": "v1", "kind": "Namespace",
				"metadata": map[string]any{"name": strings.TrimPrefix(request.URL.Path, "/api/v1/namespaces/"), "resourceVersion": "1", "labels": map[string]any{}},
			})
		default:
			writeJSON(writer, http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "reason": "NotFound", "code": 404})
		}
	}))
	defer cluster.Close()
	configurationPath := filepath.Join(t.TempDir(), "kubeconfig")
	is.NoErr(os.WriteFile(configurationPath, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: dev\n  cluster:\n    server: "+cluster.URL+"\ncontexts:\n- name: dev\n  context:\n    cluster: dev\n    user: test\ncurrent-context: dev\nusers:\n- name: test\n  user: {}\n"), 0o600))
	transport, err := NewHandler(manager)
	is.NoErr(err)
	server := httptest.NewServer(transport)
	defer server.Close()
	client, err := NewClientWithResponses(server.URL)
	is.NoErr(err)
	initialOrganizations, err := client.ListOrganizationsWithResponse(ctx)
	is.NoErr(err)
	is.Equal(initialOrganizations.StatusCode(), http.StatusOK)
	is.True(initialOrganizations.JSON200 != nil)
	is.True(*initialOrganizations.JSON200 != nil)
	is.Equal(len(*initialOrganizations.JSON200), 0)
	_, err = manager.CreateOrganization(ctx, "team")
	is.NoErr(err)
	organization, err := client.PutOrganizationWithResponse(ctx, "team", OrganizationOnboardingInput{})
	is.NoErr(err)
	is.Equal(organization.StatusCode(), http.StatusOK)
	is.Equal(organization.JSON200.Name, "team")
	createdAgain, err := client.PutOrganizationWithResponse(ctx, "team", OrganizationOnboardingInput{})
	is.NoErr(err)
	is.Equal(*createdAgain.JSON200, *organization.JSON200)
	defaultSpaces, err := client.ListSpacesWithResponse(ctx, "team")
	is.NoErr(err)
	is.Equal(defaultSpaces.StatusCode(), http.StatusOK)
	is.Equal(*defaultSpaces.JSON200, []Space{})
	initialApps, err := client.ListAppsWithResponse(ctx, "team", "default")
	is.NoErr(err)
	is.Equal(initialApps.StatusCode(), http.StatusNotFound)
	emptyClusters, err := client.ListClustersWithResponse(ctx, "team")
	is.NoErr(err)
	is.Equal(emptyClusters.StatusCode(), http.StatusOK)
	is.True(*emptyClusters.JSON200 != nil)
	is.Equal(len(*emptyClusters.JSON200), 0)
	registered, err := client.PutClusterWithResponse(ctx, "team", "dev", ClusterInput{KubeconfigPath: &configurationPath})
	is.NoErr(err)
	is.Equal(registered.StatusCode(), http.StatusOK)
	is.Equal(registered.JSON200.Name, "dev")
	initialSpace, err := client.GetSpaceWithResponse(ctx, "team", "dev")
	is.NoErr(err)
	is.Equal(initialSpace.StatusCode(), http.StatusNotFound)
	initialApps, err = client.ListAppsWithResponse(ctx, "team", "dev")
	is.NoErr(err)
	is.Equal(initialApps.StatusCode(), http.StatusNotFound)
	clusters, err := client.ListClustersWithResponse(ctx, "team")
	is.NoErr(err)
	is.Equal(clusters.StatusCode(), http.StatusOK)
	is.Equal(len(*clusters.JSON200), 1)
	is.Equal((*clusters.JSON200)[0].Name, "dev")
	_, err = manager.CreateOrganization(ctx, "another")
	is.NoErr(err)
	another, err := client.PutOrganizationWithResponse(ctx, "another", OrganizationOnboardingInput{})
	is.NoErr(err)
	otherClusters, err := client.ListClustersWithResponse(ctx, "another")
	is.NoErr(err)
	is.Equal(len(*otherClusters.JSON200), 0)
	organizations, err := client.ListOrganizationsWithResponse(ctx)
	is.NoErr(err)
	is.Equal(*organizations.JSON200, []Organization{*another.JSON200, *organization.JSON200})
	space, err := client.PutSpaceWithResponse(ctx, "team", "dev", SpaceInput{Cluster: "dev"})
	is.NoErr(err)
	is.Equal(space.StatusCode(), http.StatusOK)
	targetID := space.JSON200.ID
	is.True(space.JSON200.Configuration.Apps != nil)
	storedSpace, err := manager.GetSpace(ctx, "team", "dev")
	is.NoErr(err)
	is.Equal(space.JSON200.Namespace, storedSpace.Namespace)
	var spaceResponse map[string]any
	is.NoErr(json.Unmarshal(space.Body, &spaceResponse))
	_, issuerExposed := spaceResponse["certificateIssuer"]
	is.Equal(spaceResponse["namespace"], storedSpace.Namespace)
	is.True(!issuerExposed)
	spaces, err := client.ListSpacesWithResponse(ctx, "team")
	is.NoErr(err)
	is.Equal(len(*spaces.JSON200), 1)
	is.Equal((*spaces.JSON200)[0].Name, "dev")
	otherSpaces, err := client.ListSpacesWithResponse(ctx, "another")
	is.NoErr(err)
	is.Equal(*otherSpaces.JSON200, []Space{})
	boundAgain, err := client.PutSpaceWithResponse(ctx, "team", "dev", SpaceInput{Cluster: "dev"})
	is.NoErr(err)
	is.Equal(*boundAgain.JSON200, *space.JSON200)
	retrieved, err := client.GetSpaceWithResponse(ctx, "team", "dev")
	is.NoErr(err)
	is.Equal(retrieved.JSON200.ID, targetID)
	plan, err := client.PostPlanWithResponse(ctx, "team", "dev", Patch{Apps: &[]AppPatch{}})
	is.NoErr(err)
	is.Equal(plan.StatusCode(), http.StatusCreated)
	is.Equal(plan.JSON201.Space.ID, targetID)
	is.True(plan.JSON201.Configuration.Apps != nil)
	is.True(plan.JSON201.Manifests != nil)
	is.Equal(plan.JSON201.Values["certificateIssuer"], "borq")
	saved, err := client.GetPlanWithResponse(ctx, plan.JSON201.ID)
	is.NoErr(err)
	is.Equal(saved.JSON200.ID, plan.JSON201.ID)
	operation, err := client.ApplyPlanWithResponse(ctx, plan.JSON201.ID)
	is.NoErr(err)
	is.Equal(operation.StatusCode(), http.StatusAccepted)
	is.Equal(operation.JSON202.SpaceID, targetID)
	for {
		progress, err := client.GetOperationWithResponse(ctx, operation.JSON202.ID)
		is.NoErr(err)
		is.Equal(progress.StatusCode(), http.StatusOK)
		is.Equal(progress.JSON200.ID, operation.JSON202.ID)
		if progress.JSON200.Status == Succeeded {
			break
		}
		if progress.JSON200.Status == Failed {
			t.Fatalf("operation failed: %s", progress.JSON200.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	final, err := client.GetSpaceWithResponse(ctx, "team", "dev")
	is.NoErr(err)
	is.Equal(final.JSON200.Revision, 1)
	is.Equal(final.JSON200.LastSuccessfulRevision, 1)
	missing, err := client.GetSpaceWithResponse(ctx, "team", "missing")
	is.NoErr(err)
	is.Equal(missing.StatusCode(), http.StatusNotFound)
	missingApps, err := client.ListAppsWithResponse(ctx, "team", "missing")
	is.NoErr(err)
	is.Equal(missingApps.StatusCode(), http.StatusNotFound)
	missingOrganization, err := client.ListSpacesWithResponse(ctx, "missing")
	is.NoErr(err)
	is.Equal(missingOrganization.StatusCode(), http.StatusNotFound)
	missingClusters, err := client.ListClustersWithResponse(ctx, "missing")
	is.NoErr(err)
	is.Equal(missingClusters.StatusCode(), http.StatusNotFound)
	missingOperation, err := client.GetOperationWithResponse(ctx, "missing")
	is.NoErr(err)
	is.Equal(missingOperation.StatusCode(), http.StatusNotFound)
	is.True(missingOperation.JSON404 != nil)
	is.Equal(missingOperation.JSON404.Message, "requested resource was not found")
}

// TestRemovedProjectEnvironmentRoutes prevents clients from silently using the
// former hierarchy after spaces became direct children of organizations.
func TestRemovedProjectEnvironmentRoutes(t *testing.T) {
	assert := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	assert.NoErr(err)
	for _, input := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/organizations/team/projects/shop"},
		{http.MethodGet, "/api/v1/organizations/team/projects/shop/environments/dev"},
		{http.MethodPost, "/api/v1/organizations/team/projects/shop/environments/dev/plans"},
		{http.MethodGet, "/api/v1/organizations/team/projects/shop/environments/dev/environment-groups"},
	} {
		writer := httptest.NewRecorder()
		transport.ServeHTTP(writer, httptest.NewRequest(input.method, input.path, nil))
		assert.Equal(writer.Code, http.StatusNotFound)
	}
}
