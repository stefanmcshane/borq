package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrAddonExists lets API callers distinguish a duplicate install from other conflicts.
var ErrAddonExists = errors.New("custom chart already exists in this space")

// AddonInput describes a pinned Helm chart and its complete user overrides.
type AddonInput struct {
	// Repository is a public HTTP(S) Helm repository or OCI repository prefix.
	Repository string `json:"repository"`
	// Chart is the upstream chart name within Repository.
	Chart string `json:"chart"`
	// Version pins one exact chart version; implicit latest and ranges are rejected.
	Version string `json:"version"`
	// Values replaces the complete override map; Helm merges it with chart defaults.
	Values map[string]any `json:"values"`
	// Logs defaults to enabled. A pointer distinguishes omission from explicit disablement.
	Logs *bool `json:"logs,omitempty"`
	// PodSelector restricts collection labels to Pod templates with these labels.
	PodSelector map[string]string `json:"podSelector,omitempty"`
}

// Addon records accepted desired configuration independently of Helm's last successful revision.
type Addon struct {
	// Name identifies this addon within its space.
	Name string `json:"name"`
	// Specification contains the exact accepted chart pin and overrides.
	Specification AddonInput `json:"specification"`
	// Namespace is the space's fixed Kubernetes namespace.
	Namespace string `json:"namespace"`
	// ReleaseName is the isolated Helm release name.
	ReleaseName string `json:"releaseName"`
	// State is pending, deployed, failed, deleting, or removed.
	State string `json:"state"`
	// Message reports the latest operation outcome.
	Message string `json:"message"`
	// Revision is the installed Helm revision, independent of application revisions.
	Revision int `json:"revision"`
	// Operation is the latest durable addon operation ID.
	Operation string `json:"operation"`
	// UpdatedAt is the latest accepted configuration or operation completion time.
	UpdatedAt time.Time `json:"updatedAt"`
}

type addonRecord struct {
	Addon           Addon  `json:"addon"`
	Organization    string `json:"organization"`
	Space           string `json:"space"`
	Cluster         string `json:"cluster"`
	Owner           string `json:"owner"`
	ActiveOperation string `json:"activeOperation"`
}

type addonTask struct {
	Key    string `json:"key"`
	Remove bool   `json:"remove"`
}

func validateAddonInput(input AddonInput) error {
	repository, err := url.Parse(input.Repository)
	if err != nil || repository.Host == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" || (repository.Scheme != "https" && repository.Scheme != "http" && repository.Scheme != "oci") {
		return fmt.Errorf("%w: repository must be an HTTP(S) or OCI URL without credentials, query, or fragment", ErrInvalid)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(input.Chart) {
		return fmt.Errorf("%w: chart must be a published chart name", ErrInvalid)
	}
	if !regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`).MatchString(input.Version) {
		return fmt.Errorf("%w: version must pin an exact chart version", ErrInvalid)
	}
	for key, value := range input.PodSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("%w: podSelector must contain valid Kubernetes labels", ErrInvalid)
		}
	}
	return nil
}

// GetChartDefaults retrieves the original values.yaml for an exact chart pin without cluster access.
func (manager Manager) GetChartDefaults(ctx context.Context, input helm.ChartInput) (helm.ChartDefaults, error) {
	if err := validateAddonInput(AddonInput{Repository: input.Repository, Chart: input.Name, Version: input.Version}); err != nil {
		return helm.ChartDefaults{}, err
	}
	return helm.FetchDefaults(ctx, input)
}

// ListAddons reads the accepted addons for exactly one existing space.
func (manager Manager) ListAddons(ctx context.Context, organization, space string) ([]Addon, error) {
	if _, err := manager.GetSpace(ctx, organization, space); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "addon")
	if err != nil {
		return nil, err
	}
	result := []Addon{}
	for _, record := range records {
		var stored addonRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.Organization == organization && stored.Space == space && stored.Addon.State != "removed" {
			result = append(result, stored.Addon)
		}
	}
	return result, nil
}

// GetAddon returns accepted configuration and the latest durable Helm outcome.
func (manager Manager) GetAddon(ctx context.Context, organization, space, name string) (Addon, error) {
	stored, _, err := loadDocument[addonRecord](ctx, manager.database, "addon", addonKey(organization, space, name))
	if err != nil {
		return Addon{}, err
	}
	if stored.Addon.State == "removed" {
		return Addon{}, ErrNotFound
	}
	return stored.Addon, nil
}

// PutAddon durably installs one space-owned release. Apply explicitly allows replacing existing settings.
func (manager Manager) PutAddon(ctx context.Context, organization, space, name string, input AddonInput, apply bool) (Addon, error) {
	if err := validateNames(organization, space, name); err != nil {
		return Addon{}, err
	}
	if len(name) > 47 {
		return Addon{}, fmt.Errorf("%w: addon names must contain at most 47 characters", ErrInvalid)
	}
	if err := validateAddonInput(input); err != nil {
		return Addon{}, err
	}
	if input.Values == nil {
		input.Values = map[string]any{}
	}
	action := "install"
	if apply {
		action = "apply"
	}
	return manager.acceptAddon(ctx, organization, space, name, input, action)
}

// DeleteAddon durably schedules uninstall without deleting chart CRDs or shared namespaces.
func (manager Manager) DeleteAddon(ctx context.Context, organization, space, name string) (Addon, error) {
	return manager.acceptAddon(ctx, organization, space, name, AddonInput{}, "delete")
}

// Persisted keys, Helm release names, and collection labels retain their original
// identities so the public custom charts rename preserves existing installations.
func addonKey(organization, space, name string) string {
	return strings.Join([]string{organization, space, name}, "/")
}

func (manager Manager) acceptAddon(ctx context.Context, organization, space, name string, input AddonInput, action string) (Addon, error) {
	remove := action == "delete"
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Addon{}, errors.New("application manager is stopping")
	}
	target, err := manager.GetSpace(ctx, organization, space)
	if err != nil {
		return Addon{}, err
	}
	if manager.runtime.infrastructureEnabled && !remove {
		installation, err := manager.GetClusterComponents(ctx, organization, target.Cluster)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Addon{}, err
		}
		if installation.Status != "ready" {
			return Addon{}, errors.Join(ErrConflict, ErrComponentsNotReady)
		}
	}
	key := addonKey(organization, space, name)
	stored, record, err := loadDocument[addonRecord](ctx, manager.database, "addon", key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Addon{}, err
	}
	if action == "install" && err == nil && stored.Addon.State != "removed" {
		return Addon{}, errors.Join(ErrConflict, ErrAddonExists)
	}
	if remove && (errors.Is(err, ErrNotFound) || stored.Addon.State == "removed") {
		return Addon{}, ErrNotFound
	}
	if stored.ActiveOperation != "" {
		return Addon{}, fmt.Errorf("%w: addon operation %s is still running", ErrConflict, stored.ActiveOperation)
	}
	if stored.Owner == "" {
		stored = addonRecord{Organization: organization, Space: space, Cluster: target.Cluster, Owner: stableId("addon/" + key), Addon: Addon{Name: name, Namespace: target.Namespace, ReleaseName: "addon-" + name}}
	}
	if !remove {
		stored.Addon.Specification = input
	}
	now := time.Now().UTC()
	id, err := newId()
	if err != nil {
		return Addon{}, err
	}
	stored.ActiveOperation, stored.Addon.Operation, stored.Addon.UpdatedAt = id, id, now
	stored.Addon.State, stored.Addon.Message = "pending", "Helm installation or upgrade accepted"
	if remove {
		stored.Addon.State, stored.Addon.Message = "deleting", "Helm uninstall accepted"
	}
	operation := Operation{ID: id, SpaceID: target.ID, Status: "pending", Phase: "custom-charts", Message: stored.Addon.Message, Resources: []kubeapps.ResourceStatus{}, StartedAt: now, UpdatedAt: now}
	writes := []sqlite.Write{}
	for _, document := range []struct {
		collection, key string
		value           any
		revision        int64
	}{
		{"addon", key, stored, record.Revision}, {"addon-operation", id, operation, 0}, {"addon-task", id, addonTask{Key: key, Remove: remove}, 0},
	} {
		write, err := documentWrite(document.collection, document.key, document.value, document.revision)
		if err != nil {
			return Addon{}, err
		}
		writes = append(writes, write)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Addon{}, translateStoreError(err)
	}
	manager.startAddonOperation(operation)
	return stored.Addon, nil
}

// GetAddonOperation reads progress without retrying or reinstalling a chart.
func (manager Manager) GetAddonOperation(ctx context.Context, id string) (Operation, error) {
	operation, _, err := loadDocument[Operation](ctx, manager.database, "addon-operation", id)
	return operation, err
}

func (manager Manager) resumeAddonOperations(ctx context.Context) error {
	records, err := manager.database.List(ctx, "addon-operation")
	if err != nil {
		return err
	}
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		if operation.Status != "succeeded" && operation.Status != "failed" {
			manager.startAddonOperation(operation)
		}
	}
	return nil
}

func (manager Manager) startAddonOperation(operation Operation) {
	manager.runtime.workers.Add(1)
	go func() { defer manager.runtime.workers.Done(); manager.runAddonOperation(operation) }()
}

func (manager Manager) runAddonOperation(operation Operation) {
	select {
	case manager.runtime.addonSlots <- struct{}{}:
		defer func() { <-manager.runtime.addonSlots }()
	case <-manager.runtime.context.Done():
		return
	}
	ctx, cancel := context.WithTimeout(manager.runtime.context, manager.runtime.rolloutTimeout)
	defer cancel()
	task, _, err := loadDocument[addonTask](ctx, manager.database, "addon-task", operation.ID)
	if err != nil {
		return
	}
	stored, _, err := loadDocument[addonRecord](ctx, manager.database, "addon", task.Key)
	if err != nil || stored.ActiveOperation != operation.ID {
		return
	}
	state, reconcileErr := manager.reconcileAddon(ctx, stored, task.Remove)
	if manager.runtime.context.Err() != nil {
		return
	} // Restart resumes the persisted operation.
	for {
		if err := manager.finishAddonOperation(operation, task, state, reconcileErr); err == nil {
			return
		}
		if err := manager.waitForPoll(manager.runtime.context); err != nil {
			return
		}
	}
}

func (manager Manager) reconcileAddon(ctx context.Context, stored addonRecord, remove bool) (helm.ReleaseState, error) {
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return helm.ReleaseState{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return helm.ReleaseState{}, err
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: cluster.KubeconfigPath})
	if err != nil {
		return helm.ReleaseState{}, err
	}
	defer releases.Close()
	if remove {
		return helm.ReleaseState{}, releases.Uninstall(ctx, stored.Addon.Namespace, stored.Addon.ReleaseName, stored.Owner)
	}
	input := stored.Addon.Specification
	release := (helm.ChartInput{Repository: input.Repository, Name: input.Chart, Version: input.Version}).ReleaseInput()
	logs := "true"
	if input.Logs != nil && !*input.Logs {
		logs = "false"
	}
	release.Namespace, release.ReleaseName, release.Owner = stored.Addon.Namespace, stored.Addon.ReleaseName, stored.Owner
	release.Values, release.Upgrade, release.RecoverPending = input.Values, true, true
	release.Timeout = manager.runtime.rolloutTimeout
	release.ResourceLabels = map[string]string{"borq.run/addon-name": stored.Addon.Name, "borq.run/space-id": stableId(stored.Organization + "/" + stored.Space), "borq.run/organization": stored.Organization}
	release.PodLabels = map[string]string{"borq.run/logs": logs, "borq.run/metrics": "true"}
	release.PodSelector = input.PodSelector
	return releases.Reconcile(ctx, release)
}

func (manager Manager) finishAddonOperation(operation Operation, task addonTask, state helm.ReleaseState, reconcileErr error) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, record, err := loadDocument[addonRecord](ctx, manager.database, "addon", task.Key)
	if err != nil {
		return err
	}
	if stored.ActiveOperation != operation.ID {
		return nil
	}
	_, operationRecord, err := loadDocument[Operation](ctx, manager.database, "addon-operation", operation.ID)
	if err != nil {
		return err
	}
	stored.ActiveOperation = ""
	stored.Addon.State, stored.Addon.Message = "deployed", "Helm release deployed"
	operation.Status = "succeeded"
	if state.Revision > 0 {
		stored.Addon.Revision = state.Revision
	}
	if task.Remove {
		stored.Addon.State, stored.Addon.Message = "removed", "Helm release removed; chart CRDs retained"
	}
	if reconcileErr != nil {
		operation.Status, stored.Addon.State, stored.Addon.Message = "failed", "failed", reconcileErr.Error()
	}
	operation.Message, operation.UpdatedAt = stored.Addon.Message, time.Now().UTC()
	stored.Addon.UpdatedAt = operation.UpdatedAt
	addonWrite, err := documentWrite("addon", task.Key, stored, record.Revision)
	if err != nil {
		return err
	}
	operationWrite, err := documentWrite("addon-operation", operation.ID, operation, operationRecord.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{addonWrite, operationWrite})
}
