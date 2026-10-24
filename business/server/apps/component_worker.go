package apps

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func componentActive(status string) bool {
	return status == "queued" || status == "diffing" || status == "installing"
}

func queueComponentRecord(id string, stored componentRecord, upgrade bool) (componentRecord, error) {
	if componentActive(stored.Installation.Status) {
		return stored, nil
	}
	runID, err := newId()
	if err != nil {
		return componentRecord{}, err
	}
	stored.Upgrade = upgrade
	stored.Installation = ComponentInstallation{
		ID: runID, ClusterID: id, Status: "queued", Message: "deployment target reconciliation queued",
		Diff:   platform.ComponentDiff{Components: []platform.ComponentChange{}},
		Result: stored.Installation.Result, UpdatedAt: time.Now().UTC(),
	}
	if stored.Installation.Result.Components == nil {
		stored.Installation.Result.Components = []platform.ComponentState{}
	}
	return stored, nil
}

func (manager Manager) commitClusterRegistration(ctx context.Context, cluster Cluster, writes []sqlite.Write, reconcile bool) error {
	manager.runtime.componentMutex.Lock()
	defer manager.runtime.componentMutex.Unlock()
	stored, previous, err := loadDocument[componentRecord](ctx, manager.database, "components", cluster.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if reconcile {
		stored, err = queueComponentRecord(cluster.ID, stored, false)
		if err != nil {
			return err
		}
	} else if errors.Is(err, ErrNotFound) {
		stored.Installation = ComponentInstallation{
			ClusterID: cluster.ID, Status: "disabled", Message: "cluster registered without scheduling component reconciliation",
			Diff: platform.ComponentDiff{Components: []platform.ComponentChange{}}, UpdatedAt: time.Now().UTC(),
		}
		stored.Installation.Result.Components = []platform.ComponentState{}
		if !manager.runtime.infrastructureEnabled {
			stored.Installation.Message = "infrastructure management is disabled"
		}
	}
	write, err := documentWrite("components", cluster.ID, stored, previous.Revision)
	if err != nil {
		return err
	}
	writes = append(writes, write)
	if err := manager.database.Commit(ctx, writes); err != nil {
		return err
	}
	if reconcile {
		manager.wakeComponentWorker()
	}
	return nil
}

func (manager Manager) queueClusterComponents(ctx context.Context, cluster Cluster, upgrade bool) (ComponentInstallation, error) {
	manager.runtime.componentMutex.Lock()
	defer manager.runtime.componentMutex.Unlock()
	id := platform.ClusterID(cluster.Organization, cluster.Name)
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ComponentInstallation{}, err
	}
	if componentActive(stored.Installation.Status) {
		if upgrade && !stored.Upgrade {
			return ComponentInstallation{}, errors.Join(ErrConflict, errors.New("a component reconciliation is already active; retry the upgrade when it completes"))
		}
		return stored.Installation, nil
	}
	stored, err = queueComponentRecord(id, stored, upgrade)
	if err != nil {
		return ComponentInstallation{}, err
	}
	if err := manager.saveComponentRecordLocked(ctx, id, stored); err != nil {
		return ComponentInstallation{}, err
	}
	manager.wakeComponentWorker()
	return stored.Installation, nil
}

func (manager Manager) wakeComponentWorker() {
	select {
	case manager.runtime.componentWake <- struct{}{}:
	default:
	}
}

// startCredentialRenewal also resumes queued target reconciliations. Both tasks
// share one lifetime, and management startup calls this only after its vault is ready.
func (manager Manager) startCredentialRenewal() {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return
	}
	manager.runtime.credentialRenewalOnce.Do(func() {
		manager.runtime.workers.Add(1)
		go manager.runComponentWorker()
	})
}

func (manager Manager) runComponentWorker() {
	defer manager.runtime.workers.Done()
	// An interrupted run keeps its ID and is re-diffed against the live target.
	// This also queues registrations from the previous synchronous implementation.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := manager.recoverComponentRuns(manager.runtime.context); err == nil {
			break
		} else if manager.runtime.context.Err() != nil {
			return
		} else {
			slog.ErrorContext(manager.runtime.context, "target_reconciliation_recovery_failed", "error", err)
		}
		select {
		case <-manager.runtime.context.Done():
			return
		case <-ticker.C:
		}
	}
	for {
		manager.processComponentRuns()
		select {
		case <-manager.runtime.context.Done():
			return
		case <-manager.runtime.componentWake:
		case <-ticker.C:
		}
	}
}

func (manager Manager) recoverComponentRuns(ctx context.Context) error {
	records, err := manager.database.List(ctx, "cluster")
	if err != nil {
		return err
	}
	for _, record := range records {
		var cluster Cluster
		if err := json.Unmarshal(record.Value, &cluster); err != nil {
			return err
		}
		id := platform.ClusterID(cluster.Organization, cluster.Name)
		manager.runtime.componentMutex.Lock()
		stored, _, readErr := loadDocument[componentRecord](ctx, manager.database, "components", id)
		changed := false
		if errors.Is(readErr, ErrNotFound) {
			stored, readErr = queueComponentRecord(id, stored, false)
			changed = true
		} else if readErr == nil && (stored.Installation.Status == "diffing" || stored.Installation.Status == "installing") {
			stored.Installation.Status, stored.Installation.Message = "queued", "resuming deployment target reconciliation"
			stored.RecoverPending = true
			changed = true
		}
		if readErr == nil && changed {
			readErr = manager.saveComponentRecordLocked(ctx, id, stored)
		}
		manager.runtime.componentMutex.Unlock()
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func (manager Manager) processComponentRuns() {
	ctx := manager.runtime.context
	records, err := manager.database.List(ctx, "cluster")
	if err != nil {
		return
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return
		}
		var cluster Cluster
		if json.Unmarshal(record.Value, &cluster) != nil {
			continue
		}
		id := platform.ClusterID(cluster.Organization, cluster.Name)
		stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", id)
		if err != nil || !componentRunDue(stored.Installation, time.Now()) {
			continue
		}
		if stored.Installation.Status != "queued" {
			if _, err := manager.queueClusterComponents(ctx, cluster, stored.Upgrade); err != nil {
				continue
			}
		}
		timeout := 30 * time.Minute
		if manager.runtime.platformSettings.Timeout > 0 {
			timeout = 6 * manager.runtime.platformSettings.Timeout
		}
		attempt, cancel := context.WithTimeout(ctx, timeout)
		_, runErr := manager.reconcileClusterPlatform(attempt, cluster)
		if runErr != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "target_reconciliation_failed", "cluster_id", id, "error", runErr)
		}
		cancel()
	}
}

func componentRunDue(installation ComponentInstallation, now time.Time) bool {
	switch installation.Status {
	case "queued":
		return true
	case "failed":
		return !now.Before(installation.NextRetryAt)
	case "ready":
		return !now.Before(installation.UpdatedAt.Add(6 * time.Hour))
	default:
		return false
	}
}

func componentRetryDelay(attempts int) time.Duration {
	delay := 15 * time.Second
	for attempt := 1; attempt < attempts && delay < 5*time.Minute; attempt++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}
