package reconciler

import (
	"testing"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/matryer/is"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCloudNativePGOperatorDefaultConfig verifies the operator handler pins
// the expected CloudNativePG release and runtime defaults.
func TestCloudNativePGOperatorDefaultConfig(t *testing.T) {
	is := is.New(t)

	cfg, ok := CloudNativePGOperatorHandler{}.DefaultConfig().(*CloudNativePGOperatorConfig)
	is.True(ok)
	is.Equal(cfg.Version, "1.29.0")
	is.Equal(cfg.Namespace, "borq-system")
	is.Equal(cfg.Image, "ghcr.io/cloudnative-pg/cloudnative-pg:1.29.0")
	is.Equal(cfg.CPURequest, "100m")
	is.Equal(cfg.MemoryRequest, "100Mi")
}

// TestCloudNativePGOperatorPlanIncludesInstallSet verifies the operator plan
// creates the expected code-owned install resources.
func TestCloudNativePGOperatorPlanIncludesInstallSet(t *testing.T) {
	is := is.New(t)

	plan := CloudNativePGOperatorHandler{}.Plan(&CloudNativePGOperatorConfig{})
	namesByType := map[string]map[string]bool{}
	for _, res := range plan.Desired {
		if namesByType[res.Type] == nil {
			namesByType[res.Type] = map[string]bool{}
		}
		namesByType[res.Type][res.Name] = true
	}

	is.Equal(len(namesByType["CustomResourceDefinition.apiextensions.k8s.io"]), 10)
	is.True(namesByType["Namespace"]["borq-system"])
	is.True(namesByType["ServiceAccount"]["cnpg-manager"])
	is.True(namesByType["ClusterRole.rbac.authorization.k8s.io"]["cnpg-manager"])
	is.True(namesByType["ClusterRoleBinding.rbac.authorization.k8s.io"]["cnpg-manager-rolebinding"])
	is.True(namesByType["ConfigMap"]["cnpg-default-monitoring"])
	is.True(namesByType["Service"]["cnpg-webhook-service"])
	is.True(namesByType["Deployment.apps"]["cnpg-controller-manager"])
	is.True(namesByType["MutatingWebhookConfiguration.admissionregistration.k8s.io"]["cnpg-mutating-webhook-configuration"])
	is.True(namesByType["ValidatingWebhookConfiguration.admissionregistration.k8s.io"]["cnpg-validating-webhook-configuration"])
	is.True(namesByType[cloudNativePGOperatorReadyType]["cnpg-controller-manager"])
}

// TestCloudNativePGOperatorReadinessRunsAfterDeployment verifies readiness is
// reconciled after the controller deployment exists.
func TestCloudNativePGOperatorReadinessRunsAfterDeployment(t *testing.T) {
	is := is.New(t)

	plan := CloudNativePGOperatorHandler{}.Plan(&CloudNativePGOperatorConfig{})
	deployIndex := indexOf(plan.ApplyOrder, "Deployment.apps")
	readyIndex := indexOf(plan.ApplyOrder, cloudNativePGOperatorReadyType)

	is.True(deployIndex >= 0)
	is.True(readyIndex >= 0)
	is.True(readyIndex > deployIndex)
}

// TestCloudNativePGOperatorPlanCarriesWebhookNamespace verifies the webhook
// resources retain the configured operator namespace so CA bundle patching
// reads from the right secret.
func TestCloudNativePGOperatorPlanCarriesWebhookNamespace(t *testing.T) {
	is := is.New(t)

	plan := CloudNativePGOperatorHandler{}.Plan(&CloudNativePGOperatorConfig{Namespace: "custom-operators"})

	webhookCount := 0
	for _, res := range plan.Desired {
		if !isWebhookConfigType(res.Type) {
			continue
		}
		webhookCount++
		is.Equal(res.Namespace, "custom-operators")
	}

	is.Equal(webhookCount, 2)
}

// TestBorqPostgresDefaultConfig verifies the borq postgres component defaults
// to the requested cluster name, database, owner, storage, and requests.
func TestBorqPostgresDefaultConfig(t *testing.T) {
	is := is.New(t)

	cfg, ok := BorqPostgresHandler{}.DefaultConfig().(*BorqPostgresConfig)
	is.True(ok)
	is.Equal(cfg.Name, "borq-postgres")
	is.Equal(cfg.Namespace, "borq-system")
	is.Equal(cfg.StorageSize, "5Gi")
	is.Equal(cfg.Database, "borq")
	is.Equal(cfg.Owner, "borq")
	is.Equal(cfg.CPURequest, "100m")
	is.Equal(cfg.MemoryRequest, "100Mi")
}

// TestBorqPostgresClusterDefaults verifies the handler builds a typed
// CloudNativePG Cluster using the SDK type.
func TestBorqPostgresClusterDefaults(t *testing.T) {
	is := is.New(t)

	plan := BorqPostgresHandler{}.Plan(&BorqPostgresConfig{})
	is.Equal(len(plan.Desired), 2)

	var cluster *apiv1.Cluster
	var pushSecret *unstructured.Unstructured
	for _, res := range plan.Desired {
		switch desired := res.Desired.(type) {
		case *apiv1.Cluster:
			cluster = desired
		case *unstructured.Unstructured:
			if res.Type == pushSecretType {
				pushSecret = desired
			}
		}
	}

	is.True(cluster != nil)
	is.Equal(cluster.Name, "borq-postgres")
	is.Equal(cluster.Namespace, "borq-system")
	is.Equal(cluster.Spec.Instances, 1)
	is.Equal(cluster.Spec.StorageConfiguration.Size, "5Gi")
	is.Equal(cluster.Spec.Bootstrap.InitDB.Database, "borq")
	is.Equal(cluster.Spec.Bootstrap.InitDB.Owner, "borq")
	is.Equal(cluster.Spec.Resources.Requests.Cpu().String(), "100m")
	is.Equal(cluster.Spec.Resources.Requests.Memory().String(), "100Mi")

	is.True(pushSecret != nil)
	is.Equal(pushSecret.GetName(), "borq-postgres-credentials")
	is.Equal(pushSecret.GetNamespace(), "borq-system")
}

// TestBorqPostgresClusterCustomConfig verifies user config can override the
// postgres cluster defaults.
func TestBorqPostgresClusterCustomConfig(t *testing.T) {
	is := is.New(t)

	cluster := borqPostgresCluster(BorqPostgresConfig{
		Name:          "custom",
		Namespace:     "data",
		Instances:     2,
		StorageSize:   "10Gi",
		Database:      "app",
		Owner:         "owner",
		CPURequest:    "250m",
		MemoryRequest: "512Mi",
		StorageClass:  "retain",
	})

	is.Equal(cluster.Name, "custom")
	is.Equal(cluster.Namespace, "data")
	is.Equal(cluster.Spec.Instances, 2)
	is.Equal(cluster.Spec.StorageConfiguration.Size, "10Gi")
	is.Equal(*cluster.Spec.StorageConfiguration.StorageClass, "retain")
	is.Equal(cluster.Spec.Bootstrap.InitDB.Database, "app")
	is.Equal(cluster.Spec.Bootstrap.InitDB.Owner, "owner")
	cpuRequest := cluster.Spec.Resources.Requests[corev1.ResourceCPU]
	memoryRequest := cluster.Spec.Resources.Requests[corev1.ResourceMemory]
	is.Equal(cpuRequest.String(), "250m")
	is.Equal(memoryRequest.String(), "512Mi")
}

// TestBorqPostgresPlanPushesApplicationCredentials verifies the postgres
// component publishes the generated application secret into OpenBao/Vault.
func TestBorqPostgresPlanPushesApplicationCredentials(t *testing.T) {
	is := is.New(t)

	plan := BorqPostgresHandler{}.Plan(&BorqPostgresConfig{
		Name:      "custom",
		Namespace: "data",
		Database:  "appdb",
	})

	var pushSecret *unstructured.Unstructured
	for _, res := range plan.Desired {
		if res.Type != pushSecretType {
			continue
		}
		var ok bool
		pushSecret, ok = res.Desired.(*unstructured.Unstructured)
		is.True(ok)
		break
	}

	if pushSecret == nil {
		t.Fatal("expected pushsecret in desired resources")
	}

	is.Equal(pushSecret.GetName(), "custom-credentials")
	is.Equal(pushSecret.GetNamespace(), "data")

	dataTo, found, err := unstructured.NestedSlice(pushSecret.Object, "spec", "dataTo")
	is.NoErr(err)
	is.True(found)
	is.Equal(len(dataTo), 1)

	firstEntry, ok := dataTo[0].(map[string]any)
	is.True(ok)
	is.Equal(firstEntry["remoteKey"], "data/appdb")
}

func indexOf(values []string, value string) int {
	for i, candidate := range values {
		if candidate == value {
			return i
		}
	}
	return -1
}
