package apps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagerQueuedTimeoutStartsAtExecution simulates a long queue residence
// without consuming the rollout deadline before the operation gets its turn,
// and records every application resource's final successful state.
func TestManagerQueuedTimeoutStartsAtExecution(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{namespaceGate: gate})
	cluster.setReady(true)
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	first, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	second, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, first.ID)
	assert.NoErr(err)
	waitManagerNamespace(t, cluster)
	queued, err := manager.Apply(ctx, second.ID)
	assert.NoErr(err)
	assert.Equal(queued.Status, "queued")
	queued, record, err := loadDocument[Operation](ctx, manager.database, "operation", second.ID)
	assert.NoErr(err)
	queued.StartedAt = time.Now().Add(-time.Hour)
	write, err := documentWrite("operation", queued.ID, queued, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	state, _, err := loadDocument[operationState](ctx, manager.database, "operation-state", second.ID)
	assert.NoErr(err)
	assert.True(state.ExecutionStartedAt.IsZero())
	release()
	waitManagerOperation(t, manager, first.ID, "succeeded")
	finished := waitManagerOperation(t, manager, second.ID, "succeeded")
	assert.Equal(len(finished.Resources), 4)
	for _, resource := range finished.Resources {
		assert.Equal(resource.State, "succeeded")
	}
	assert.True(finished.StartedAt.Before(time.Now().Add(-30 * time.Minute)))
	state, _, err = loadDocument[operationState](ctx, manager.database, "operation-state", second.ID)
	assert.NoErr(err)
	assert.True(state.ExecutionStartedAt.After(queued.StartedAt.Add(30 * time.Minute)))
	assert.Equal(manager.operationDeadline(finished), state.ExecutionStartedAt.Add(time.Second))
}

// TestManagerRecoversQueuedHeadWithEmptyActiveSlot covers shutdown between
// completing a predecessor and starting the durable queue's next operation.
func TestManagerRecoversQueuedHeadWithEmptyActiveSlot(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	cluster.setReady(true)
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Second)
	environment := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	key := spaceKey("acme", "production")
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	assert.NoErr(err)
	stored.Queue = []string{plan.ID}
	stored.Space.Revision = 1
	stored.Space.Configuration = plan.Configuration
	stored.Manifests = plan.Manifests
	queued := Operation{ID: plan.ID, PlanID: plan.ID, SpaceID: environment.ID, Status: "queued", Phase: "applications", StartedAt: time.Now().Add(-time.Hour)}
	writes := []sqlite.Write{}
	for _, document := range []struct {
		collection, key string
		value           any
		revision        int64
	}{
		{"space", key, stored, record.Revision},
		{"operation", plan.ID, queued, 0},
		{"operation-state", plan.ID, operationState{Revision: 1}, 0},
	} {
		write, err := documentWrite(document.collection, document.key, document.value, document.revision)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	assert.NoErr(manager.database.Commit(ctx, writes))
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Second)
	waitManagerOperation(t, reopened, plan.ID, "succeeded")
	stored, _, err = loadDocument[spaceRecord](ctx, reopened.database, "space", key)
	assert.NoErr(err)
	assert.Equal(stored.ActiveOperation, "")
	assert.Equal(len(stored.Queue), 0)
	assert.Equal(stored.Space.LastSuccessfulRevision, 1)
	state, _, err := loadDocument[operationState](ctx, reopened.database, "operation-state", plan.ID)
	assert.NoErr(err)
	assert.True(state.ExecutionStartedAt.After(queued.StartedAt.Add(30 * time.Minute)))
}

// TestManagerFinalizationRetriesUnreadablePlan repairs a transiently unreadable
// plan record and verifies its blocked queue continues without a server restart.
func TestManagerFinalizationRetriesUnreadablePlan(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	cluster.setReady(true)
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	environment := createManagerEnvironment(t, manager, cluster, "production")
	first, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	second, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	key := spaceKey("acme", "production")
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	assert.NoErr(err)
	stored.Space.Revision = 2
	stored.Space.Configuration = second.Configuration
	stored.Manifests = second.Manifests
	stored.ActiveOperation = first.ID
	stored.Queue = []string{second.ID}
	operation := Operation{ID: first.ID, PlanID: first.ID, SpaceID: environment.ID, Status: "waiting", Phase: "applications", StartedAt: time.Now()}
	queued := Operation{ID: second.ID, PlanID: second.ID, SpaceID: environment.ID, Status: "queued", Phase: "applications", StartedAt: time.Now()}
	writes := []sqlite.Write{}
	for _, document := range []struct {
		collection, key string
		value           any
		revision        int64
	}{
		{"space", key, stored, record.Revision},
		{"operation", first.ID, operation, 0},
		{"operation", second.ID, queued, 0},
		{"operation-state", first.ID, operationState{Revision: 1, ExecutionStartedAt: time.Now()}, 0},
		{"operation-state", second.ID, operationState{Revision: 2}, 0},
	} {
		write, err := documentWrite(document.collection, document.key, document.value, document.revision)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	original, err := manager.database.Get(ctx, "plan", first.ID)
	assert.NoErr(err)
	writes = append(writes, sqlite.Write{Collection: "plan", Key: first.ID, Value: json.RawMessage(`{"plan":42}`), ExpectedRevision: original.Revision})
	assert.NoErr(manager.database.Commit(ctx, writes))
	assert.True(manager.completeQueuedOperation(operation, "succeeded", "resources ready") != nil)
	finalized := make(chan struct{})
	go func() { manager.finishOperation(operation, "succeeded", "resources ready"); close(finalized) }()
	select {
	case <-finalized:
		t.Fatal("finalization abandoned the active slot after a transient record failure")
	case <-time.After(25 * time.Millisecond):
	}
	stored, _, err = loadDocument[spaceRecord](ctx, manager.database, "space", key)
	assert.NoErr(err)
	assert.Equal(stored.ActiveOperation, first.ID)
	assert.Equal(stored.Queue, []string{second.ID})
	corrupt, err := manager.database.Get(ctx, "plan", first.ID)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{{Collection: "plan", Key: first.ID, Value: original.Value, ExpectedRevision: corrupt.Revision}}))
	select {
	case <-finalized:
	case <-time.After(3 * time.Second):
		t.Fatal("finalization did not recover after the record was repaired")
	}
	waitManagerOperation(t, manager, second.ID, "succeeded")
	stored, _, err = loadDocument[spaceRecord](ctx, manager.database, "space", key)
	assert.NoErr(err)
	assert.Equal(stored.ActiveOperation, "")
	assert.Equal(len(stored.Queue), 0)
	assert.Equal(stored.Space.LastSuccessfulRevision, 2)
}
