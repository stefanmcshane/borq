package platform

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestCatalogDependencyOrder verifies stable display and upgrade order, required
// install dependencies, and that production never installs development RustFS.
func TestCatalogDependencyOrder(t *testing.T) {
	is := is.New(t)
	dev, err := normalizeSettings(Settings{})
	is.NoErr(err)
	is.Equal(dev.Namespace, "borq")
	is.Equal(dev.StorageClass, "local-path")
	is.Equal(componentNames(managementCatalog(dev, false)), "openbao,external-secrets,rustfs,prometheus,loki")
	is.Equal(componentNames(managementCatalog(dev, true)), "external-secrets,openbao,rustfs,loki,prometheus")
	prod, err := normalizeSettings(Settings{Profile: "prod"})
	is.NoErr(err)
	is.Equal(componentNames(managementCatalog(prod, false)), "openbao,external-secrets,prometheus,loki")
	is.Equal(componentNames(targetCatalog(false)), "cert-manager,external-secrets,traefik,alloy,snapshot-controller")
	is.Equal(componentNames(targetCatalog(true)), "cert-manager,external-secrets,alloy,traefik,snapshot-controller")
	for _, catalog := range [][]component{managementCatalog(dev, false), managementCatalog(dev, true), managementCatalog(prod, false), managementCatalog(prod, true), targetCatalog(false), targetCatalog(true)} {
		seen := map[string]bool{}
		for _, definition := range catalog {
			is.True(!seen[definition.Name])
			for _, dependency := range definition.DependsOn {
				is.True(seen[dependency]) // Every catalog is acyclic in both install and upgrade order.
			}
			seen[definition.Name] = true
		}
	}
	dependencies := map[string][]string{}
	for _, definition := range managementCatalog(dev, false) {
		dependencies[definition.Name] = definition.DependsOn
	}
	is.Equal(dependencies["rustfs"], []string{"openbao", "external-secrets"})
	is.Equal(dependencies["loki"], []string{"rustfs"})
	for _, definition := range managementCatalog(prod, false) {
		is.Equal(len(definition.DependsOn), 0)
	}
	is.Equal(targetCatalog(false)[3].DependsOn, []string{"external-secrets"})
}

// TestCatalogValuesResolveWithoutSecrets verifies every pinned component has
// valid Borq-owned values and telemetry credentials stay in Secret references.
func TestCatalogValuesResolveWithoutSecrets(t *testing.T) {
	is := is.New(t)
	for _, profile := range []string{"dev", "production"} {
		settings, err := normalizeSettings(Settings{Profile: profile, StorageClass: "borq-local", LokiURL: "https://management.test/loki/api/v1/push", PrometheusURL: "https://management.test/api/v1/write"})
		is.NoErr(err)
		definitions := append(managementCatalog(settings, false), targetCatalog(false)...)
		for _, definition := range definitions {
			values, err := renderComponentValues(definition, valuesInput{Settings: settings, ClusterID: "cluster-1"})
			is.NoErr(err)
			is.True(len(values) > 0)
			is.True(definition.Version != "")
			if definition.Name == "openbao" {
				service := values["server"].(map[string]any)["service"].(map[string]any)
				is.Equal(service["type"], "NodePort")
				is.Equal(service["nodePort"], float64(30200))
			}
		}
		alloy, err := renderComponentValues(targetCatalog(false)[3], valuesInput{Settings: settings, ClusterID: "cluster-1"})
		is.NoErr(err)
		content := alloy["alloy"].(map[string]any)["configMap"].(map[string]any)["content"].(string)
		is.True(strings.Contains(content, `bearer_token_file = "/var/run/borq-telemetry/TELEMETRY_TOKEN"`))
		is.True(!strings.Contains(content, `sys.env("TELEMETRY_TOKEN")`))
		volumes := alloy["controller"].(map[string]any)["volumes"].(map[string]any)["extra"].([]any)
		is.Equal(volumes[0].(map[string]any)["secret"].(map[string]any)["secretName"], "borq-telemetry")
		is.True(strings.Contains(content, `borq_cluster = "cluster-1"`))
		is.True(strings.Contains(content, `prometheus.scrape "cadvisor"`))
		is.True(strings.Contains(content, `loki.source.kubernetes_events "lifecycle"`))
		is.True(strings.Contains(content, `target_label = "borq_revision"`))
		is.True(strings.Contains(content, `target_label = "image"`))
	}
}

// TestPodHistoryChartScope verifies development renders a scheduled worker with
// namespace-only pod permissions, while production retains its existing policy.
func TestPodHistoryChartScope(t *testing.T) {
	is := is.New(t)
	for _, profile := range []string{"dev", "production"} {
		values, err := renderComponentValues(component{Name: "external-secrets", Values: "external-secrets.yaml"}, valuesInput{Settings: Settings{Profile: profile, Namespace: "custom-platform"}})
		is.NoErr(err)
		objects, present := values["extraObjects"].([]any)
		is.Equal(present, profile == "dev")
		if !present {
			continue
		}
		is.Equal(len(objects), 5)
		byKind := map[string]map[string]any{}
		for _, entry := range objects {
			object := entry.(map[string]any)
			byKind[object["kind"].(string)] = object
		}
		role := byKind["Role"]
		is.True(role != nil)
		rules := role["rules"].([]any)
		pods := rules[0].(map[string]any)
		is.Equal(pods["resources"], []any{"pods"})
		is.Equal(pods["verbs"], []any{"list", "delete"})
		for _, entry := range rules[1:] {
			is.Equal(entry.(map[string]any)["verbs"], []any{"get"})
		}
		subject := byKind["RoleBinding"]["subjects"].([]any)[0].(map[string]any)
		is.Equal(subject["namespace"], "custom-platform")
		spec := byKind["CronJob"]["spec"].(map[string]any)
		is.Equal(spec["schedule"], "*/5 * * * *")
		is.Equal(spec["concurrencyPolicy"], "Forbid")
		job := spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)
		pod := job["template"].(map[string]any)["spec"].(map[string]any)
		is.Equal(pod["serviceAccountName"], subject["name"])
		container := pod["containers"].([]any)[0].(map[string]any)
		retention := container["env"].([]any)[1].(map[string]any)
		is.Equal(retention["name"], "KEEP_FAILED_PODS")
		is.Equal(retention["value"], "5")
		is.True(byKind["ConfigMap"]["data"].(map[string]any)["cleanup.py"].(string) != "")
	}
}

// TestSecretBindingsUsePublicVaultAndValuesProperty verifies ESO reads the
// management endpoint and unwraps Borq's KV document rather than its metadata.
func TestSecretBindingsUsePublicVaultAndValuesProperty(t *testing.T) {
	is := is.New(t)
	store := secretStore("borq-openbao", "borq", "https://vault.test", "borq-target", "borq-openbao")
	vault := store.Object["spec"].(map[string]any)["provider"].(map[string]any)["vault"].(map[string]any)
	is.Equal(vault["server"], "https://vault.test")
	is.Equal(vault["path"], "borq-target")
	secret := externalSecret("borq-telemetry", "borq", "borq-openbao", "borq/platform/telemetry")
	extract := secret.Object["spec"].(map[string]any)["dataFrom"].([]any)[0].(map[string]any)["extract"].(map[string]any)
	is.Equal(extract["property"], "values")
	is.Equal(extract["key"], "borq/platform/telemetry")
}

// TestBootstrapAdminCredentialIsStable verifies repeated bootstrap preserves
// the original token and production recovery material instead of rotating them.
func TestBootstrapAdminCredentialIsStable(t *testing.T) {
	is := is.New(t)
	client := fake.NewClientset()
	connection := kubernetes.NewConnectionForTest(client)
	ctx := context.Background()
	is.NoErr(ensureAdminSecret(ctx, connection, "borq"))
	first, err := connection.Secrets().Get(ctx, "borq", "borq-openbao-admin")
	is.NoErr(err)
	is.Equal(len(first.Data["token"]), 64)
	first.Data["unsealKeys"] = `["recovery"]`
	is.NoErr(connection.Secrets().Apply(ctx, first))
	is.NoErr(ensureAdminSecret(ctx, connection, "borq"))
	second, err := connection.Secrets().Get(ctx, "borq", "borq-openbao-admin")
	is.NoErr(err)
	is.True(maps.Equal(first.Data, second.Data))
}

// TestComponentSecretGateRequiresExpectedKeys verifies components cannot start
// while ESO's Secret is absent or lacks a required credential.
func TestComponentSecretGateRequiresExpectedKeys(t *testing.T) {
	is := is.New(t)
	client := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "borq"}, Data: map[string][]byte{"TOKEN": []byte("value")}})
	connection := kubernetes.NewConnectionForTest(client)
	is.NoErr(waitForSecret(context.Background(), connection, "borq", "ready", []string{"TOKEN"}, time.Second))
	err := waitForSecret(context.Background(), connection, "borq", "ready", []string{"MISSING"}, time.Millisecond)
	is.True(err != nil)
	err = waitForSecret(context.Background(), connection, "borq", "absent", []string{"TOKEN"}, time.Millisecond)
	is.True(err != nil)
}

// TestManagementProfileMigrationGuard verifies storage-changing profile switches
// are rejected before any write, including explicit catalog upgrade requests.
func TestManagementProfileMigrationGuard(t *testing.T) {
	is := is.New(t)
	encoded, err := json.Marshal(Result{Profile: "dev", Namespace: "borq"})
	is.NoErr(err)
	client := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-management", Namespace: "borq"}, Data: map[string]string{"state.json": string(encoded)}})
	connection := kubernetes.NewConnectionForTest(client)
	ctx := context.Background()
	is.NoErr(checkManagementProfile(ctx, connection, Settings{Profile: "dev", Namespace: "borq"}))
	err = checkManagementProfile(ctx, connection, Settings{Profile: "production", Namespace: "borq"})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "data migration"))
	for _, action := range client.Actions() {
		is.Equal(action.GetVerb(), "get")
	}
	is.NoErr(checkManagementProfile(ctx, connection, Settings{Profile: "production", Namespace: "fresh"}))
}

func componentNames(components []component) string {
	names := make([]string, 0, len(components))
	for _, definition := range components {
		names = append(names, definition.Name)
	}
	return strings.Join(names, ",")
}
