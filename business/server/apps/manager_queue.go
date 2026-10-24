package apps

import (
	"context"
	"errors"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type operationState struct {
	Revision           int       `json:"revision"`
	ExecutionStartedAt time.Time `json:"executionStartedAt"`
}

// enqueuePlan records FIFO order in the same transaction as the accepted desired revision.
func (manager Manager) enqueuePlan(ctx context.Context, id string) (Operation, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Operation{}, errors.New("application manager is stopping")
	}
	previous, _, err := loadDocument[Operation](ctx, manager.database, "operation", id)
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", id)
	if err != nil {
		return Operation{}, err
	}
	environment := saved.Plan.Space
	key := spaceKey(environment.Organization, environment.Name)
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	if err != nil {
		return Operation{}, err
	}
	now := time.Now().UTC()
	jobs, _ := splitPlanManifests(saved.Plan.Manifests)
	phase := "applications"
	if len(jobs) > 0 {
		phase = "predeploy"
	}
	if len(saved.EnvironmentSecrets) > 0 {
		phase = "secrets"
	}
	operation := Operation{ID: id, PlanID: id, SpaceID: environment.ID, Status: "queued", Phase: phase, Message: "waiting for the previous apply in this environment", Resources: []kubeapps.ResourceStatus{}, StartedAt: now, UpdatedAt: now}
	stored.Space.Revision++
	stored.Space.Configuration = saved.Plan.Configuration
	stored.Manifests = saved.Plan.Manifests
	stored.EnvSnapshots = envSnapshots(saved.EnvironmentSecrets)
	stored.EnvSelectors = saved.EnvSelectors
	state := operationState{Revision: stored.Space.Revision}
	start := stored.ActiveOperation == ""
	if start {
		stored.ActiveOperation = id
		state.ExecutionStartedAt = now
		operation.Status, operation.Message = "pending", "apply accepted"
	} else {
		stored.Queue = append(stored.Queue, id)
	}
	environmentWrite, err := documentWrite("space", key, stored, record.Revision)
	if err != nil {
		return Operation{}, err
	}
	operationWrite, err := documentWrite("operation", id, operation, 0)
	if err != nil {
		return Operation{}, err
	}
	stateWrite, err := documentWrite("operation-state", id, state, 0)
	if err != nil {
		return Operation{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{environmentWrite, operationWrite, stateWrite}); err != nil {
		return Operation{}, translateStoreError(err)
	}
	if start {
		manager.startOperation(operation)
	}
	return operation, nil
}

func (manager Manager) operationDeadline(operation Operation) time.Time {
	state, _, err := loadDocument[operationState](manager.runtime.context, manager.database, "operation-state", operation.ID)
	if err != nil || state.ExecutionStartedAt.IsZero() {
		return operation.StartedAt.Add(manager.runtime.rolloutTimeout)
	}
	return state.ExecutionStartedAt.Add(manager.runtime.rolloutTimeout)
}

func (manager Manager) completeQueuedOperation(operation Operation, status, message string) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
	if err != nil {
		return err
	}
	environment := saved.Plan.Space
	key := spaceKey(environment.Organization, environment.Name)
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	if err != nil {
		return err
	}
	if stored.ActiveOperation != operation.ID {
		return nil
	}
	_, operationRecord, err := loadDocument[Operation](ctx, manager.database, "operation", operation.ID)
	if err != nil {
		return err
	}
	state, _, err := loadDocument[operationState](ctx, manager.database, "operation-state", operation.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if state.Revision == 0 {
		state.Revision = saved.Plan.BaseRevision + 1
	}
	stored.ActiveOperation = ""
	if status == "succeeded" {
		stored.Space.LastSuccessfulRevision = state.Revision
	}
	operation.Status, operation.Message, operation.UpdatedAt = status, message, time.Now().UTC()
	writes := []sqlite.Write{}
	next := Operation{}
	if len(stored.Queue) > 0 && !manager.runtime.closing {
		nextID := stored.Queue[0]
		stored.Queue = stored.Queue[1:]
		queued, queuedRecord, err := loadDocument[Operation](ctx, manager.database, "operation", nextID)
		if err != nil {
			return err
		}
		nextState, nextRecord, err := loadDocument[operationState](ctx, manager.database, "operation-state", nextID)
		if err != nil {
			return err
		}
		nextState.ExecutionStartedAt = time.Now().UTC()
		queued.Status, queued.Message, queued.UpdatedAt = "pending", "starting queued apply", nextState.ExecutionStartedAt
		queuedWrite, err := documentWrite("operation", nextID, queued, queuedRecord.Revision)
		if err != nil {
			return err
		}
		stateWrite, err := documentWrite("operation-state", nextID, nextState, nextRecord.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, queuedWrite, stateWrite)
		stored.ActiveOperation = nextID
		next = queued
	}
	environmentWrite, err := documentWrite("space", key, stored, record.Revision)
	if err != nil {
		return err
	}
	operationWrite, err := documentWrite("operation", operation.ID, operation, operationRecord.Revision)
	if err != nil {
		return err
	}
	writes = append(writes, environmentWrite, operationWrite)
	if err := manager.database.Commit(ctx, writes); err != nil {
		return err
	}
	if next.ID != "" {
		manager.startOperation(next)
	}
	return nil
}

func (manager Manager) resumeQueueHeads(ctx context.Context, queued []Operation) ([]Operation, error) {
	resumed := []Operation{}
	for _, operation := range queued {
		saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return nil, err
		}
		environment := saved.Plan.Space
		key := spaceKey(environment.Organization, environment.Name)
		stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
		if err != nil {
			return nil, err
		}
		if stored.ActiveOperation != "" || len(stored.Queue) == 0 {
			continue
		}
		nextID := stored.Queue[0]
		next, nextRecord, err := loadDocument[Operation](ctx, manager.database, "operation", nextID)
		if err != nil {
			return nil, err
		}
		state, stateRecord, err := loadDocument[operationState](ctx, manager.database, "operation-state", nextID)
		if err != nil {
			return nil, err
		}
		state.ExecutionStartedAt = time.Now().UTC()
		next.Status, next.Message, next.UpdatedAt = "pending", "resuming queued apply", state.ExecutionStartedAt
		stored.ActiveOperation = nextID
		stored.Queue = stored.Queue[1:]
		writes := []sqlite.Write{}
		for _, document := range []struct {
			collection, key string
			value           any
			revision        int64
		}{
			{"space", key, stored, record.Revision}, {"operation", nextID, next, nextRecord.Revision}, {"operation-state", nextID, state, stateRecord.Revision},
		} {
			write, err := documentWrite(document.collection, document.key, document.value, document.revision)
			if err != nil {
				return nil, err
			}
			writes = append(writes, write)
		}
		if err := manager.database.Commit(ctx, writes); err != nil {
			return nil, err
		}
		resumed = append(resumed, next)
	}
	return resumed, nil
}
