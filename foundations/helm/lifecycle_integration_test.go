//go:build integration

package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/release/common"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestReleaseLifecycleIntegration verifies real Helm records, explicit upgrade
// gating, idempotent repeats, live rendered values, and active CRD schema upgrades
// without changing disabled dependencies or reevaluating imported conditions.
func TestReleaseLifecycleIntegration(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	is.NoErr(err)
	manager, err := NewReleaseManager(NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfigPath})
	is.NoErr(err)
	defer manager.Close()
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	is.NoErr(err)
	extensions, err := apiextensionsclient.NewForConfig(&configuration)
	is.NoErr(err)
	namespace := fmt.Sprintf("borq-helm-%d", time.Now().UnixNano())
	crdName := "widgets." + namespace + ".borq.run"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = extensions.ApiextensionsV1().CustomResourceDefinitions().Delete(cleanup, crdName, metav1.DeleteOptions{})
	})
	chartPath := t.TempDir()
	writeLifecycleChart(t, chartPath, namespace, "0.1.0", false)
	input := InstallOrUpgradeInput{Namespace: namespace, ReleaseName: "lifecycle", ChartRef: chartPath, Version: "v0.1.0", Values: map[string]any{"message": "first", "legacy": map[string]any{"enabled": false}}, Timeout: 30 * time.Second}
	first, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(first.Revision, 1)
	is.True(first.Changed)
	same, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(same.Revision, 1)
	is.True(!same.Changed)
	is.True(!same.UpgradeAvailable)
	foreign := input
	foreign.RepositoryUrl = "https://different.test/charts"
	provenance, err := manager.InspectWithValues(ctx, foreign)
	is.NoErr(err)
	is.True(provenance.DefinitionChanged)
	is.True(provenance.State.UpgradeAvailable)
	input.Values["message"] = "second"
	deferred, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(deferred.Revision, 1)
	is.True(deferred.UpgradeAvailable)
	live, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, "lifecycle", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(live.Data["message"], "first")
	writeLifecycleChart(t, chartPath, namespace, "0.1.1", true)
	input.Version = "0.1.1"
	input.Upgrade = true
	upgraded, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(upgraded.Revision, 2)
	is.True(upgraded.Changed)
	live, err = client.CoreV1().ConfigMaps(namespace).Get(ctx, "lifecycle", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(live.Data["message"], "second")
	_, err = client.CoreV1().ConfigMaps(namespace).Get(ctx, "lifecycle-feature", metav1.GetOptions{})
	is.NoErr(err)
	definition, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	is.NoErr(err)
	_, exists := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["updated"]
	is.True(exists)
	repeated, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(repeated.Revision, 2)
	is.True(!repeated.Changed)
	inspected, err := manager.Inspect(ctx, input)
	is.NoErr(err)
	is.Equal(inspected.Revision, 2)
	is.True(!inspected.UpgradeAvailable)
	private, err := manager.InspectWithValues(ctx, input)
	is.NoErr(err)
	is.Equal(private.Values["message"], "second")
	is.True(!private.DefinitionChanged)
	encoded, err := json.Marshal(private)
	is.NoErr(err)
	is.True(!strings.Contains(string(encoded), "second"))
	// Remove the release so the namespace can disappear immediately even when
	// test chart resources later gain foreground finalizers.
	configurationAction := action.NewConfiguration()
	is.NoErr(configurationAction.Init(manager.restClientGetter(namespace), namespace, "secrets"))
	storedRelease, err := configurationAction.Releases.Last("lifecycle")
	is.NoErr(err)
	failedRelease, err := releaseRecord(storedRelease)
	is.NoErr(err)
	failedRelease.Info.Status = common.StatusFailed
	is.NoErr(configurationAction.Releases.Update(failedRelease))
	failedInspection, err := manager.InspectWithValues(ctx, input)
	is.NoErr(err)
	is.Equal(failedInspection.State.Status, "failed")
	is.True(failedInspection.State.UpgradeAvailable)
	is.True(!failedInspection.DefinitionChanged)
	input.Upgrade = false
	recovered, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(recovered.Status, "deployed")
	is.Equal(recovered.Revision, 3)
	is.True(recovered.Changed)
	stable, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(stable.Revision, 3)
	is.True(!stable.Changed)
	storedRelease, err = configurationAction.Releases.Last("lifecycle")
	is.NoErr(err)
	interrupted, err := releaseRecord(storedRelease)
	is.NoErr(err)
	interrupted.Info.Status = common.StatusPendingUpgrade
	is.NoErr(configurationAction.Releases.Update(interrupted))
	blocked, err := manager.Reconcile(ctx, input)
	is.True(err != nil)
	is.Equal(blocked.Status, "pending-upgrade")
	is.Equal(blocked.Revision, 3)
	input.RecoverPending = true
	ownedLabels := interrupted.Labels
	interrupted.Labels = map[string]string{}
	is.NoErr(configurationAction.Releases.Update(interrupted))
	unowned, err := manager.Reconcile(ctx, input)
	is.True(err != nil)
	is.Equal(unowned.Status, "pending-upgrade")
	is.Equal(unowned.Revision, 3)
	interrupted.Labels = ownedLabels
	is.NoErr(configurationAction.Releases.Update(interrupted))
	input.ChartRef = filepath.Join(chartPath, "missing-chart")
	_, err = manager.Reconcile(ctx, input)
	is.True(err != nil)
	storedRelease, err = configurationAction.Releases.Last("lifecycle")
	is.NoErr(err)
	retryFailed, err := releaseRecord(storedRelease)
	is.NoErr(err)
	is.Equal(retryFailed.Version, 3)
	is.Equal(retryFailed.Info.Status, common.StatusFailed)
	is.True(strings.Contains(retryFailed.Info.Description, "interrupted"))
	input.ChartRef = chartPath
	resumed, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(resumed.Status, "deployed")
	is.Equal(resumed.Revision, 4)
	history, err := configurationAction.Releases.History("lifecycle")
	is.NoErr(err)
	is.Equal(len(history), 4)
	writeLifecycleChart(t, chartPath, namespace, "0.1.2", true)
	input.Version, input.Values, input.Upgrade = "0.1.2", nil, true
	retained, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(retained.Revision, 5)
	live, err = client.CoreV1().ConfigMaps(namespace).Get(ctx, "lifecycle", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(live.Data["message"], "second")
	storedRelease, err = configurationAction.Releases.Last("lifecycle")
	is.NoErr(err)
	failedAttempt, err := releaseRecord(storedRelease)
	is.NoErr(err)
	failedAttempt.Version = 6
	failedAttempt.Info.Status = common.StatusFailed
	failedAttempt.Config = map[string]any{"message": "failed", "legacy": map[string]any{"enabled": true}}
	is.NoErr(configurationAction.Releases.Create(failedAttempt))
	retried, err := manager.Reconcile(ctx, input)
	is.NoErr(err)
	is.Equal(retried.Revision, 7)
	live, err = client.CoreV1().ConfigMaps(namespace).Get(ctx, "lifecycle", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(live.Data["message"], "second")
	uninstall := action.NewUninstall(configurationAction)
	uninstall.WaitStrategy = kube.HookOnlyStrategy
	_, err = uninstall.Run("lifecycle")
	is.NoErr(err)
}

func writeLifecycleChart(t *testing.T, path, namespace, version string, updated bool) {
	t.Helper()
	for _, directory := range []string{"templates", "crds", "charts/legacy/crds", "charts/exporter", "charts/feature/templates"} {
		if err := os.MkdirAll(filepath.Join(path, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"Chart.yaml":                              "apiVersion: v2\nname: lifecycle\nversion: " + version + "\ndependencies:\n  - name: legacy\n    version: 0.1.0\n    condition: legacy.enabled\n  - name: exporter\n    version: 0.1.0\n    import-values:\n      - switches\n  - name: feature\n    version: 0.1.0\n    condition: feature.enabled\n",
		"values.yaml":                             "message: first\nlegacy:\n  enabled: true\n",
		"charts/legacy/Chart.yaml":                "apiVersion: v2\nname: legacy\nversion: 0.1.0\n",
		"charts/exporter/Chart.yaml":              "apiVersion: v2\nname: exporter\nversion: 0.1.0\n",
		"charts/exporter/values.yaml":             "exports:\n  switches:\n    feature:\n      enabled: false\n",
		"charts/feature/Chart.yaml":               "apiVersion: v2\nname: feature\nversion: 0.1.0\n",
		"charts/feature/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: lifecycle-feature\ndata:\n  message: enabled child\n",
		"templates/configmap.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: lifecycle\ndata:\n  message: {{ .Values.message | quote }}\n",
		"crds/widgets.yaml": fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.%s.borq.run
spec:
  group: %s.borq.run
  scope: Namespaced
  names:
    plural: widgets
    singular: widget
    kind: Widget
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                original:
                  type: string
`, namespace, namespace),
	}
	if updated {
		files["crds/widgets.yaml"] += "                updated:\n                  type: string\n"
	}
	files["charts/legacy/crds/widgets.yaml"] = strings.ReplaceAll(files["crds/widgets.yaml"], "name: v1\n", "name: v1alpha1\n")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
