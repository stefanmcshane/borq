package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestDiffTargetReadOnlyActions exercises actual Helm Secret encoding and reads
// with selected kubecontext, missing releases, failed releases and upgrade gates.
func TestDiffTargetReadOnlyActions(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
			assert := is.New(t)
			input := diffTestInput()
			input.Upgrade = upgrade
			var records []releasev1.Release
			for _, definition := range targetCatalog(false)[1:] {
				record := diffTestRelease(t, definition, input)
				switch definition.Name {
				case "external-secrets":
					record.Info.Status = common.StatusFailed
				case "traefik":
					record.Chart.Metadata.Version = "41.0.0"
					record.Config["apiKey"] = "private-api-key"
					record.Config["deployment"].(map[string]any)["replicas"] = float64(2)
				}
				records = append(records, record)
			}
			path, requests := diffTestServer(t, diffTestInventory{releases: records})
			input.KubeconfigPath = path
			result, err := DiffTarget(context.Background(), input)
			assert.NoErr(err)
			assert.Equal(len(result.Components), 4)
			changes := map[string]ComponentChange{}
			for _, change := range result.Components {
				changes[change.Name] = change
				assert.True(change.DesiredVersion != "")
				assert.Equal(change.Reason, "")
			}
			assert.Equal(changes["cert-manager"].Action, "install")
			assert.Equal(changes["cert-manager"].InstalledVersion, "")
			assert.Equal(changes["cert-manager"].Revision, 0)
			assert.Equal(changes["external-secrets"].Action, "repair")
			assert.True(!changes["external-secrets"].UpgradeAvailable)
			assert.Equal(changes["external-secrets"].ValuesDiff, "")
			assert.Equal(changes["alloy"].Action, "none")
			assert.Equal(changes["alloy"].ValuesDiff, "")
			assert.Equal(changes["alloy"].InstalledVersion, changes["alloy"].DesiredVersion)
			assert.Equal(changes["alloy"].Revision, 3)
			expectedAction := "none"
			if upgrade {
				expectedAction = "upgrade"
			}
			assert.Equal(changes["traefik"].Action, expectedAction)
			assert.True(changes["traefik"].UpgradeAvailable)
			assert.True(strings.Contains(changes["traefik"].ValuesDiff, "replicas: 2"))
			assert.True(strings.Contains(changes["traefik"].ValuesDiff, "[redacted]"))
			assert.True(!strings.Contains(changes["traefik"].ValuesDiff, "private-api-key"))
			assert.Equal(<-requests, "GET /api/v1/namespaces/borq/configmaps/borq-platform-owner")
			for len(requests) > 0 {
				assert.True(strings.HasPrefix(<-requests, "GET "))
			}
		})
	}
}

// TestDiffTargetSparseUpgrade preserves unrelated existing release versions.
func TestDiffTargetSparseUpgrade(t *testing.T) {
	assert := is.New(t)
	input := diffTestInput()
	input.Upgrade = true
	input.UpgradeComponents = []string{"alloy"}
	var records []releasev1.Release
	for _, definition := range targetCatalog(false) {
		record := diffTestRelease(t, definition, input)
		record.Chart.Metadata.Version = "0.0.1"
		if definition.Name == "traefik" {
			record.Info.Status = common.StatusFailed
		}
		records = append(records, record)
	}
	path, _ := diffTestServer(t, diffTestInventory{releases: records})
	input.KubeconfigPath = path
	result, err := DiffTarget(context.Background(), input)
	assert.NoErr(err)
	for _, change := range result.Components {
		expected := "none"
		if change.Name == "alloy" {
			expected = "upgrade"
		}
		assert.Equal(change.Action, expected)
		assert.True(change.UpgradeAvailable)
	}
}

// TestSparseComponentLeavesFailedReleaseUntouched prevents implicit repair of an unmentioned release.
func TestSparseComponentLeavesFailedReleaseUntouched(t *testing.T) {
	assert := is.New(t)
	input := diffTestInput()
	definition := targetCatalog(false)[2]
	assert.Equal(definition.Name, "traefik")
	record := diffTestRelease(t, definition, input)
	record.Info.Status = common.StatusFailed
	record.Chart.Metadata.Version = "0.0.1"
	path, requests := diffTestServer(t, diffTestInventory{releases: []releasev1.Release{record}})
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path, Context: input.Context}, logger.Logger{})
	assert.NoErr(err)
	settings, err := targetSettings(input)
	assert.NoErr(err)
	state, err := (componentRun{connection: connection, kubeconfigPath: path, values: valuesInput{Settings: settings, ClusterID: input.ID}, upgrade: true, upgradeComponents: []string{"alloy"}}).reconcile(context.Background(), definition)
	assert.NoErr(err)
	assert.Equal(state.Status, "failed")
	assert.Equal(state.Version, "0.0.1")
	assert.Equal(state.Revision, record.Version)
	assert.True(len(requests) > 0)
	for len(requests) > 0 {
		assert.True(strings.HasPrefix(<-requests, "GET "))
	}
}

// TestDiffTargetOwnershipStopsAllInventory proves an ownership conflict prevents
// even reading other Helm credentials, not just subsequent installation writes.
func TestDiffTargetOwnershipStopsAllInventory(t *testing.T) {
	assert := is.New(t)
	path, requests := diffTestServer(t, diffTestInventory{owner: map[string]string{"clusterId": "foreign", "organization": "other", "name": "dev"}})
	input := diffTestInput()
	input.KubeconfigPath = path
	_, err := DiffTarget(context.Background(), input)
	assert.True(errors.Is(err, ErrTargetOwnership))
	assert.Equal(len(requests), 1)
	assert.Equal(<-requests, "GET /api/v1/namespaces/borq/configmaps/borq-platform-owner")
}

// TestDiffTargetForeignInstallations detects alternate Helm releases and
// controllers installed without Helm, while ignoring unrelated monitoring.
func TestDiffTargetForeignInstallations(t *testing.T) {
	assert := is.New(t)
	input := diffTestInput()
	var records []releasev1.Release
	for _, definition := range targetCatalog(false) {
		if definition.Name != "external-secrets" && definition.Name != "traefik" {
			continue
		}
		record := diffTestRelease(t, definition, input)
		record.Name = "existing-" + definition.Name
		if definition.Name == "traefik" {
			record.Namespace = "kube-system"
		}
		records = append(records, record)
	}
	// A monitoring release is unrelated to the target catalog.
	records = append(records, releasev1.Release{Name: "monitoring", Namespace: "monitoring", Version: 1, Info: &releasev1.Info{Status: common.StatusDeployed}, Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{Name: "prometheus", Version: "1.0.0"}}})
	path, requests := diffTestServer(t, diffTestInventory{
		releases:    records,
		deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-system", Labels: map[string]string{"app.kubernetes.io/name": "cert-manager"}}}},
		daemonSets:  []appsv1.DaemonSet{{ObjectMeta: metav1.ObjectMeta{Name: "collector", Namespace: "observability", Labels: map[string]string{"app.kubernetes.io/name": "alloy"}}}},
	})
	input.KubeconfigPath = path
	result, err := DiffTarget(context.Background(), input)
	assert.NoErr(err)
	assert.Equal(len(result.Components), 4)
	for _, change := range result.Components {
		assert.Equal(change.Action, "conflict")
		assert.True(strings.Contains(change.Reason, change.Name) || strings.Contains(change.Reason, "observability/collector"))
		assert.True(!strings.Contains(change.Reason, "prometheus"))
	}
	for len(requests) > 0 {
		assert.True(strings.HasPrefix(<-requests, "GET "))
	}
}

// TestDiffTargetOwnControllers permits a repeat reconcile of Borq's expected
// release and rejects an unlabelled-for-release controller even in borq.
func TestDiffTargetOwnControllers(t *testing.T) {
	assert := is.New(t)
	input := diffTestInput()
	path, _ := diffTestServer(t, diffTestInventory{
		deployments:  []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "alloy", Namespace: "borq", Labels: map[string]string{"app.kubernetes.io/name": "alloy", "app.kubernetes.io/instance": "alloy"}}}},
		statefulSets: []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "manual-eso", Namespace: "borq", Labels: map[string]string{"app.kubernetes.io/name": "external-secrets"}}}},
	})
	input.KubeconfigPath = path
	result, err := DiffTarget(context.Background(), input)
	assert.NoErr(err)
	for _, change := range result.Components {
		if change.Name == "external-secrets" {
			assert.Equal(change.Action, "conflict")
			assert.True(strings.Contains(change.Reason, "StatefulSet borq/manual-eso"))
			continue
		}
		assert.Equal(change.Action, "install")
	}
}

// TestComponentValuesDiffRedactsExistingSecrets prevents private manually-added
// fields and embedded Alloy source from appearing in public diff responses.
func TestComponentValuesDiffRedactsExistingSecrets(t *testing.T) {
	assert := is.New(t)
	desired := map[string]any{"alloy": map[string]any{"configMap": map[string]any{"content": "bearer_token = sys.env(\"TELEMETRY_TOKEN\")"}, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "borq-telemetry"}}}}, "replicas": float64(1)}
	installed := map[string]any{"alloy": map[string]any{"configMap": map[string]any{"content": "bearer_token = \"inline-private-token\""}, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "borq-telemetry"}}}}, "extraEnv": []any{map[string]any{"name": "SENSITIVE", "value": "private-env"}}, "replicas": float64(2), "password": "private-password"}
	desired["volume"] = map[string]any{"secret": map[string]any{"secretName": "borq-telemetry"}}
	installed["volume"] = map[string]any{"secret": map[string]any{"secretName": "borq-telemetry", "unexpected": "private-volume-value"}}

	diff, err := componentValuesDiff(installed, desired)
	assert.NoErr(err)
	for _, secret := range []string{"inline-private-token", "private-env", "private-password", "private-volume-value"} {
		assert.True(!strings.Contains(diff, secret))
	}
	assert.True(strings.Contains(diff, "[redacted existing configuration]"))
	assert.True(strings.Contains(diff, "borq-telemetry"))
	assert.True(strings.Contains(diff, "TELEMETRY_TOKEN"))
	again, err := componentValuesDiff(installed, desired)
	assert.NoErr(err)
	assert.Equal(again, diff)
	unchanged, err := componentValuesDiff(map[string]any{"replicas": float64(1)}, map[string]any{"replicas": 1})
	assert.NoErr(err)
	assert.Equal(unchanged, "")
}

func diffTestInput() TargetInput {
	return TargetInput{Context: "selected", ID: "target-id", Organization: "team", Name: "dev", Settings: Settings{OpenBaoURL: "https://unreachable-vault.test", TelemetryURL: "https://unreachable-management.test"}}
}

func diffTestRelease(t *testing.T, definition component, input TargetInput) releasev1.Release {
	t.Helper()
	assert := is.New(t)
	settings, err := targetSettings(input)
	assert.NoErr(err)
	values, err := renderComponentValues(definition, valuesInput{Settings: settings, ClusterID: input.ID})
	assert.NoErr(err)
	// Persist the chart provenance label written by the real SDK reconciliation.
	encoded, err := json.Marshal(struct {
		Chart, Repository, Version, Owner      string
		ResourceLabels, PodLabels, PodSelector map[string]string
	}{definition.Chart, definition.Repository, definition.Version, "", nil, componentPodLabels(), nil})
	assert.NoErr(err)
	digest := sha256.Sum256(encoded)
	// Helm uses pointers for chart metadata and release status.
	return releasev1.Release{Name: definition.Name, Namespace: "borq", Version: 3, Config: values, Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{Name: definition.Chart, Version: definition.Version}}, Info: &releasev1.Info{Status: common.StatusDeployed}, Labels: map[string]string{"borq.run/definition": hex.EncodeToString(digest[:])[:32]}}
}

type diffTestInventory struct {
	releases     []releasev1.Release
	owner        map[string]string
	deployments  []appsv1.Deployment
	daemonSets   []appsv1.DaemonSet
	statefulSets []appsv1.StatefulSet
}

func diffTestServer(t *testing.T, inventory diffTestInventory) (string, chan string) {
	t.Helper()
	assert := is.New(t)
	client := fake.NewSimpleClientset()
	for _, record := range inventory.releases {
		store := driver.NewSecrets(client.CoreV1().Secrets(record.Namespace))
		// The official Helm storage driver requires a release pointer.
		assert.NoErr(store.Create(fmt.Sprintf("sh.helm.release.v1.%s.v%d", record.Name, record.Version), &record))
	}
	requests := make(chan string, 100)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Method + " " + request.URL.Path
		if request.Method != http.MethodGet {
			t.Errorf("diff attempted mutation: %s %s", request.Method, request.URL.Path)
			http.Error(writer, "mutation", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/api/v1/namespaces/borq/configmaps/borq-platform-owner":
			if inventory.owner == nil {
				writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: metav1.StatusReasonNotFound, Code: 404})
				return
			}
			writeOwnershipResponse(writer, http.StatusOK, corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-owner", Namespace: "borq"}, Data: inventory.owner})
		case "/api/v1/secrets", "/api/v1/namespaces/borq/secrets":
			namespace := ""
			if strings.Contains(request.URL.Path, "/namespaces/") {
				namespace = "borq"
			}
			secrets, err := client.CoreV1().Secrets(namespace).List(request.Context(), metav1.ListOptions{LabelSelector: request.URL.Query().Get("labelSelector")})
			if err != nil {
				t.Errorf("list fixture releases: %v", err)
				http.Error(writer, "fixture", 500)
				return
			}
			secrets.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}
			writeOwnershipResponse(writer, http.StatusOK, secrets)
		case "/apis/apps/v1/deployments":
			writeOwnershipResponse(writer, http.StatusOK, appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, Items: inventory.deployments})
		case "/apis/apps/v1/daemonsets":
			writeOwnershipResponse(writer, http.StatusOK, appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, Items: inventory.daemonSets})
		case "/apis/apps/v1/statefulsets":
			writeOwnershipResponse(writer, http.StatusOK, appsv1.StatefulSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSetList"}, Items: inventory.statefulSets})
		default:
			t.Errorf("unexpected diff read: %s", request.URL.Path)
			http.Error(writer, "unexpected", 500)
		}
	}))
	t.Cleanup(server.Close)
	_, path := ownershipTestConnection(t, server.URL)
	return path, requests
}
