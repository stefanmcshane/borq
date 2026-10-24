package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type operationState struct {
	Revision           int       `json:"revision"`
	ExecutionStartedAt time.Time `json:"executionStartedAt"`
	StopStatus         string    `json:"stopStatus,omitempty"`
	StopMessage        string    `json:"stopMessage,omitempty"`
}

func operationFinished(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled" || status == "superseded"
}

func (manager Manager) latestAcceptedOperation(ctx context.Context, stored spaceRecord) (string, error) {
	if stored.LatestOperation != "" || stored.Space.Revision == 0 {
		return stored.LatestOperation, nil
	}
	// Older records lack the retained reference. Recover it from the accepted
	// revision, including terminal operations that no longer appear in the queue.
	records, err := manager.database.List(ctx, "operation")
	if err != nil {
		return "", err
	}
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return "", err
		}
		if operation.SpaceID != stored.Space.ID {
			continue
		}
		plan, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return "", err
		}
		if plan.Plan.BaseRevision+1 == stored.Space.Revision {
			return operation.ID, nil
		}
	}
	return "", nil
}

// CancelOperation stops an accepted deployment while retaining its desired configuration.
// Running Kubernetes requests are interrupted before the replacement starts.
func (manager Manager) CancelOperation(ctx context.Context, id string) (Operation, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Operation{}, errors.New("application manager is stopping")
	}
	operation, err := manager.GetOperation(ctx, id)
	if err != nil || operationFinished(operation.Status) || operation.Status == "canceling" {
		return operation, err
	}
	stored, record, err := manager.operationSpace(ctx, operation)
	if err != nil {
		return Operation{}, err
	}
	active := stored.ActiveOperation == id
	operation, writes, err := manager.stopOperationWrites(ctx, id, "canceled", "deployment canceled by user", active)
	if err != nil {
		return Operation{}, err
	}
	stored.Queue = slices.DeleteFunc(stored.Queue, func(queued string) bool { return queued == id })
	write, err := documentWrite("space", record.Key, stored, record.Revision)
	if err != nil {
		return Operation{}, err
	}
	writes = append(writes, write)
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Operation{}, translateStoreError(err)
	}
	if cancel := manager.runtime.operationCancels[id]; cancel != nil {
		cancel()
	}
	return operation, nil
}

func (manager Manager) operationSpace(ctx context.Context, operation Operation) (spaceRecord, sqlite.Record, error) {
	records, err := manager.database.List(ctx, "space")
	if err != nil {
		return spaceRecord{}, sqlite.Record{}, err
	}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return spaceRecord{}, sqlite.Record{}, err
		}
		if stored.Space.ID == operation.SpaceID {
			return stored, record, nil
		}
	}
	return spaceRecord{}, sqlite.Record{}, ErrNotFound
}

// stopOperationWrites is called with the runtime mutex held; its caller commits
// the stop request atomically with queue changes before signaling the worker.
func (manager Manager) stopOperationWrites(ctx context.Context, id, status, message string, active bool) (Operation, []sqlite.Write, error) {
	operation, record, err := loadDocument[Operation](ctx, manager.database, "operation", id)
	if err != nil {
		return Operation{}, nil, err
	}
	if operationFinished(operation.Status) || operation.Status == "canceling" {
		return operation, nil, nil
	}
	operation.Status, operation.Message, operation.UpdatedAt = status, message, time.Now().UTC()
	writes := []sqlite.Write{}
	if active {
		state, stateRecord, err := loadDocument[operationState](ctx, manager.database, "operation-state", id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Operation{}, nil, err
		}
		state.StopStatus, state.StopMessage = status, message
		write, err := documentWrite("operation-state", id, state, stateRecord.Revision)
		if err != nil {
			return Operation{}, nil, err
		}
		writes = append(writes, write)
		operation.Status = "canceling"
		operation.Message = "stopping deployment: " + message
	}
	write, err := documentWrite("operation", id, operation, record.Revision)
	if err != nil {
		return Operation{}, nil, err
	}
	return operation, append(writes, write), nil
}

// enqueuePlan replaces unfinished work in the same transaction as the desired revision.
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
	if saved.Plan.BaseRevision != stored.Space.Revision {
		return Operation{}, fmt.Errorf("%w: plan is stale; create a new plan from the accepted configuration", ErrConflict)
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
	stored.LatestOperation = id
	stored.Space.Configuration = saved.Plan.Configuration
	stored.Manifests = saved.Plan.Manifests
	stored.EnvSnapshots = envSnapshots(saved.EnvironmentSecrets)
	stored.EnvSelectors = saved.EnvSelectors
	state := operationState{Revision: stored.Space.Revision}
	writes := []sqlite.Write{}
	for _, queuedID := range stored.Queue {
		_, canceledWrites, err := manager.stopOperationWrites(ctx, queuedID, "superseded", "superseded by deployment "+id, false)
		if err != nil {
			return Operation{}, err
		}
		writes = append(writes, canceledWrites...)
	}
	stored.Queue = nil
	start := stored.ActiveOperation == ""
	if start {
		stored.ActiveOperation = id
		state.ExecutionStartedAt = now
		operation.Status, operation.Message = "pending", "apply accepted"
	} else {
		_, canceledWrites, err := manager.stopOperationWrites(ctx, stored.ActiveOperation, "superseded", "superseded by deployment "+id, true)
		if err != nil {
			return Operation{}, err
		}
		writes = append(writes, canceledWrites...)
		stored.Queue = []string{id}
		operation.Message = "waiting for the previous deployment to stop"
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
	writes = append(writes, environmentWrite, operationWrite, stateWrite)
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Operation{}, translateStoreError(err)
	}
	if start {
		manager.startOperation(operation)
	} else if cancel := manager.runtime.operationCancels[stored.ActiveOperation]; cancel != nil {
		cancel()
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
	stored, record, err := manager.operationSpace(ctx, operation)
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
		saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return err
		}
		state.Revision = saved.Plan.BaseRevision + 1
	}
	if state.StopStatus != "" {
		if status != state.StopStatus {
			message = state.StopMessage
		}
		status = state.StopStatus
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
	environmentWrite, err := documentWrite("space", record.Key, stored, record.Revision)
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

// resumeQueueHeads also collapses FIFO backlogs written by older servers.
func (manager Manager) resumeQueueHeads(ctx context.Context, queued []Operation) ([]Operation, error) {
	resumed := []Operation{}
	seen := map[string]bool{}
	for _, operation := range queued {
		if seen[operation.SpaceID] {
			continue
		}
		seen[operation.SpaceID] = true
		stored, record, err := manager.operationSpace(ctx, operation)
		if err != nil {
			return nil, err
		}
		if len(stored.Queue) == 0 {
			continue
		}
		nextID := stored.Queue[len(stored.Queue)-1]
		writes := []sqlite.Write{}
		for _, previousID := range stored.Queue[:len(stored.Queue)-1] {
			_, stopped, err := manager.stopOperationWrites(ctx, previousID, "superseded", "superseded by deployment "+nextID, false)
			if err != nil {
				return nil, err
			}
			writes = append(writes, stopped...)
		}
		stored.Queue = []string{nextID}
		if stored.ActiveOperation != "" {
			_, stopped, err := manager.stopOperationWrites(ctx, stored.ActiveOperation, "superseded", "superseded by deployment "+nextID, true)
			if err != nil {
				return nil, err
			}
			writes = append(writes, stopped...)
			write, err := documentWrite("space", record.Key, stored, record.Revision)
			if err != nil {
				return nil, err
			}
			if err := manager.database.Commit(ctx, append(writes, write)); err != nil {
				return nil, err
			}
			continue
		}
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
		stored.Queue = nil
		for _, document := range []struct {
			collection, key string
			value           any
			revision        int64
		}{
			{"space", record.Key, stored, record.Revision}, {"operation", nextID, next, nextRecord.Revision}, {"operation-state", nextID, state, stateRecord.Revision},
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
