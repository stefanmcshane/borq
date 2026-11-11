package apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// ManagementDestroyStatus combines durable progress and the accepted teardown plan.
type ManagementDestroyStatus struct {
	// Operation remains available through server restarts and client disconnects.
	Operation Operation `json:"operation"`
	// Plan anchors cleanup and final server removal to the accepted installation.
	Plan platform.ManagementDestroyPlan `json:"plan"`
}

type managementDestroyTask struct {
	Plan             platform.ManagementDestroyPlan `json:"plan"`
	Deadline         time.Time                      `json:"deadline"`
	ExecutionPlanned bool                           `json:"executionPlanned"`
}

type managementDestroyMarker struct {
	OperationID string `json:"operationId"`
}

// ManagementDestroyActive reports the durable maintenance fence, including failed cleanup.
func (manager Manager) ManagementDestroyActive() bool {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	return manager.runtime.destroying
}

// BeginApplicationRequest admits ordinary API dispatch until teardown is accepted.
// The caller must release once when dispatch finishes. Counting requests keeps the
// teardown drain cancellable even when an existing request body stops arriving.
func (manager Manager) BeginApplicationRequest(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return nil, errors.New("application manager is closing")
	}
	if manager.runtime.destroying {
		return nil, fmt.Errorf("%w: management teardown is active; inspect its operation or retry management destroy", ErrConflict)
	}
	if manager.runtime.activeRequests == 0 {
		manager.runtime.requestsDrained = make(chan struct{})
	}
	manager.runtime.activeRequests++
	return func() {
		manager.runtime.mutex.Lock()
		defer manager.runtime.mutex.Unlock()
		manager.runtime.activeRequests--
		if manager.runtime.activeRequests == 0 {
			close(manager.runtime.requestsDrained)
			manager.runtime.requestsDrained = nil
		}
	}, nil
}

// PlanManagementDestroy observes server-owned resources without starting teardown.
func (manager Manager) PlanManagementDestroy(ctx context.Context) (platform.ManagementDestroyPlan, error) {
	manager.runtime.mutex.Lock()
	marker, _, err := loadDocument[managementDestroyMarker](ctx, manager.database, "management-destroy", "active")
	if err == nil {
		task, _, taskErr := loadDocument[managementDestroyTask](ctx, manager.database, "management-destroy-task", marker.OperationID)
		manager.runtime.mutex.Unlock()
		return task.Plan, taskErr
	}
	manager.runtime.mutex.Unlock()
	if !errors.Is(err, ErrNotFound) {
		return platform.ManagementDestroyPlan{}, err
	}
	if !manager.runtime.infrastructureEnabled {
		return platform.ManagementDestroyPlan{}, fmt.Errorf("%w: management teardown requires infrastructure management", ErrInvalid)
	}
	plan, err := platform.PlanManagementDestroy(ctx, manager.managementDestroyInput(""))
	if err != nil {
		return plan, err
	}
	if !managementDestroyHasServer(plan) {
		return plan, fmt.Errorf("%w: management teardown requires the running Borq Deployment", ErrConflict)
	}
	return plan, nil
}

// DestroyManagement accepts one durable server operation; retries reuse its identity.
func (manager Manager) DestroyManagement(ctx context.Context) (Operation, error) {
	manager.runtime.mutex.Lock()
	operation, exists, err := manager.retryManagementDestroyLocked(ctx)
	manager.runtime.mutex.Unlock()
	if err != nil || exists {
		return operation, err
	}
	plan, err := manager.PlanManagementDestroy(ctx)
	if err != nil {
		return Operation{}, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	operation, exists, err = manager.retryManagementDestroyLocked(ctx)
	if err != nil || exists {
		return operation, err
	}
	id, err := newId()
	if err != nil {
		return Operation{}, err
	}
	now := time.Now().UTC()
	operation = Operation{ID: id, Status: "queued", Phase: "management-destroy", Message: "Management teardown accepted; draining active requests and reconciliation.", StartedAt: now, UpdatedAt: now, Resources: []kubeapps.ResourceStatus{}}
	task := managementDestroyTask{Plan: plan, Deadline: now.Add(manager.runtime.rolloutTimeout * 4)}
	markerWrite, err := documentWrite("management-destroy", "active", managementDestroyMarker{OperationID: id}, 0)
	if err != nil {
		return Operation{}, err
	}
	taskWrite, err := documentWrite("management-destroy-task", id, task, 0)
	if err != nil {
		return Operation{}, err
	}
	operationWrite, err := documentWrite("management-operation", id, operation, 0)
	if err != nil {
		return Operation{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{markerWrite, taskWrite, operationWrite}); err != nil {
		return Operation{}, translateStoreError(err)
	}
	manager.runtime.destroying = true
	manager.runtime.cancel()
	manager.startManagementDestroyLocked(operation, task)
	return operation, nil
}

// The admission mutex serializes both durable acceptance and worker registration.
func (manager Manager) retryManagementDestroyLocked(ctx context.Context) (Operation, bool, error) {
	if manager.runtime.closing {
		return Operation{}, false, errors.New("application manager is closing")
	}
	marker, _, err := loadDocument[managementDestroyMarker](ctx, manager.database, "management-destroy", "active")
	if errors.Is(err, ErrNotFound) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	operation, _, err := loadDocument[Operation](ctx, manager.database, "management-operation", marker.OperationID)
	if err != nil {
		return Operation{}, true, err
	}
	if manager.runtime.destroyRunning || operation.Status == "succeeded" {
		return operation, true, nil
	}
	task, _, err := loadDocument[managementDestroyTask](ctx, manager.database, "management-destroy-task", operation.ID)
	if err != nil {
		return Operation{}, true, err
	}
	task.Deadline = time.Now().UTC().Add(manager.runtime.rolloutTimeout * 4)
	operation.Status, operation.Message = "queued", "Retrying management teardown; ordinary work remains paused."
	if err := manager.saveManagementDestroy(ctx, operation, task); err != nil {
		return Operation{}, true, err
	}
	manager.runtime.destroying = true
	manager.startManagementDestroyLocked(operation, task)
	return operation, true, nil
}

// GetManagementDestroy reads progress without restarting reconciliation.
func (manager Manager) GetManagementDestroy(ctx context.Context, id string) (ManagementDestroyStatus, error) {
	task, _, err := loadDocument[managementDestroyTask](ctx, manager.database, "management-destroy-task", id)
	if err != nil {
		return ManagementDestroyStatus{}, err
	}
	operation, err := manager.GetManagementOperation(ctx, id)
	return ManagementDestroyStatus{Operation: operation, Plan: task.Plan}, err
}

func (manager Manager) managementDestroyInput(id string) platform.ManagementDestroyInput {
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	return platform.ManagementDestroyInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Namespace: namespace, OperationID: id, Timeout: manager.runtime.rolloutTimeout * 4}
}

func managementDestroyHasServer(plan platform.ManagementDestroyPlan) bool {
	for _, identity := range plan.Server {
		if identity.Resource == "deployments" && identity.Name == "borq" && identity.UID != "" {
			return true
		}
	}
	return false
}

func (manager Manager) initializeManagementDestroy(ctx context.Context) (bool, error) {
	marker, record, err := loadDocument[managementDestroyMarker](ctx, manager.database, "management-destroy", "active")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status, err := manager.GetManagementDestroy(ctx, marker.OperationID)
	if err != nil {
		return false, err
	}
	task, _, err := loadDocument[managementDestroyTask](ctx, manager.database, "management-destroy-task", marker.OperationID)
	if err != nil {
		return false, err
	}
	// The immutable cluster receipt commits before the final SQLite update. A
	// crash in between must not resume cleanup against an already removed core.
	if status.Operation.Status != "succeeded" && task.ExecutionPlanned {
		completed, err := platform.ManagementDestroyCompleted(ctx, manager.managementDestroyInput(marker.OperationID), status.Plan)
		if err != nil {
			return false, err
		}
		if completed {
			task.Plan.Executed = true
			status.Plan = task.Plan
			status.Operation.Status = "succeeded"
			status.Operation.Message = "Managed resource cleanup completed; recovered its immutable completion receipt."
			if err := manager.saveManagementDestroy(ctx, status.Operation, task); err != nil {
				return false, err
			}
		}
	}
	if status.Operation.Status == "succeeded" {
		recreated, err := platform.ManagementServerRecreated(ctx, manager.managementDestroyInput(marker.OperationID), status.Plan)
		if err != nil {
			return false, err
		}
		if recreated {
			if err := manager.database.Commit(ctx, []sqlite.Write{{Collection: "management-destroy", Key: "active", ExpectedRevision: record.Revision, Delete: true}}); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	manager.runtime.destroying = true
	manager.runtime.cancel()
	if status.Operation.Status == "queued" || status.Operation.Status == "running" {
		manager.runtime.mutex.Lock()
		manager.startManagementDestroyLocked(status.Operation, task)
		manager.runtime.mutex.Unlock()
	}
	return true, nil
}

func (manager Manager) startManagementDestroyLocked(operation Operation, task managementDestroyTask) {
	ctx, cancel := context.WithDeadline(manager.runtime.lifecycleContext, task.Deadline)
	manager.runtime.destroyCancel = cancel
	manager.runtime.destroyRunning = true
	manager.runtime.destroyWorkers.Add(1)
	go func() {
		defer manager.runtime.destroyWorkers.Done()
		defer cancel()
		defer func() {
			manager.runtime.mutex.Lock()
			manager.runtime.destroyRunning = false
			manager.runtime.mutex.Unlock()
		}()
		manager.runManagementDestroy(ctx, operation, task)
	}()
}

func (manager Manager) runManagementDestroy(ctx context.Context, operation Operation, task managementDestroyTask) {
	operation.Status, operation.Message = "running", "Draining active requests and stopping reconciliation; the Borq API stays available."
	progress, cancelProgress := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	saveErr := manager.saveManagementDestroy(progress, operation, task)
	cancelProgress()
	if saveErr != nil {
		return
	}
	err := manager.quiesceForManagementDestroy(ctx)
	if err == nil && !task.ExecutionPlanned {
		var plan platform.ManagementDestroyPlan
		plan, err = platform.PlanManagementDestroy(ctx, manager.managementDestroyInput(operation.ID))
		if err == nil && !sameManagementDestroyInstallation(task.Plan, plan) {
			err = fmt.Errorf("%w: management server identity or network configuration changed during teardown admission", ErrConflict)
		}
		if err == nil {
			task.Plan = plan
			task.ExecutionPlanned = true
			err = manager.saveManagementDestroy(ctx, operation, task)
		}
	}
	if err == nil {
		operation.Message = "Removing managed resources and provider exposure; retaining the Borq server and recovery data."
		err = manager.saveManagementDestroy(ctx, operation, task)
	}
	if err == nil {
		var result platform.ManagementDestroyPlan
		result, err = platform.DestroyManagementResources(ctx, manager.managementDestroyInput(operation.ID), task.Plan)
		if err == nil {
			task.Plan = result
		}
	}
	manager.runtime.mutex.Lock()
	closing := manager.runtime.closing
	manager.runtime.mutex.Unlock()
	if closing || manager.runtime.lifecycleContext.Err() != nil {
		return
	}
	operation.Status, operation.Message = "succeeded", "Managed resource cleanup completed. The CLI can now remove the Borq server using its completion receipt."
	if err != nil {
		operation.Status = "failed"
		operation.Message = "Management teardown failed; ordinary work remains paused. Check Borq server logs, resolve the failure, and retry management destroy."
		slog.ErrorContext(ctx, "management_destroy_failed", "operation_id", operation.ID, "error", err)
	}
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if saveErr := manager.saveManagementDestroy(final, operation, task); saveErr != nil {
		slog.ErrorContext(final, "management_destroy_progress_failed", "operation_id", operation.ID, "error", saveErr)
	}
}

func (manager Manager) quiesceForManagementDestroy(ctx context.Context) error {
	manager.runtime.cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.runtime.mutex.Lock()
	drained := manager.runtime.requestsDrained
	manager.runtime.mutex.Unlock()
	if drained != nil {
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	manager.runtime.quiescenceOnce.Do(func() {
		go func() {
			// Management startup runs outside the ordinary worker group. Cancellation
			// reaches its linked context; this lock waits for its final writes too.
			manager.runtime.platformMutex.Lock()
			manager.runtime.platformMutex.Unlock()
			manager.runtime.workers.Wait()
			close(manager.runtime.quiescenceDone)
		}()
	})
	select {
	case <-manager.runtime.quiescenceDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sameManagementDestroyInstallation(left, right platform.ManagementDestroyPlan) bool {
	if left.Namespace != right.Namespace || left.NamespaceUID != right.NamespaceUID || left.ClusterUID != right.ClusterUID {
		return false
	}
	for _, pair := range []struct {
		accepted, current []platform.ManagementDestroyIdentity
	}{
		{left.Server, right.Server}, {left.Dependencies, right.Dependencies},
	} {
		if len(pair.accepted) != len(pair.current) {
			return false
		}
		for _, identity := range pair.accepted {
			found := false
			for _, candidate := range pair.current {
				if identity.Group == candidate.Group && identity.Version == candidate.Version && identity.Resource == candidate.Resource && identity.Namespace == candidate.Namespace && identity.Name == candidate.Name && identity.UID == candidate.UID && identity.OwnershipHash == candidate.OwnershipHash {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func (manager Manager) saveManagementDestroy(ctx context.Context, operation Operation, task managementDestroyTask) error {
	_, storedTask, err := loadDocument[managementDestroyTask](ctx, manager.database, "management-destroy-task", operation.ID)
	if err != nil {
		return err
	}
	_, storedOperation, err := loadDocument[Operation](ctx, manager.database, "management-operation", operation.ID)
	if err != nil {
		return err
	}
	operation.UpdatedAt = time.Now().UTC()
	taskWrite, err := documentWrite("management-destroy-task", operation.ID, task, storedTask.Revision)
	if err != nil {
		return err
	}
	operationWrite, err := documentWrite("management-operation", operation.ID, operation, storedOperation.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{taskWrite, operationWrite})
}
