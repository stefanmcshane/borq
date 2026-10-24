package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagerFailedApplyRetainsDesired verifies planning is inert, failed
// accepted changes remain desired, retries are idempotent, and all artifacts
// survive a database reopen independently of the last successful revision.
func TestManagerFailedApplyRetainsDesired(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Second)
	environment := createManagerEnvironment(t, manager, cluster, "production")
	empty, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(empty, ApplicationState{Configuration: Configuration{Apps: []App{}}, Manifests: []map[string]any{}})
	input := managerPlanInput(t, "production", managerInitialApps())
	plan, err := manager.Plan(ctx, input)
	assert.NoErr(err)
	otherPlan, err := manager.Plan(ctx, input)
	assert.NoErr(err)
	assert.True(otherPlan.ID != plan.ID)
	assert.Equal(plan.BaseRevision, 0)
	assert.Equal(len(plan.Configuration.Apps), 2)
	assert.Equal(len(plan.Manifests), 4)
	assert.Equal(len(plan.ResourceDiffs), 4)
	assert.True(strings.Contains(plan.ConfigurationDiff, "backend:v1"))
	for _, difference := range plan.ResourceDiffs {
		assert.Equal(difference.Action, "create")
	}
	beforeApply, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(beforeApply, environment)
	unapplied, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(unapplied, empty)
	assert.Equal(cluster.namespaceAttempts(), 0)

	accepted, err := manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(accepted.Status, "pending")
	failed := waitManagerOperation(t, manager, plan.ID, "failed")
	assert.True(strings.Contains(failed.Message, "forbidden"))
	desired, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(desired.Revision, 1)
	assert.Equal(desired.LastSuccessfulRevision, 0)
	assert.Equal(desired.Configuration, plan.Configuration)
	listed, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(managerJson(t, listed), managerJson(t, ApplicationState{Configuration: plan.Configuration, Manifests: plan.Manifests}))
	retried, err := manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(retried.ID, accepted.ID)
	assert.Equal(retried.Status, "failed")
	assert.Equal(cluster.namespaceAttempts(), 1)
	savedOther, err := manager.GetPlan(ctx, otherPlan.ID)
	assert.NoErr(err)
	assert.Equal(savedOther.ID, otherPlan.ID)

	future, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v2"}]}`))
	assert.NoErr(err)
	assert.Equal(future.BaseRevision, 1)
	assert.Equal(len(future.Configuration.Apps), 2)
	assert.Equal(future.Configuration.Apps[0].Image, "backend:v2")
	assert.Equal(future.Configuration.Apps[0].Env, EnvReference{Name: "acme-production-backend", Version: "0"})
	assert.Equal(future.Configuration.Apps[1].Name, "frontend")
	assert.Equal(future.Configuration.Apps[1].Image, "frontend:v1")
	assert.Equal(len(future.Manifests), 4)
	_, err = manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[]}`))
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "deletion is deferred"))
	stillAccepted, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(managerJson(t, stillAccepted), managerJson(t, listed))
	cluster.server.Close()
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Second)
	persisted, err := reopened.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(persisted, desired)
	persistedApps, err := reopened.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(managerJson(t, persistedApps), managerJson(t, listed))
	for _, target := range [][2]string{{"other", "production"}, {"acme", "missing"}} {
		_, err := reopened.ListApps(ctx, target[0], target[1])
		assert.True(errors.Is(err, ErrNotFound))
	}
	persistedPlan, err := reopened.GetPlan(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(managerJson(t, persistedPlan), managerJson(t, plan))
	persistedFuture, err := reopened.GetPlan(ctx, future.ID)
	assert.NoErr(err)
	assert.Equal(managerJson(t, persistedFuture), managerJson(t, future))
	persistedOperation, err := reopened.GetOperation(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(persistedOperation, failed)
	_, err = reopened.Apply(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(cluster.namespaceAttempts(), 1)
}

// TestManagerConcurrentApply verifies retries share the original operation, stale
// plans cannot discard accepted changes, and a fresh plan interrupts a blocked apply.
func TestManagerConcurrentApply(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true, namespaceGate: gate})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	competitor, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	accepted, err := manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerNamespace(t, cluster)

	type result struct {
		operation Operation
		err       error
	}
	results := make(chan result, 8)
	for range 8 {
		go func() {
			operation, applyErr := manager.Apply(ctx, plan.ID)
			results <- result{operation: operation, err: applyErr}
		}()
	}
	for range 8 {
		retry := <-results
		assert.NoErr(retry.err)
		assert.Equal(retry.operation.ID, accepted.ID)
	}
	_, err = manager.Apply(ctx, competitor.ID)
	assert.True(errors.Is(err, ErrConflict))
	competitor, err = manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","replicas":2}]}`))
	assert.NoErr(err)
	queued, err := manager.Apply(ctx, competitor.ID)
	assert.NoErr(err)
	assert.Equal(queued.Status, "queued")
	desired, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(desired.Revision, 2)
	waitManagerOperation(t, manager, plan.ID, "superseded")
	waitManagerNamespace(t, cluster)
	assert.Equal(cluster.namespaceAttempts(), 2)
	release()
	waitManagerOperation(t, manager, competitor.ID, "failed")
}

// TestManagerResumesInterruptedApply verifies graceful shutdown leaves accepted
// work resumable and reopening does not accept the desired revision a second time.
func TestManagerResumesInterruptedApply(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true, namespaceGate: gate})
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerNamespace(t, cluster)
	beforeClose, err := manager.GetOperation(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(beforeClose.Status, "applying")
	assert.NoErr(manager.Close())
	_, err = manager.Apply(ctx, plan.ID)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "stopping"))
	release()
	reopened := openTestManager(t, path, time.Second)
	waitManagerOperation(t, reopened, plan.ID, "failed")
	assert.Equal(cluster.namespaceAttempts(), 2)
	desired, err := reopened.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(desired.Revision, 1)
	assert.Equal(desired.Configuration, plan.Configuration)
	assert.Equal(desired.LastSuccessfulRevision, 0)
}

// TestManagerRolloutDeadline verifies a deployment that never becomes ready
// records failure without discarding the configuration accepted for that rollout.
func TestManagerRolloutDeadline(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), 150*time.Millisecond)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waiting := waitManagerOperation(t, manager, plan.ID, "waiting")
	assert.Equal(len(waiting.Resources), 4)
	beforeReady, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(beforeReady.Revision, 1)
	assert.Equal(beforeReady.LastSuccessfulRevision, 0)
	failed := waitManagerOperation(t, manager, plan.ID, "failed")
	assert.True(strings.Contains(failed.Message, "deadline exceeded"))
	desired, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(desired.Configuration, plan.Configuration)
	assert.Equal(desired.Revision, 1)
	assert.Equal(desired.LastSuccessfulRevision, 0)
}

// TestManagerRegistrationAndValidation verifies organization-scoped identity,
// idempotent registration, exclusive namespace bindings, and invalid or missing
// targets fail before accepting deployment state.
func TestManagerRegistrationAndValidation(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	project, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	duplicate, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(duplicate, project)
	_, err = manager.CreateOrganization(ctx, "another-org")
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, "Invalid")
	assert.True(errors.Is(err, ErrInvalid))
	registered, err := manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	_, err = manager.RegisterCluster(ctx, registered)
	assert.NoErr(err)
	changed := registered
	changed.Context = "alternate"
	_, err = manager.RegisterCluster(ctx, changed)
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "missing", KubeconfigPath: "not-present"})
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "missing", Name: "production", Cluster: "local"})
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "production", Cluster: "missing"})
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "production", Cluster: "local"})
	assert.NoErr(err)
	_, err = manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"new-app"}]}`))
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.Plan(ctx, managerPlanInput(t, "missing", managerInitialApps()))
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.GetPlan(ctx, "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.Apply(ctx, "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.GetOperation(ctx, "missing")
	assert.True(errors.Is(err, ErrNotFound))
	assert.Equal(cluster.namespaceAttempts(), 0)
}

// TestManagerRejectsUnownedResources verifies planning cannot adopt an existing
// workload even when its Kubernetes name matches a requested application.
func TestManagerRejectsUnownedResources(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	environment := createManagerEnvironment(t, manager, cluster, "production")
	configuration, err := Merge(Configuration{}, patchFromJson(t, managerInitialApps()))
	assert.NoErr(err)
	rendered, err := Render(configuration, environment.Namespace, "another-target")
	assert.NoErr(err)
	cluster.store(findManifest(t, rendered, "Deployment", "backend"))
	_, err = manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "refuse to adopt"))
	unchanged, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(unchanged, environment)
	assert.Equal(cluster.namespaceAttempts(), 0)
}

// TestManagerRejectsCorruptOperationRecords verifies recovery fails visibly if
// an on-disk operation cannot be decoded instead of silently forgetting work.
func TestManagerRejectsCorruptOperationRecords(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, path)
	assert.NoErr(err)
	assert.NoErr(database.Commit(ctx, []sqlite.Write{{Collection: "operation", Key: "corrupt", Value: json.RawMessage(`{"status":42}`)}}))
	assert.NoErr(database.Close())
	_, err = NewManager(ctx, NewInput{DatabasePath: path})
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "read unfinished operation"))
	_, err = NewManager(ctx, NewInput{})
	assert.True(err != nil)
}

func openTestManager(t *testing.T, path string, timeout time.Duration) Manager {
	t.Helper()
	manager, err := NewManager(context.Background(), NewInput{DatabasePath: path, RolloutTimeout: timeout, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func createManagerEnvironment(t *testing.T, manager Manager, cluster *managerCluster, name string) Space {
	t.Helper()
	ctx := context.Background()
	if _, err := manager.CreateOrganization(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: cluster.kubeconfig}); err != nil {
		t.Fatal(err)
	}
	environment, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: name, Cluster: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func managerPlanInput(t *testing.T, environment, document string) PlanInput {
	t.Helper()
	return PlanInput{Organization: "acme", Space: environment, Patch: patchFromJson(t, document)}
}

func managerInitialApps() string {
	return `{"apps":[{"name":"frontend","image":"frontend:v1","port":3000},{"name":"backend","image":"backend:v1","port":8080}]}`
}

func waitManagerOperation(t *testing.T, manager Manager, id, state string) Operation {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	last := Operation{}
	for {
		operation, err := manager.GetOperation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		last = operation
		if operation.Status == state {
			return operation
		}
		if operationFinished(operation.Status) {
			t.Fatalf("operation finished %s while waiting for %s: %s", operation.Status, state, operation.Message)
		}
		select {
		case <-timer.C:
			t.Fatalf("operation remained %s while waiting for %s: %s", last.Status, state, last.Message)
		case <-ticker.C:
		}
	}
}

func waitManagerNamespace(t *testing.T, cluster *managerCluster) {
	t.Helper()
	select {
	case <-cluster.namespaceStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach namespace creation")
	}
}

func managerJson(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

type managerClusterOptions struct {
	forbidNamespace bool
	namespaceGate   <-chan struct{}
	applicationGate <-chan struct{}
	jobStopGate     <-chan struct{}
}

// The fixture is shared by concurrent HTTP handlers, so pointer ownership keeps
// its mutex, resource map, and namespace-attempt notifications synchronized.
type managerCluster struct {
	server             *httptest.Server
	kubeconfig         string
	mutex              sync.Mutex
	resources          map[string]map[string]any
	ready              bool
	namespaceCreates   int
	namespaceStarted   chan struct{}
	applicationStarted chan struct{}
	options            managerClusterOptions
}

func newManagerCluster(t *testing.T, options managerClusterOptions) *managerCluster {
	t.Helper()
	cluster := &managerCluster{resources: map[string]map[string]any{}, namespaceStarted: make(chan struct{}, 16), applicationStarted: make(chan struct{}, 16), options: options}
	cluster.server = httptest.NewServer(http.HandlerFunc(cluster.serve))
	t.Cleanup(cluster.server.Close)
	cluster.kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\n- name: alternate\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", cluster.server.URL)
	if err := os.WriteFile(cluster.kubeconfig, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func (cluster *managerCluster) serve(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	if discovery, exists := managerDiscovery(request.URL.Path); exists {
		_ = json.NewEncoder(writer).Encode(discovery)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces" {
		cluster.createNamespace(writer, request)
		return
	}
	if cluster.options.applicationGate != nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/deployments") && request.URL.Query().Get("dryRun") == "" {
		select {
		case cluster.applicationStarted <- struct{}{}:
		default:
		}
		select {
		case <-request.Context().Done():
			return
		case <-cluster.options.applicationGate:
		}
	}
	if cluster.options.jobStopGate != nil && request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/jobs/") && strings.HasPrefix(request.Header.Get("Content-Type"), "application/merge-patch+json") {
		select {
		case <-request.Context().Done():
			return
		case <-cluster.options.jobStopGate:
		}
	}
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/replicasets") {
		_ = json.NewEncoder(writer).Encode(cluster.replicaSets(request.URL.Path))
		return
	}
	resource, exists := cluster.resources[request.URL.Path]
	if request.Method == http.MethodGet {
		if !exists {
			managerStatus(writer, http.StatusNotFound, "NotFound", "resource not found")
			return
		}
		_ = json.NewEncoder(writer).Encode(cluster.observed(resource))
		return
	}
	if request.Method != http.MethodPost && request.Method != http.MethodPatch {
		managerStatus(writer, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported test request")
		return
	}
	var desired map[string]any
	if strings.HasPrefix(request.Header.Get("Content-Type"), "application/json-patch+json") {
		var operations json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&operations); err != nil {
			managerStatus(writer, 400, "BadRequest", err.Error())
			return
		}
		patch, err := jsonpatch.DecodePatch(operations)
		if err != nil {
			managerStatus(writer, 400, "BadRequest", err.Error())
			return
		}
		encoded, _ := json.Marshal(resource)
		updated, err := patch.Apply(encoded)
		if err != nil {
			managerStatus(writer, 400, "BadRequest", err.Error())
			return
		}
		_ = json.Unmarshal(updated, &desired)
	} else if err := json.NewDecoder(request.Body).Decode(&desired); err != nil {
		managerStatus(writer, 400, "BadRequest", err.Error())
		return
	}
	metadata := desired["metadata"].(map[string]any)
	metadata["resourceVersion"], metadata["generation"], metadata["uid"] = "1", float64(1), metadata["name"]
	if desired["kind"] == "Service" {
		desired["spec"].(map[string]any)["clusterIP"] = "10.0.0.10"
	}
	if request.URL.Query().Get("dryRun") == "" {
		key := request.URL.Path
		if request.Method == http.MethodPost {
			key += "/" + metadata["name"].(string)
		}
		cluster.resources[key] = desired
	}
	_ = json.NewEncoder(writer).Encode(cluster.observed(desired))
}

func (cluster *managerCluster) createNamespace(writer http.ResponseWriter, request *http.Request) {
	cluster.mutex.Lock()
	cluster.namespaceCreates++
	cluster.mutex.Unlock()
	select {
	case cluster.namespaceStarted <- struct{}{}:
	default:
	}
	if cluster.options.namespaceGate != nil {
		select {
		case <-request.Context().Done():
			return
		case <-cluster.options.namespaceGate:
		}
	}
	if cluster.options.forbidNamespace {
		managerStatus(writer, http.StatusForbidden, "Forbidden", "namespace creation is forbidden by test policy")
		return
	}
	var namespace map[string]any
	if err := json.NewDecoder(request.Body).Decode(&namespace); err != nil {
		managerStatus(writer, 400, "BadRequest", err.Error())
		return
	}
	cluster.store(namespace)
	_ = json.NewEncoder(writer).Encode(namespace)
}

func (cluster *managerCluster) observed(resource map[string]any) map[string]any {
	result := cloneManagerResource(resource)
	if result["kind"] != "Deployment" {
		return result
	}
	replicas := result["spec"].(map[string]any)["replicas"]
	result["status"] = map[string]any{"observedGeneration": float64(1), "replicas": replicas, "updatedReplicas": replicas}
	if cluster.ready {
		result["status"].(map[string]any)["availableReplicas"] = replicas
		result["status"].(map[string]any)["readyReplicas"] = replicas
	}
	return result
}

func (cluster *managerCluster) replicaSets(path string) map[string]any {
	namespacePrefix := strings.TrimSuffix(path, "/replicasets") + "/deployments/"
	items := []any{}
	for key, resource := range cluster.resources {
		if !strings.HasPrefix(key, namespacePrefix) {
			continue
		}
		metadata := resource["metadata"].(map[string]any)
		spec := resource["spec"].(map[string]any)
		items = append(items, map[string]any{
			"apiVersion": "apps/v1", "kind": "ReplicaSet",
			"metadata": map[string]any{"name": metadata["name"].(string) + "-new", "ownerReferences": []any{map[string]any{"kind": "Deployment", "name": metadata["name"], "uid": metadata["uid"], "controller": true}}},
			"spec":     map[string]any{"replicas": spec["replicas"], "template": spec["template"]},
			"status":   map[string]any{"readyReplicas": spec["replicas"], "availableReplicas": spec["replicas"]},
		})
	}
	return map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSetList", "items": items}
}

func (cluster *managerCluster) store(resource map[string]any) {
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	cluster.resources[managerResourcePath(resource)] = cloneManagerResource(resource)
}

func (cluster *managerCluster) namespaceAttempts() int {
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	return cluster.namespaceCreates
}

func (cluster *managerCluster) setReady(ready bool) {
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	cluster.ready = ready
}

func managerResourcePath(resource map[string]any) string {
	metadata := resource["metadata"].(map[string]any)
	if resource["kind"] == "Namespace" {
		return "/api/v1/namespaces/" + metadata["name"].(string)
	}
	base := "/api/v1"
	if resource["apiVersion"] != "v1" {
		base = "/apis/" + resource["apiVersion"].(string)
	}
	return base + "/namespaces/" + metadata["namespace"].(string) + "/" + strings.ToLower(resource["kind"].(string)) + "s/" + metadata["name"].(string)
}

func cloneManagerResource(resource map[string]any) map[string]any {
	encoded, _ := json.Marshal(resource)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

func managerStatus(writer http.ResponseWriter, code int, reason, message string) {
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "message": message, "code": code})
}

func managerDiscovery(path string) (map[string]any, bool) {
	groups := []any{
		map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}},
		map[string]any{"name": "batch", "versions": []any{map[string]any{"groupVersion": "batch/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "batch/v1", "version": "v1"}},
		map[string]any{"name": "networking.k8s.io", "versions": []any{map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"}},
		map[string]any{"name": "external-secrets.io", "versions": []any{map[string]any{"groupVersion": "external-secrets.io/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "external-secrets.io/v1", "version": "v1"}},
	}
	switch path {
	case "/version":
		return map[string]any{"major": "1", "minor": "35", "gitVersion": "v1.35.0"}, true
	case "/api":
		return map[string]any{"kind": "APIVersions", "apiVersion": "v1", "versions": []string{"v1"}}, true
	case "/apis":
		return map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups}, true
	case "/api/v1":
		return managerResourceList("v1", []string{"Namespace", "Service", "Secret"}), true
	case "/apis/apps/v1":
		return managerResourceList("apps/v1", []string{"Deployment", "ReplicaSet"}), true
	case "/apis/batch/v1":
		return managerResourceList("batch/v1", []string{"Job"}), true
	case "/apis/networking.k8s.io/v1":
		return managerResourceList("networking.k8s.io/v1", []string{"Ingress"}), true
	case "/apis/external-secrets.io/v1":
		return managerResourceList("external-secrets.io/v1", []string{"ExternalSecret"}), true
	default:
		return nil, false
	}
}

func managerResourceList(version string, kinds []string) map[string]any {
	resources := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		resources = append(resources, map[string]any{"name": strings.ToLower(kind) + "s", "singularName": strings.ToLower(kind), "namespaced": kind != "Namespace", "kind": kind, "verbs": []string{"get", "list", "create", "patch"}})
	}
	return map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": version, "resources": resources}
}
