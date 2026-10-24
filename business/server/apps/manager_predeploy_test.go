package apps

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestPredeployCheckpointSkipsFinishedJobs verifies every task blocks apps and
// recovery past the barrier does not rerun Jobs whose history has been removed.
func TestPredeployCheckpointSkipsFinishedJobs(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{applicationGate: gate})
	cluster.setReady(true)
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, 10*time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"predeploy":{"command":"migrate"}},{"name":"frontend","image":"frontend:v1","port":3000,"predeploy":{"command":"verify"}}]}`))
	assert.NoErr(err)
	jobs, applications := splitPlanManifests(plan.Manifests)
	assert.Equal(len(jobs), 2)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waiting := waitManagerOperation(t, manager, plan.ID, "waiting")
	assert.Equal(waiting.Phase, "predeploy")
	assert.Equal(len(waiting.Resources), 1)
	assert.True(!managerFixtureHasResource(cluster, applications[0]))
	assert.True(!managerFixtureHasResource(cluster, jobs[1]))
	finishManagerFixtureJob(cluster, jobs[0])
	waitManagerFixtureResource(t, cluster, jobs[1])
	assert.True(!managerFixtureHasResource(cluster, applications[0]))
	finishManagerFixtureJob(cluster, jobs[1])
	select {
	case <-cluster.applicationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("applications were not started after all predeploy tasks completed")
	}
	checkpoint, err := manager.GetOperation(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(checkpoint.Phase, "applications")
	assert.Equal(len(checkpoint.Resources), 2)
	assert.NoErr(manager.Close())
	cluster.mutex.Lock()
	for _, job := range jobs {
		delete(cluster.resources, managerResourcePath(job))
	}
	cluster.mutex.Unlock()
	release()
	reopened := openTestManager(t, path, 10*time.Second)
	succeeded := waitManagerOperation(t, reopened, plan.ID, "succeeded")
	assert.Equal(succeeded.Phase, "applications")
	for _, job := range jobs {
		assert.True(!managerFixtureHasResource(cluster, job))
	}
	desired, err := reopened.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(desired.LastSuccessfulRevision, 1)
}

// TestPredeployRolloutDeadlineStopsJob verifies the server's overall deadline
// suspends active tasks and leaves application resources untouched.
func TestPredeployRolloutDeadlineStopsJob(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"predeploy":{"command":"migrate","timeoutSeconds":300}}]}`))
	assert.NoErr(err)
	jobs, applications := splitPlanManifests(plan.Manifests)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	failed := waitManagerOperation(t, manager, plan.ID, "failed")
	assert.Equal(failed.Phase, "predeploy")
	cluster.mutex.Lock()
	suspended := cluster.resources[managerResourcePath(jobs[0])]["spec"].(map[string]any)["suspend"]
	cluster.mutex.Unlock()
	assert.Equal(suspended, true)
	for _, manifest := range applications {
		assert.True(!managerFixtureHasResource(cluster, manifest))
	}
}

// The fixture pointer protects state shared with Kubernetes HTTP handlers.
func managerFixtureHasResource(cluster *managerCluster, manifest map[string]any) bool {
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	_, exists := cluster.resources[managerResourcePath(manifest)]
	return exists
}

// The fixture pointer protects state shared with Kubernetes HTTP handlers.
func finishManagerFixtureJob(cluster *managerCluster, manifest map[string]any) {
	cluster.mutex.Lock()
	defer cluster.mutex.Unlock()
	cluster.resources[managerResourcePath(manifest)]["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "Complete", "status": "True"}},
	}
}

// The fixture pointer protects state shared with Kubernetes HTTP handlers.
func waitManagerFixtureResource(t *testing.T, cluster *managerCluster, manifest map[string]any) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !managerFixtureHasResource(cluster, manifest) {
		select {
		case <-deadline:
			t.Fatal("predeploy resource was not created")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
