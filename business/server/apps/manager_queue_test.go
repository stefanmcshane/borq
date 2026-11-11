package apps

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagerSupersedesStackedChanges owns coalescing, inherited changes and barriers,
// explicit cancellation of queued and running work, and terminal-state persistence.
func TestManagerSupersedesStackedChanges(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	cluster := newManagerCluster(t, managerClusterOptions{jobStopGate: gate})
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Minute)
	createManagerEnvironment(t, manager, cluster, "production")
	first, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v2","port":8080,"predeploy":{"command":"migrate"}},{"name":"frontend","image":"frontend:v1","port":3000}]}`))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, first.ID)
	assert.NoErr(err)
	waiting := waitManagerOperation(t, manager, first.ID, "waiting")
	assert.Equal(waiting.Phase, "predeploy")
	second, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"frontend","replicas":2}]}`))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, second.ID)
	assert.NoErr(err)
	third, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"frontend","command":"serve"}]}`))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, third.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, second.ID, "superseded")
	assert.Equal(third.Configuration.Apps[0].Image, "backend:v2")
	assert.Equal(third.Configuration.Apps[1].Replicas, 2)
	assert.Equal(third.Configuration.Apps[1].Command, "serve")
	jobs, _ := splitPlanManifests(third.Manifests)
	assert.Equal(len(jobs), 1) // Backend's unfinished barrier survives frontend-only patches.
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	assert.Equal(stored.Queue, []string{third.ID})
	canceled, err := manager.CancelOperation(ctx, third.ID)
	assert.NoErr(err)
	assert.Equal(canceled.Status, "canceled")
	fourth, err := manager.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"frontend","replicas":3}]}`))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, fourth.ID)
	assert.NoErr(err)
	release()
	waitManagerOperation(t, manager, first.ID, "superseded")
	waitManagerOperation(t, manager, fourth.ID, "waiting")
	_, err = manager.CancelOperation(ctx, fourth.ID)
	assert.NoErr(err)
	finished := waitManagerOperation(t, manager, fourth.ID, "canceled")
	repeated, err := manager.CancelOperation(ctx, fourth.ID)
	assert.NoErr(err)
	assert.Equal(repeated, finished)
	_, err = manager.CancelOperation(ctx, "missing")
	assert.True(errors.Is(err, ErrNotFound))
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Minute)
	for id, status := range map[string]string{first.ID: "superseded", second.ID: "superseded", third.ID: "canceled", fourth.ID: "canceled"} {
		operation, err := reopened.GetOperation(ctx, id)
		assert.NoErr(err)
		assert.Equal(operation.Status, status)
	}
	stored, _, err = loadDocument[spaceRecord](ctx, reopened.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	assert.Equal(stored.ActiveOperation, "")
	assert.Equal(len(stored.Queue), 0)
	assert.Equal(stored.Space.Revision, 4)
	assert.Equal(stored.Space.LastSuccessfulRevision, 0)
	assert.Equal(stored.Space.Configuration, fourth.Configuration)
	retry, err := reopened.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"frontend","replicas":4}]}`))
	assert.NoErr(err)
	jobs, _ = splitPlanManifests(retry.Manifests)
	assert.Equal(len(jobs), 1) // Terminal cancellation and a restart must not discard the backend barrier.
	cleared, err := reopened.Plan(ctx, managerPlanInput(t, "production", `{"apps":[{"name":"backend","predeploy":{"command":""}}]}`))
	assert.NoErr(err)
	jobs, _ = splitPlanManifests(cleared.Manifests)
	assert.Equal(len(jobs), 0)
}

// TestManagerRecoversLatestQueuedApply covers old FIFO backlogs, interrupted
// cancellation, empty active slots, and rollout deadlines starting at execution.
func TestManagerRecoversLatestQueuedApply(t *testing.T) {
	for _, activeStatus := range []string{"", "waiting", "canceling"} {
		t.Run("active="+activeStatus, func(t *testing.T) {
			assert := is.New(t)
			ctx := context.Background()
			cluster := newManagerCluster(t, managerClusterOptions{})
			cluster.setReady(true)
			path := filepath.Join(t.TempDir(), "borq.sqlite")
			manager := openTestManager(t, path, time.Second)
			environment := createManagerEnvironment(t, manager, cluster, "production")
			plans := []Plan{}
			for range 3 {
				plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
				assert.NoErr(err)
				plans = append(plans, plan)
			}
			key := spaceKey("acme", "production")
			stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
			assert.NoErr(err)
			stored.Queue = []string{plans[0].ID, plans[1].ID, plans[2].ID}
			stored.Space.Revision = 3
			stored.Space.Configuration = plans[2].Configuration
			stored.Manifests = plans[2].Manifests
			if activeStatus != "" {
				stored.ActiveOperation = plans[0].ID
				stored.Queue = stored.Queue[1:]
			}
			spaceWrite, err := documentWrite("space", key, stored, record.Revision)
			assert.NoErr(err)
			writes := []sqlite.Write{spaceWrite}
			accepted := time.Now().Add(-time.Hour)
			for index, plan := range plans {
				operation := Operation{ID: plan.ID, PlanID: plan.ID, SpaceID: environment.ID, Status: "queued", Phase: "applications", StartedAt: accepted}
				state := operationState{Revision: index + 1}
				if index == 0 && activeStatus != "" {
					operation.Status = activeStatus
					state.ExecutionStartedAt = time.Now()
					if activeStatus == "canceling" {
						state.StopStatus, state.StopMessage = "canceled", "deployment canceled by user"
					}
				}
				operationWrite, err := documentWrite("operation", plan.ID, operation, 0)
				assert.NoErr(err)
				stateWrite, err := documentWrite("operation-state", plan.ID, state, 0)
				assert.NoErr(err)
				writes = append(writes, operationWrite, stateWrite)
			}
			assert.NoErr(manager.database.Commit(ctx, writes))
			assert.NoErr(manager.Close())
			reopened := openTestManager(t, path, time.Second)
			finished := waitManagerOperation(t, reopened, plans[2].ID, "succeeded")
			assert.Equal(len(finished.Resources), 4)
			for _, resource := range finished.Resources {
				assert.Equal(resource.State, "succeeded")
			}
			for index, plan := range plans[:2] {
				expected := "superseded"
				if index == 0 && activeStatus == "canceling" {
					expected = "canceled"
				}
				operation, err := reopened.GetOperation(ctx, plan.ID)
				assert.NoErr(err)
				assert.Equal(operation.Status, expected)
			}
			assert.Equal(finished.StartedAt, accepted.UTC())
			stored, _, err = loadDocument[spaceRecord](ctx, reopened.database, "space", key)
			assert.NoErr(err)
			assert.Equal(stored.ActiveOperation, "")
			assert.Equal(len(stored.Queue), 0)
			assert.Equal(stored.Space.LastSuccessfulRevision, 3)
			state, _, err := loadDocument[operationState](ctx, reopened.database, "operation-state", plans[2].ID)
			assert.NoErr(err)
			assert.True(state.ExecutionStartedAt.After(accepted.Add(30 * time.Minute)))
			assert.Equal(reopened.operationDeadline(finished), state.ExecutionStartedAt.Add(time.Second))
		})
	}
}

// TestManagerFinalizationRetriesUnreadableSpace repairs a transiently unreadable
// space record and verifies its blocked queue continues without a server restart.
func TestManagerFinalizationRetriesUnreadableSpace(t *testing.T) {
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
	assert.NoErr(manager.database.Commit(ctx, writes))
	original, err := manager.database.Get(ctx, "space", key)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{{Collection: "space", Key: key, Value: json.RawMessage(`{"space":42}`), ExpectedRevision: original.Revision}}))
	assert.True(manager.completeQueuedOperation(operation, "succeeded", "resources ready") != nil)
	finalized := make(chan struct{})
	go func() { manager.finishOperation(operation, "succeeded", "resources ready"); close(finalized) }()
	select {
	case <-finalized:
		t.Fatal("finalization abandoned the active slot after a transient record failure")
	case <-time.After(25 * time.Millisecond):
	}
	corrupt, err := manager.database.Get(ctx, "space", key)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{{Collection: "space", Key: key, Value: original.Value, ExpectedRevision: corrupt.Revision}}))
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
