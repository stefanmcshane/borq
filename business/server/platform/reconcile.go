package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func reconcileManagement(ctx context.Context, input ManagementInput) (Result, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return Result{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return Result{}, err
	}
	if err := checkManagementProfile(ctx, connection, settings); err != nil {
		return Result{}, err
	}
	if err := checkSharedComponentConflicts(ctx, connection, input.KubeconfigPath, settings); err != nil {
		return Result{}, err
	}
	var recovery []string
	if input.RecoverPending {
		recovery, err = sharedRecoveryComponents(ctx, connection, settings.Namespace)
		if err != nil {
			return Result{}, err
		}
		// Startup owns OpenBao reconciliation, so an interrupted initial Helm install can resume.
		recovery = append(recovery, "openbao")
	}
	if err := prepareNamespace(ctx, connection, SystemNamespace); err != nil {
		return Result{}, err
	}
	if err := prepareNamespace(ctx, connection, settings.Namespace); err != nil {
		return Result{}, err
	}
	if err := ensureAdminSecret(ctx, connection, settings.Namespace); err != nil {
		return Result{}, err
	}
	return reconcileComponents(ctx, componentRun{
		connection: connection, kubeconfigPath: input.KubeconfigPath, scope: "management", recoverComponents: recovery,
		values: valuesInput{Settings: settings}, definitions: managementCatalog(settings, input.Upgrade), upgrade: input.Upgrade, upgradeComponents: input.UpgradeComponents,
	})
}

// A shared release can be left pending by either management or a converged target.
// Read both durable runs before overwriting management state; target workers cannot
// recover it themselves until management startup has completed. Helm separately
// requires Borq provenance before recovering the pending release.
func sharedRecoveryComponents(ctx context.Context, connection kubernetes.Connection, managementNamespace string) ([]string, error) {
	interrupted := map[string]bool{}
	for _, location := range []struct{ namespace, scope string }{{managementNamespace, "management"}, {SystemNamespace, "target"}} {
		stored, err := connection.ConfigMaps().Get(ctx, location.namespace, "borq-platform-"+location.scope)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read shared controller recovery state: %w", err)
		}
		var previous Result
		if err := json.Unmarshal([]byte(stored.Data["state.json"]), &previous); err != nil {
			return nil, fmt.Errorf("read shared controller recovery state: %w", err)
		}
		for _, state := range previous.Components {
			namespace := state.Namespace
			if namespace == "" {
				namespace = previous.Namespace
			}
			if namespace == SystemNamespace && (state.Status == "installing" || strings.HasPrefix(state.Status, "pending-")) {
				interrupted[state.Name] = true
			}
		}
	}
	var recovery []string
	for _, definition := range append(sharedCatalog(), tailscaleComponent()) {
		if interrupted[definition.Name] {
			recovery = append(recovery, definition.Name)
		}
	}
	return recovery, nil
}

func checkManagementProfile(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	stored, err := connection.ConfigMaps().Get(ctx, settings.Namespace, "borq-platform-management")
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read management installation profile: %w", err)
	}
	var previous Result
	if err := json.Unmarshal([]byte(stored.Data["state.json"]), &previous); err != nil {
		return fmt.Errorf("read management installation profile: invalid saved component state")
	}
	if previous.Profile == "prod" {
		previous.Profile = "production"
	}
	if previous.Profile != settings.Profile {
		return fmt.Errorf("management profile change from %q to %q requires a separate installation or data migration", previous.Profile, settings.Profile)
	}
	return nil
}

func readManagement(ctx context.Context, input ManagementInput) (Result, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return Result{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return Result{}, err
	}
	return readComponents(ctx, connection, input.KubeconfigPath, valuesInput{Settings: settings}, managementCatalog(settings, false))
}

func readComponents(ctx context.Context, connection kubernetes.Connection, kubeconfigPath string, input valuesInput, definitions []component) (Result, error) {
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfigPath})
	if err != nil {
		return Result{}, err
	}
	defer releases.Close()
	result := Result{Profile: input.Profile, Namespace: input.Namespace, Components: []ComponentState{}}
	for _, definition := range definitions {
		namespace := definition.namespace(input.Namespace)
		if definition.Name == "tailscale-operator" {
			tag, err := networkOperatorTag(ctx, connection, input)
			if err != nil {
				return result, err
			}
			input.NetworkOperatorTag = tag
		}
		if definition.Name == "external-dns" {
			owner, err := networkOwnerID(ctx, connection)
			if err != nil {
				return result, err
			}
			input.NetworkOwner = owner
		}
		values, err := renderComponentValues(definition, input)
		if err != nil {
			return result, err
		}
		installed, err := releases.Inspect(ctx, helm.InstallOrUpgradeInput{Namespace: namespace, ReleaseName: definition.releaseName(), ChartRef: definition.Chart, RepositoryUrl: definition.Repository, Version: definition.Version, Values: values, PodLabels: componentPodLabels(), SharedCrds: definition.Name == "external-secrets" || definition.Name == "traefik"})
		if err != nil {
			return result, err
		}
		result.Components = append(result.Components, ComponentState{Name: definition.Name, Namespace: namespace, Chart: definition.Chart, Version: installed.Version, Revision: installed.Revision, Status: installed.Status, UpgradeAvailable: installed.UpgradeAvailable})
	}
	return result, nil
}

func reconcileTarget(ctx context.Context, input TargetInput) (Result, error) {
	settings, err := targetSettings(input)
	if err != nil {
		return Result{}, err
	}
	if input.ID == "" || input.SecretMount == "" || input.ClusterToken == "" {
		return Result{}, errors.New("target installation requires cluster identity and scoped vault credentials")
	}
	if err := CheckTargetOwnership(ctx, input); err != nil {
		return Result{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return Result{}, err
	}
	if settings.Network.Tailscale != nil {
		if err := checkNetworkControllerConflicts(ctx, connection, settings, false); err != nil {
			return Result{}, err
		}
	}
	if err := checkComponentConflicts(ctx, connection, input.KubeconfigPath, networkTargetCatalog(settings, input.Upgrade)); err != nil {
		return Result{}, err
	}
	if err := prepareNamespace(ctx, connection, settings.Namespace); err != nil {
		return Result{}, err
	}
	if err := claimTargetOwner(ctx, connection, settings.Namespace, input); err != nil {
		return Result{}, err
	}
	if err := connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao", Namespace: settings.Namespace, Labels: managedLabels(), Data: map[string]string{"token": input.ClusterToken}}); err != nil {
		return Result{}, fmt.Errorf("save scoped target vault credential: %w", err)
	}
	result, err := reconcileComponents(ctx, componentRun{
		connection: connection, kubeconfigPath: input.KubeconfigPath, scope: "target",
		values: valuesInput{Settings: settings, ClusterID: input.ID}, definitions: networkTargetCatalog(settings, input.Upgrade),
		upgrade: input.Upgrade, upgradeComponents: input.UpgradeComponents, recoverPending: input.RecoverPending, target: input,
	})
	return result, err
}

func targetSettings(input TargetInput) (Settings, error) {
	input.Settings.Namespace = SystemNamespace
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return Settings{}, err
	}
	if input.ID == "" {
		return Settings{}, errors.New("target cluster identity is required")
	}
	if settings.OpenBaoURL == "" {
		return Settings{}, errors.New("target installation requires a reachable management OpenBao URL")
	}
	if settings.TelemetryURL == "" && (settings.LokiURL == "" || settings.PrometheusURL == "") {
		return Settings{}, errors.New("target installation requires authenticated management telemetry URLs")
	}
	if settings.LokiURL == "" {
		settings.LokiURL = strings.TrimRight(settings.TelemetryURL, "/") + "/api/v1/ingest/" + input.ID + "/loki/api/v1/push"
	}
	if settings.PrometheusURL == "" {
		settings.PrometheusURL = strings.TrimRight(settings.TelemetryURL, "/") + "/api/v1/ingest/" + input.ID + "/api/v1/write"
	}
	return settings, nil
}

func reconcileComponent(ctx context.Context, releases helm.ReleaseManager, definition component, input valuesInput, upgrade, recoverPending bool) (ComponentState, error) {
	input.Namespace = definition.namespace(input.Namespace)
	state := ComponentState{Name: definition.Name, Namespace: input.Namespace, Chart: definition.Chart, Version: definition.Version, Status: "failed"}
	values, err := renderComponentValues(definition, input)
	if err != nil {
		return state, err
	}
	release := helm.InstallOrUpgradeInput{
		Namespace: input.Namespace, ReleaseName: definition.releaseName(), ChartRef: definition.Chart, RepositoryUrl: definition.Repository,
		Version: definition.Version, Values: values, Timeout: input.Timeout, Upgrade: upgrade,
		RecoverPending: recoverPending,
		PodLabels:      componentPodLabels(),
		SkipWait:       definition.Name == "openbao",
		SharedCrds:     definition.Name == "external-secrets" || definition.Name == "traefik",
	}
	if !upgrade && (definition.Name == "tailscale-operator" || definition.Name == "external-dns" || definition.Name == "traefik") {
		inspection, err := releases.InspectWithValues(ctx, release)
		if err != nil {
			return state, fmt.Errorf("inspect %s network settings: %w", definition.Name, err)
		}
		release.Upgrade, err = networkValuesUpgrade(definition, inspection, release.Values)
		if err != nil {
			return state, err
		}
	}
	installed, err := releases.Reconcile(ctx, release)
	if installed.Version != "" {
		state.Version = installed.Version
	}
	state.Revision = installed.Revision
	if installed.Status != "" {
		state.Status = installed.Status
	}
	state.Changed = installed.Changed
	state.UpgradeAvailable = installed.UpgradeAvailable
	if err != nil {
		return state, fmt.Errorf("reconcile %s: %w", definition.Name, err)
	}
	return state, nil
}

func prepareNamespace(ctx context.Context, connection kubernetes.Connection, namespace string) error {
	return connection.Namespaces().Apply(ctx, kubernetes.Namespace{Name: namespace, Labels: managedLabels()})
}

func managedLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq"}
}

func persistState(ctx context.Context, connection kubernetes.Connection, scope string, state Result) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return connection.ConfigMaps().Apply(ctx, kubernetes.ConfigMap{Name: "borq-platform-" + scope, Namespace: state.Namespace, Labels: managedLabels(), Data: map[string]string{"state.json": string(encoded)}})
}

func randomCredential() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate platform credential: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func prepareVault(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	vault, err := OpenManagementVault(ctx, connection, settings.Namespace)
	if err != nil {
		return err
	}
	if err := vault.WaitForActive(ctx, time.Second, settings.Timeout); err != nil {
		return err
	}
	if err := vault.EnsureKvV2(ctx, "secret"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		_, err = prepareManagementBackupKey(ctx, connection, settings.Namespace, vault)
		if !errors.Is(err, openbao.ErrUpgrading) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("management vault versioned storage did not become ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func prepareRustfs(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	vault, err := OpenManagementVault(ctx, connection, settings.Namespace)
	if err != nil {
		return err
	}
	if err := CheckRustfsEncryptionContinuity(ctx, connection, settings, vault); err != nil {
		return err
	}
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	if entry.Version == 0 {
		access, err := randomCredential()
		if err != nil {
			return err
		}
		secret, err := randomCredential()
		if err != nil {
			return err
		}
		if _, err := vault.WriteKv(ctx, "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": access, "RUSTFS_SECRET_KEY": secret}, CheckVersion: 0}); err != nil {
			return err
		}
	}
	if err := ReconcileRustfsEncryption(ctx, vault); err != nil {
		return err
	}
	store := secretStore("borq-management-openbao", settings.Namespace, "http://openbao."+settings.Namespace+".svc:8200", "secret", "borq-openbao-admin")
	if err := connection.Objects().Apply(ctx, store); err != nil {
		return err
	}
	if err := connection.Objects().Apply(ctx, externalSecret("borq-rustfs", settings.Namespace, "borq-management-openbao", "borq/platform/rustfs")); err != nil {
		return err
	}
	if err := waitForSecret(ctx, connection, settings.Namespace, "borq-rustfs", []string{"RUSTFS_ACCESS_KEY", "RUSTFS_SECRET_KEY", "RUSTFS_KMS_VAULT_TOKEN"}, settings.Timeout); err != nil {
		return err
	}
	return ReconcileRustfsEncryptionProjection(ctx, connection, settings)
}

func prepareTargetSecrets(ctx context.Context, connection kubernetes.Connection, input TargetInput, settings Settings) error {
	if err := connection.Objects().Apply(ctx, secretStore("borq-openbao", settings.Namespace, settings.OpenBaoURL, input.SecretMount, "borq-openbao")); err != nil {
		return err
	}
	if err := connection.Objects().Apply(ctx, externalSecret("borq-telemetry", settings.Namespace, "borq-openbao", "borq/platform/telemetry")); err != nil {
		return err
	}
	return waitForSecret(ctx, connection, settings.Namespace, "borq-telemetry", []string{"TELEMETRY_TOKEN"}, settings.Timeout)
}

func secretStore(name, namespace, address, mount, credential string) *unstructured.Unstructured {
	kind, controller := "ClusterSecretStore", SystemNamespace
	if name == "borq-management-openbao" {
		kind, controller = "SecretStore", ManagementNamespace
	}
	// Kubernetes' object API requires a pointer to attach resource versions.
	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": kind, "metadata": map[string]any{"name": name},
		"spec": map[string]any{"controller": controller, "provider": map[string]any{"vault": map[string]any{
			"server": address, "path": mount, "version": "v2", "auth": map[string]any{"tokenSecretRef": map[string]any{"name": credential, "namespace": namespace, "key": "token"}},
		}}},
	}}
	if kind == "SecretStore" {
		store.SetNamespace(namespace)
		unstructured.RemoveNestedField(store.Object, "spec", "provider", "vault", "auth", "tokenSecretRef", "namespace")
	}
	return store
}

func externalSecret(name, namespace, store, path string) *unstructured.Unstructured {
	kind := "ClusterSecretStore"
	if store == "borq-management-openbao" {
		kind = "SecretStore"
	}
	labels := make(map[string]any)
	for key, value := range managedLabels() {
		labels[key] = value
	}
	// Kubernetes' object API requires a pointer to attach resource versions.
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"refreshInterval": "1m", "secretStoreRef": map[string]any{"kind": kind, "name": store},
			"dataFrom": []any{map[string]any{"extract": map[string]any{"key": path, "property": "values"}}},
			"target":   map[string]any{"name": name, "creationPolicy": "Owner", "template": map[string]any{"metadata": map[string]any{"labels": labels}}},
		},
	}}
}

func waitForSecret(ctx context.Context, connection kubernetes.Connection, namespace, name string, keys []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		secret, err := connection.Secrets().Get(ctx, namespace, name)
		if err == nil {
			ready := true
			for _, key := range keys {
				if secret.Data[key] == "" {
					ready = false
				}
			}
			if ready {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for component Secret %s/%s: %w", namespace, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Collection labels are added after rendering so every chart, including hooks
// and charts without podLabels values, follows the same selection policy.
func componentPodLabels() map[string]string {
	return map[string]string{"borq.run/logs": "true", "borq.run/metrics": "true"}
}
