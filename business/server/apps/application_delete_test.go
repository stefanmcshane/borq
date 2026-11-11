package apps

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestAppDeletionResumes owns deletion acceptance, retry identity, concurrency guards and durable removal after restart.
// Kubernetes garbage collection is covered by TestApplicationIntegration/deletion.
func TestAppDeletionResumes(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true, deleteGate: gate})
	path := filepath.Join(t.TempDir(), "apps.sqlite")
	manager := openTestManager(t, path, time.Minute)
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	for _, manifest := range plan.Manifests {
		live := cloneManagerResource(manifest)
		live["metadata"].(map[string]any)["uid"] = manifestName(manifest)
		live["metadata"].(map[string]any)["resourceVersion"] = "1"
		cluster.store(live)
	}
	stale, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","replicas":2}]}`))
	assert.NoErr(err)
	deletion, err := manager.DeleteApp(ctx, "acme", "production", "backend")
	assert.NoErr(err)
	assert.Equal(deletion.Phase, "deleting")
	assert.Equal(deletion.SpaceID, space.ID)
	again, err := manager.DeleteApp(ctx, "acme", "production", "backend")
	assert.NoErr(err)
	assert.Equal(again.ID, deletion.ID)
	_, err = manager.DeleteApp(ctx, "acme", "production", "frontend")
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.Apply(ctx, stale.ID)
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","replicas":3}]}`))
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.CancelOperation(ctx, deletion.ID)
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.RestartApp(ctx, "acme", "production", "frontend")
	assert.True(errors.Is(err, ErrConflict))
	accepted, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(len(accepted.Configuration.Apps), 2)
	manager.runtime.cancel()
	manager.runtime.workers.Wait()
	// A worker can reach its first plan read after shutdown cancels the runtime.
	manager.runOperation(manager.runtime.context, deletion)
	assert.NoErr(manager.Close())
	release()
	reopened := openTestManager(t, path, time.Minute)
	waitManagerOperation(t, reopened, deletion.ID, "succeeded")
	assert.NoErr(reopened.Close())
	persisted := openTestManager(t, path, time.Minute)
	apps, err := persisted.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(len(apps.Configuration.Apps), 1)
	assert.Equal(apps.Configuration.Apps[0].Name, "frontend")
	history, err := persisted.ListAppDeployments(ctx, "acme", "production", "frontend")
	assert.NoErr(err)
	assert.Equal(len(history), 1)
	assert.Equal(history[0].Operation.ID, plan.ID)
	_, err = persisted.GetApp(ctx, "acme", "production", "backend")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = persisted.Apply(ctx, stale.ID)
	assert.True(errors.Is(err, ErrConflict))
	_, err = persisted.DeleteApp(ctx, "acme", "production", "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = persisted.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"frontend","replicas":2}]}`))
	assert.NoErr(err)
}

// TestAppDeletionFailureRetainsConfiguration ensures failed cleanup leaves a visible application that can be deleted again.
func TestAppDeletionFailureRetainsConfiguration(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "apps.sqlite"), time.Minute)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	foreign := cloneManagerResource(plan.Manifests[0])
	foreign["metadata"].(map[string]any)["labels"].(map[string]any)["borq.run/target-id"] = "another-space"
	cluster.store(foreign)
	name := manifestText(foreign, "metadata", "labels", "app.kubernetes.io/name")
	deletion, err := manager.DeleteApp(ctx, "acme", "production", name)
	assert.NoErr(err)
	waitManagerOperation(t, manager, deletion.ID, "failed")
	accepted, err := manager.ListApps(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(len(accepted.Configuration.Apps), 2)
	cluster.mutex.Lock()
	delete(cluster.resources, managerResourcePath(foreign))
	cluster.mutex.Unlock()
	retry, err := manager.DeleteApp(ctx, "acme", "production", name)
	assert.NoErr(err)
	assert.True(retry.ID != deletion.ID)
	waitManagerOperation(t, manager, retry.ID, "succeeded")
}
