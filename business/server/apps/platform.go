package apps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/client-go/tools/clientcmd"
)

// ComponentInstallation reports a durable component reconciliation and its initial diff.
type ComponentInstallation struct {
	// ID identifies one reconciliation across client disconnects and server restarts.
	ID string `json:"id"`
	// ClusterID is stable for an organization and cluster name; management uses management.
	ClusterID string `json:"clusterId"`
	// Status is disabled, queued, diffing, installing, ready, or failed.
	Status string `json:"status"`
	// Diff records the read-only comparison performed before any installation writes.
	Diff platform.ComponentDiff `json:"diff"`
	// Result contains installed chart versions and revisions without secret values.
	Result platform.Result `json:"result"`
	// Message describes the phase without exposing chart credentials.
	Message string `json:"message"`
	// UpdatedAt identifies when the installation state was saved.
	UpdatedAt time.Time `json:"updatedAt"`
	// NextRetryAt is the next automatic retry after a failed reconciliation.
	NextRetryAt time.Time `json:"nextRetryAt"`
}

type componentRecord struct {
	Installation        ComponentInstallation `json:"installation"`
	TelemetryHash       string                `json:"telemetryHash,omitempty"`
	Upgrade             bool                  `json:"upgrade,omitempty"`
	UpgradeComponents   []string              `json:"upgradeComponents,omitempty"`
	ManagementOperation string                `json:"managementOperation,omitempty"`
	RecoverPending      bool                  `json:"recoverPending,omitempty"`
	Attempts            int                   `json:"attempts,omitempty"`
}

func storeClusterKubeconfig(path, source string) (bool, error) {
	configuration, err := clientcmd.Load([]byte(source))
	if err != nil {
		return false, fmt.Errorf("%w: invalid kubeconfig document", ErrInvalid)
	}
	encoded, err := clientcmd.Write(*configuration)
	if err != nil {
		return false, fmt.Errorf("%w: invalid kubeconfig document", ErrInvalid)
	}
	previous, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(previous, encoded) {
			return false, fmt.Errorf("%w: cluster is already registered with different credentials", ErrConflict)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create private kubeconfig: %w", err)
	}
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("save private kubeconfig: %w", err)
	}
	return true, nil
}

func (manager Manager) platformCredentialsInput(id string) platform.CredentialsInput {
	return platform.CredentialsInput{ManagementKubeconfigPath: manager.runtime.managementKubeconfigPath, Namespace: manager.runtime.platformSettings.Namespace, TargetID: id}
}

func (manager Manager) saveComponentRecord(ctx context.Context, id string, stored componentRecord) error {
	manager.runtime.componentMutex.Lock()
	defer manager.runtime.componentMutex.Unlock()
	return manager.saveComponentRecordLocked(ctx, id, stored)
}

func (manager Manager) saveComponentRecordLocked(ctx context.Context, id string, stored componentRecord) error {
	_, previous, err := loadDocument[componentRecord](ctx, manager.database, "components", id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	stored.Installation.UpdatedAt = time.Now().UTC()
	write, err := documentWrite("components", id, stored, previous.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

// ReconcileManagementComponents installs missing management releases or explicitly upgrades them.
func (manager Manager) ReconcileManagementComponents(ctx context.Context, upgrade bool) (ComponentInstallation, error) {
	return manager.reconcileManagementComponents(ctx, upgrade, nil)
}

func (manager Manager) reconcileManagementComponents(ctx context.Context, upgrade bool, names []string) (ComponentInstallation, error) {
	if !manager.runtime.infrastructureEnabled {
		return ComponentInstallation{}, fmt.Errorf("%w: infrastructure management is disabled", ErrInvalid)
	}
	manager.runtime.platformMutex.Lock()
	defer manager.runtime.platformMutex.Unlock()
	stored := componentRecord{Installation: ComponentInstallation{ClusterID: "management", Status: "installing", Message: "reconciling management components"}}
	if err := manager.saveComponentRecord(ctx, "management", stored); err != nil {
		return ComponentInstallation{}, err
	}
	settings, settingsErr := manager.GetPlatformSettings(ctx)
	if settingsErr != nil {
		return ComponentInstallation{}, settingsErr
	}
	result, runErr := platform.ReconcileManagement(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: settings, Upgrade: upgrade, UpgradeComponents: names})
	if runErr == nil {
		runErr = manager.resumeOperationWorkers(ctx)
	}
	stored.Installation.Result = result
	stored.Installation.Status, stored.Installation.Message = "ready", "management components ready"
	if runErr != nil {
		stored.Installation.Status, stored.Installation.Message = "failed", "management component reconciliation failed"
	}
	stored.Installation.UpdatedAt = time.Now().UTC()
	finalContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := manager.saveComponentRecord(finalContext, "management", stored); err != nil {
		return ComponentInstallation{}, err
	}
	if runErr == nil {
		manager.startCredentialRenewal()
	}
	return stored.Installation, runErr
}

func (manager Manager) reconcileClusterPlatform(ctx context.Context, cluster Cluster) (ComponentInstallation, error) {
	manager.runtime.platformMutex.Lock()
	defer manager.runtime.platformMutex.Unlock()
	id := platform.ClusterID(cluster.Organization, cluster.Name)
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ComponentInstallation{}, err
	}
	stored.Installation.Status, stored.Installation.Message = "diffing", "comparing live target components"
	stored.Installation.NextRetryAt = time.Time{}
	stored.Attempts++
	if err := manager.saveComponentRecord(ctx, id, stored); err != nil {
		return ComponentInstallation{}, err
	}
	settings, settingsErr := manager.GetPlatformSettings(ctx)
	if settingsErr != nil {
		return ComponentInstallation{}, settingsErr
	}
	input := platform.TargetInput{
		KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster,
		Organization: cluster.Organization, Name: cluster.Name, ID: id, Settings: settings,
		Upgrade: stored.Upgrade, RecoverPending: stored.RecoverPending, UpgradeComponents: stored.UpgradeComponents,
	}
	// The complete live diff is durable before credentials, namespaces, or chart
	// releases can change. A failed read must never fall through to installation.
	difference, runErr := platform.DiffTarget(ctx, input)
	if runErr == nil {
		stored.Installation.Diff = difference
		for _, component := range difference.Components {
			if component.Action == "conflict" {
				runErr = fmt.Errorf("%w: an existing component installation requires attention", ErrConflict)
				break
			}
		}
		if runErr == nil {
			stored.Installation.Status, stored.Installation.Message = "installing", "reconciling deployment target components"
		}
		if err := manager.saveComponentRecord(ctx, id, stored); err != nil {
			runErr = err
		}
		if runErr == nil {
			stored, runErr = manager.installClusterComponents(ctx, id, input, stored)
		}
	}
	if manager.runtime.context.Err() != nil {
		// Keep the same run unfinished: startup will re-diff live state and resume.
		return stored.Installation, manager.runtime.context.Err()
	}
	stored.Installation.Status, stored.Installation.Message = "ready", "deployment target components ready"
	if runErr != nil {
		stored.Installation.Status, stored.Installation.Message = "failed", "deployment target component reconciliation failed"
		if errors.Is(runErr, ErrConflict) {
			stored.Installation.Message = "existing component installations require attention; inspect cluster diff"
		}
		stored.Installation.NextRetryAt = time.Now().UTC().Add(componentRetryDelay(stored.Attempts))
	} else {
		stored.Attempts = 0
		stored.Upgrade = false
		stored.RecoverPending = false
	}
	if errors.Is(runErr, platform.ErrTargetOwnership) {
		runErr = fmt.Errorf("%w: %v", ErrConflict, runErr)
	}
	stored.Installation.UpdatedAt = time.Now().UTC()
	finalContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := manager.saveComponentRecord(finalContext, id, stored); err != nil {
		return ComponentInstallation{}, err
	}
	return stored.Installation, runErr
}

func (manager Manager) installClusterComponents(ctx context.Context, id string, input platform.TargetInput, stored componentRecord) (componentRecord, error) {
	credentials, err := platform.EnsureClusterCredentials(ctx, manager.platformCredentialsInput(id))
	if err != nil {
		return stored, err
	}
	digest := sha256.Sum256([]byte(credentials.TelemetryToken))
	stored.TelemetryHash = hex.EncodeToString(digest[:])
	if err := manager.saveComponentRecord(ctx, id, stored); err != nil {
		return stored, err
	}
	input.SecretMount, input.ClusterToken = credentials.Mount, credentials.VaultToken
	stored.Installation.Result, err = platform.ReconcileTarget(ctx, input)
	return stored, err
}

// ReconcileClusterComponents durably queues installation or an explicit upgrade.
func (manager Manager) ReconcileClusterComponents(ctx context.Context, organization, name string, upgrade bool) (ComponentInstallation, error) {
	if !manager.runtime.infrastructureEnabled {
		return ComponentInstallation{}, fmt.Errorf("%w: infrastructure management is disabled", ErrInvalid)
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return ComponentInstallation{}, err
	}
	return manager.queueClusterComponents(ctx, cluster, upgrade)
}

// GetManagementComponents combines operation status with current Helm/catalog state.
func (manager Manager) GetManagementComponents(ctx context.Context) (ComponentInstallation, error) {
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", "management")
	if !manager.runtime.infrastructureEnabled || (err != nil && !errors.Is(err, ErrNotFound)) || stored.Installation.Status == "installing" {
		return stored.Installation, err
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return ComponentInstallation{}, err
	}
	result, err := platform.ReadManagement(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: settings})
	if err != nil {
		return ComponentInstallation{}, err
	}
	if stored.Installation.ClusterID == "" {
		stored.Installation = ComponentInstallation{ClusterID: "management", Status: "ready", Message: "management component state loaded", UpdatedAt: time.Now().UTC()}
	}
	return observeComponentState(stored.Installation, result), nil
}

// GetClusterSetup describes required target components using the configured namespace.
func (manager Manager) GetClusterSetup() (platform.TargetSetup, error) {
	return platform.GetTargetSetup(manager.runtime.platformSettings)
}

// GetClusterComponents returns saved worker state without waiting for the target cluster.
func (manager Manager) GetClusterComponents(ctx context.Context, organization, name string) (ComponentInstallation, error) {
	_, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return ComponentInstallation{}, err
	}
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", platform.ClusterID(organization, name))
	return stored.Installation, err
}

// GetClusterManagementComponents reads management services on this saved cluster
// link without falling back to Borq's own hosting cluster or changing releases.
func (manager Manager) GetClusterManagementComponents(ctx context.Context, organization, name string) (platform.Result, error) {
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return platform.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return platform.ReadClusterManagement(ctx, platform.TargetInput{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster, Settings: manager.runtime.platformSettings})
}

// DiffClusterComponents compares a registered target without scheduling or writing anything.
func (manager Manager) DiffClusterComponents(ctx context.Context, organization, name string, upgrade bool) (platform.ComponentDiff, error) {
	if !manager.runtime.infrastructureEnabled {
		return platform.ComponentDiff{}, fmt.Errorf("%w: infrastructure management is disabled", ErrInvalid)
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return platform.ComponentDiff{}, err
	}
	result, err := platform.DiffTarget(ctx, platform.TargetInput{
		KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster,
		Organization: organization, Name: name, ID: platform.ClusterID(organization, name), Settings: manager.runtime.platformSettings, Upgrade: upgrade,
	})
	if errors.Is(err, platform.ErrTargetOwnership) {
		return platform.ComponentDiff{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return result, err
}

func observeComponentState(installation ComponentInstallation, result platform.Result) ComponentInstallation {
	installation.Result = result
	for _, component := range result.Components {
		if component.Status != "deployed" {
			installation.Status, installation.Message = "failed", "components require reconciliation"
		}
	}
	return installation
}

// AuthorizeTelemetry checks a per-cluster bearer key against its persisted digest.
// Raw keys remain in OpenBao and never enter the database or HTTP responses.
func (manager Manager) AuthorizeTelemetry(ctx context.Context, id, token string) (bool, error) {
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if token == "" || stored.TelemetryHash == "" {
		return false, nil
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare([]byte(stored.TelemetryHash), []byte(hex.EncodeToString(digest[:]))) == 1, nil
}
