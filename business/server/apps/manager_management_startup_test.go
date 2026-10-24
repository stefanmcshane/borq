package apps

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagerDefersDurableQueueUntilManagementReady prevents accepted applies
// from failing against unavailable vaults or consuming queue execution time
// while the server installs its own management components after a restart.
func TestManagerDefersDurableQueueUntilManagementReady(t *testing.T) {
	for _, initial := range []string{"queued", "waiting"} {
		t.Run(initial, func(t *testing.T) { checkManagementDeferredApply(t, initial) })
	}
}

func checkManagementDeferredApply(t *testing.T, initial string) {
	t.Helper()
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	cluster.setReady(true)
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Minute)
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	key := spaceKey("acme", "production")
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	assert.NoErr(err)
	stateBefore := operationState{Revision: 1}
	if initial == "queued" {
		stored.Queue = []string{plan.ID}
	} else {
		stored.ActiveOperation = plan.ID
		stateBefore.ExecutionStartedAt = time.Now().UTC()
	}
	stored.Space.Revision = 1
	stored.Space.Configuration = plan.Configuration
	stored.Manifests = plan.Manifests
	queued := Operation{ID: plan.ID, PlanID: plan.ID, SpaceID: space.ID, Status: initial, Phase: "applications", StartedAt: time.Now().Add(-time.Hour)}
	writes := []sqlite.Write{}
	for _, document := range []struct {
		collection, key string
		value           any
		revision        int64
	}{
		{"space", key, stored, record.Revision},
		{"operation", plan.ID, queued, 0},
		{"operation-state", plan.ID, stateBefore, 0},
	} {
		write, err := documentWrite(document.collection, document.key, document.value, document.revision)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	assert.NoErr(manager.database.Commit(ctx, writes))
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, NewInput{
		DatabasePath: path, RolloutTimeout: time.Minute, PollInterval: 5 * time.Millisecond,
		InfrastructureEnabled: true, ManagementEnabled: true, ManagementKubeconfigPath: cluster.kubeconfig,
		PlatformSettings: platform.Settings{OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"},
	})
	assert.NoErr(err)
	t.Cleanup(func() { _ = reopened.Close() })
	before, err := reopened.GetOperation(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(before.Status, initial)
	state, _, err := loadDocument[operationState](ctx, reopened.database, "operation-state", plan.ID)
	assert.NoErr(err)
	assert.Equal(state.ExecutionStartedAt, stateBefore.ExecutionStartedAt)
	select {
	case <-cluster.namespaceStarted:
		t.Fatal("application deployment started before management readiness")
	case <-time.After(20 * time.Millisecond):
	}
	assert.NoErr(reopened.resumeOperationWorkers(ctx))
	waitManagerOperation(t, reopened, plan.ID, "succeeded")
	state, _, err = loadDocument[operationState](ctx, reopened.database, "operation-state", plan.ID)
	assert.NoErr(err)
	assert.True(!state.ExecutionStartedAt.IsZero())
	assert.NoErr(reopened.resumeOperationWorkers(ctx))
}
