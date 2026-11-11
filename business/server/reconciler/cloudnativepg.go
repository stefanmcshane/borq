package reconciler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/releases"
	"github.com/stefanmcshane/borq/foundations/kubernetes"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	cloudNativePGOperatorReadyType = "CloudNativePGOperatorReady"
	cloudNativePGClusterType       = "CloudNativePGCluster"
	cloudNativePGVersion           = "1.29.0"
	cloudNativePGNamespace         = "borq-system"
	cloudNativePGDeploymentName    = "cnpg-controller-manager"
	cloudNativePGServiceAccount    = "cnpg-manager"
	cloudNativePGWebhookService    = "cnpg-webhook-service"
	cloudNativePGMonitoringName    = "cnpg-default-monitoring"
)

// CloudNativePGOperatorConfig describes the CloudNativePG operator install.
type CloudNativePGOperatorConfig struct {
	Version       string `json:"version"`
	Namespace     string `json:"namespace"`
	Image         string `json:"image"`
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
	CPULimit      string `json:"cpuLimit"`
	MemoryLimit   string `json:"memoryLimit"`
}

// BorqPostgresConfig describes the default borq PostgreSQL cluster.
type BorqPostgresConfig struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	Instances     int    `json:"instances"`
	StorageSize   string `json:"storageSize"`
	Database      string `json:"database"`
	Owner         string `json:"owner"`
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
	StorageClass  string `json:"storageClass"`
}

type cloudNativePGOperatorReady struct {
	Available bool `json:"available"`
}

// CloudNativePGOperatorHandler implements ComponentHandler for the CNPG operator.
type CloudNativePGOperatorHandler struct{}

func (CloudNativePGOperatorHandler) Name() string { return "cloudnativepg-operator" }

func (CloudNativePGOperatorHandler) DefaultConfig() any {
	return &CloudNativePGOperatorConfig{
		Version:       cloudNativePGVersion,
		Namespace:     cloudNativePGNamespace,
		Image:         "ghcr.io/cloudnative-pg/cloudnative-pg:1.29.0",
		CPURequest:    "100m",
		MemoryRequest: "100Mi",
		CPULimit:      "100m",
		MemoryLimit:   "200Mi",
	}
}

func (handler CloudNativePGOperatorHandler) Plan(config any) Plan {
	cfg := config.(*CloudNativePGOperatorConfig)
	defaultCloudNativePGOperatorConfig(cfg)

	resources := cloudNativePGOperatorResources(*cfg)
	return Plan{
		ApplyOrder: cloudNativePGOperatorApplyOrder(resources),
		Desired:    resources,
		Current:    cloudNativePGCurrentFunc(resources),
	}
}

func (handler CloudNativePGOperatorHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == cloudNativePGOperatorReadyType {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return waitForCloudNativePGOperatorAvailable(waitCtx, conn, res.Namespace)
	}

	if isWebhookConfigType(res.Type) {
		return applyWebhookConfigWithCABundle(ctx, conn, res)
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("cloudnativepg operator: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Apply(ctx, obj)
}

func isWebhookConfigType(resType string) bool {
	return resType == "MutatingWebhookConfiguration.admissionregistration.k8s.io" ||
		resType == "ValidatingWebhookConfiguration.admissionregistration.k8s.io"
}

func applyWebhookConfigWithCABundle(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	desired, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("cloudnativepg operator: unsupported resource %s/%s", res.Type, res.Name)
	}

	caBundle, err := getCNPGWebhookCABundle(ctx, conn, res.Namespace)
	if err != nil {
		return err
	}
	if caBundle != nil {
		setCABundleOnWebhooks(desired, caBundle)
		return conn.Objects().Apply(ctx, desired)
	}

	live, err := conn.Objects().Get(ctx, desired)
	if apierrors.IsNotFound(err) {
		return conn.Objects().Apply(ctx, desired)
	}
	if err != nil {
		return err
	}
	setCABundleFromLive(desired, live)
	return conn.Objects().Apply(ctx, desired)
}

func setCABundleOnWebhooks(desired ctrlclient.Object, caBundle []byte) {
	switch desiredConfig := desired.(type) {
	case *admissionv1.MutatingWebhookConfiguration:
		for i := range desiredConfig.Webhooks {
			desiredConfig.Webhooks[i].ClientConfig.CABundle = caBundle
		}
	case *admissionv1.ValidatingWebhookConfiguration:
		for i := range desiredConfig.Webhooks {
			desiredConfig.Webhooks[i].ClientConfig.CABundle = caBundle
		}
	}
}

func setCABundleFromLive(desired ctrlclient.Object, live ctrlclient.Object) {
	switch liveConfig := live.(type) {
	case *admissionv1.MutatingWebhookConfiguration:
		desiredConfig, ok := desired.(*admissionv1.MutatingWebhookConfiguration)
		if !ok {
			return
		}
		for i := range desiredConfig.Webhooks {
			if i < len(liveConfig.Webhooks) && len(liveConfig.Webhooks[i].ClientConfig.CABundle) > 0 {
				desiredConfig.Webhooks[i].ClientConfig.CABundle = liveConfig.Webhooks[i].ClientConfig.CABundle
			}
		}
	case *admissionv1.ValidatingWebhookConfiguration:
		desiredConfig, ok := desired.(*admissionv1.ValidatingWebhookConfiguration)
		if !ok {
			return
		}
		for i := range desiredConfig.Webhooks {
			if i < len(liveConfig.Webhooks) && len(liveConfig.Webhooks[i].ClientConfig.CABundle) > 0 {
				desiredConfig.Webhooks[i].ClientConfig.CABundle = liveConfig.Webhooks[i].ClientConfig.CABundle
			}
		}
	}
}

func getCNPGWebhookCABundle(ctx context.Context, conn kubernetes.Connection, namespace string) ([]byte, error) {
	if strings.TrimSpace(namespace) == "" {
		namespace = cloudNativePGNamespace
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgSecretName,
			Namespace: namespace,
		},
	}
	live, err := conn.Objects().Get(ctx, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	liveSecret, ok := live.(*corev1.Secret)
	if !ok {
		return nil, nil
	}
	return liveSecret.Data["ca.crt"], nil
}

func (handler CloudNativePGOperatorHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == cloudNativePGOperatorReadyType {
		return nil
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		obj, ok = res.Actual.(ctrlclient.Object)
	}
	if !ok {
		return fmt.Errorf("cloudnativepg operator: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Delete(ctx, obj)
}

// BorqPostgresHandler implements ComponentHandler for the default borq database.
type BorqPostgresHandler struct{}

func (BorqPostgresHandler) Name() string { return "borq-postgres" }

func (BorqPostgresHandler) DefaultConfig() any {
	return &BorqPostgresConfig{
		Name:          "borq-postgres",
		Namespace:     "borq-mgmt",
		Instances:     1,
		StorageSize:   "5Gi",
		Database:      "borq",
		Owner:         "borq",
		CPURequest:    "100m",
		MemoryRequest: "100Mi",
	}
}

func (handler BorqPostgresHandler) Plan(config any) Plan {
	cfg := config.(*BorqPostgresConfig)
	defaultBorqPostgresConfig(cfg)

	cluster := borqPostgresCluster(*cfg)
	pushSecret := borqPostgresCredentialsPushSecret(*cfg)
	resources := []Resource{
		objectResource(cloudNativePGClusterType, cluster),
		objectResource(pushSecretType, pushSecret),
	}
	return Plan{
		ApplyOrder: []string{cloudNativePGClusterType, pushSecretType},
		Desired:    resources,
		Current:    cloudNativePGCurrentFunc(resources),
	}
}

func (handler BorqPostgresHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == cloudNativePGClusterType {
		if err := ensureComponentNamespace(ctx, conn, res.Namespace); err != nil {
			return err
		}
	}
	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("borq postgres: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Apply(ctx, obj)
}

func (handler BorqPostgresHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		obj, ok = res.Actual.(ctrlclient.Object)
	}
	if !ok {
		return fmt.Errorf("borq postgres: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Delete(ctx, obj)
}

func defaultCloudNativePGOperatorConfig(cfg *CloudNativePGOperatorConfig) {
	defaults := CloudNativePGOperatorHandler{}.DefaultConfig().(*CloudNativePGOperatorConfig)
	if strings.TrimSpace(cfg.Version) == "" {
		cfg.Version = defaults.Version
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = defaults.Namespace
	}
	if strings.TrimSpace(cfg.Image) == "" {
		cfg.Image = "ghcr.io/cloudnative-pg/cloudnative-pg:" + cfg.Version
	}
	if strings.TrimSpace(cfg.CPURequest) == "" {
		cfg.CPURequest = defaults.CPURequest
	}
	if strings.TrimSpace(cfg.MemoryRequest) == "" {
		cfg.MemoryRequest = defaults.MemoryRequest
	}
	if strings.TrimSpace(cfg.CPULimit) == "" {
		cfg.CPULimit = defaults.CPULimit
	}
	if strings.TrimSpace(cfg.MemoryLimit) == "" {
		cfg.MemoryLimit = defaults.MemoryLimit
	}
}

func defaultBorqPostgresConfig(cfg *BorqPostgresConfig) {
	defaults := BorqPostgresHandler{}.DefaultConfig().(*BorqPostgresConfig)
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = defaults.Name
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = defaults.Namespace
	}
	if cfg.Instances <= 0 {
		cfg.Instances = defaults.Instances
	}
	if strings.TrimSpace(cfg.StorageSize) == "" {
		cfg.StorageSize = defaults.StorageSize
	}
	if strings.TrimSpace(cfg.Database) == "" {
		cfg.Database = defaults.Database
	}
	if strings.TrimSpace(cfg.Owner) == "" {
		cfg.Owner = defaults.Owner
	}
	if strings.TrimSpace(cfg.CPURequest) == "" {
		cfg.CPURequest = defaults.CPURequest
	}
	if strings.TrimSpace(cfg.MemoryRequest) == "" {
		cfg.MemoryRequest = defaults.MemoryRequest
	}
}

func cloudNativePGOperatorResources(cfg CloudNativePGOperatorConfig) []Resource {
	objects := make([]ctrlclient.Object, 0)
	for _, crd := range mustCloudNativePGCRDs(cfg.Version) {
		crd.Labels = mergeStringMaps(crd.Labels, cloudNativePGLabels("crd"))
		objects = append(objects, crd.DeepCopy())
	}

	objects = append(objects,
		cloudNativePGNamespaceObject(cfg),
		cloudNativePGServiceAccountObject(cfg),
		cloudNativePGManagerClusterRole(cfg),
		cloudNativePGDatabaseEditorClusterRole(),
		cloudNativePGDatabaseViewerClusterRole(),
		cloudNativePGPublicationEditorClusterRole(),
		cloudNativePGPublicationViewerClusterRole(),
		cloudNativePGSubscriptionEditorClusterRole(),
		cloudNativePGSubscriptionViewerClusterRole(),
		cloudNativePGClusterRoleBinding(cfg),
		cloudNativePGMonitoringConfigMap(cfg),
		cloudNativePGWebhookServiceObject(cfg),
		cloudNativePGDeploymentObject(cfg),
		cloudNativePGMutatingWebhookConfiguration(cfg),
		cloudNativePGValidatingWebhookConfiguration(cfg),
	)

	resources := make([]Resource, 0, len(objects)+1)
	for _, obj := range objects {
		resource := objectResource(objectResourceType(obj), obj)
		if isWebhookConfigType(resource.Type) {
			resource.Namespace = cfg.Namespace
		}
		resources = append(resources, resource)
	}
	resources = append(resources, Resource{
		Type:      cloudNativePGOperatorReadyType,
		Namespace: cfg.Namespace,
		Name:      cloudNativePGDeploymentName,
		Desired:   cloudNativePGOperatorReady{Available: true},
	})
	return resources
}

func cloudNativePGOperatorApplyOrder(resources []Resource) []string {
	seen := make(map[string]bool)
	order := make([]string, 0, len(resources))
	for _, res := range resources {
		if seen[res.Type] {
			continue
		}
		seen[res.Type] = true
		order = append(order, res.Type)
	}
	return order
}

func cloudNativePGCurrentFunc(desired []Resource) func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
	return func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
		resources := make([]Resource, 0, len(desired))
		for _, res := range desired {
			if res.Type == cloudNativePGOperatorReadyType {
				resources = append(resources, cloudNativePGOperatorReadyResource(ctx, conn, res))
				continue
			}

			obj, ok := res.Desired.(ctrlclient.Object)
			if !ok {
				continue
			}
			live, err := conn.Objects().Get(ctx, obj)
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			resources = append(resources, Resource{
				Type:      res.Type,
				Namespace: res.Namespace,
				Name:      res.Name,
				Actual:    objectForDiff(live),
				Desired:   res.Desired,
			})
		}
		return resources, nil
	}
}

func cloudNativePGOperatorReadyResource(ctx context.Context, conn kubernetes.Connection, res Resource) Resource {
	ready := conn.Deployments().IsAvailable(ctx, res.Namespace, cloudNativePGDeploymentName)
	return Resource{
		Type:      res.Type,
		Namespace: res.Namespace,
		Name:      res.Name,
		Actual:    cloudNativePGOperatorReady{Available: ready},
	}
}

func waitForCloudNativePGOperatorAvailable(ctx context.Context, conn kubernetes.Connection, namespace string) error {
	if err := conn.Deployments().WaitForAvailable(ctx, namespace, cloudNativePGDeploymentName); err != nil {
		return err
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := cloudNativePGWebhooksHaveCABundle(ctx, conn)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("cloudnativepg webhooks were not trusted before timeout: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func cloudNativePGWebhooksHaveCABundle(ctx context.Context, conn kubernetes.Connection) (bool, error) {
	mutating := &admissionv1.MutatingWebhookConfiguration{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "MutatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{Name: "cnpg-mutating-webhook-configuration"},
	}
	mutatingLive, err := conn.Objects().Get(ctx, mutating)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	mutatingConfig, ok := mutatingLive.(*admissionv1.MutatingWebhookConfiguration)
	if !ok {
		return false, fmt.Errorf("unexpected mutating webhook type %T", mutatingLive)
	}
	for _, webhook := range mutatingConfig.Webhooks {
		if len(webhook.ClientConfig.CABundle) == 0 {
			return false, nil
		}
	}

	validating := &admissionv1.ValidatingWebhookConfiguration{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{Name: "cnpg-validating-webhook-configuration"},
	}
	validatingLive, err := conn.Objects().Get(ctx, validating)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	validatingConfig, ok := validatingLive.(*admissionv1.ValidatingWebhookConfiguration)
	if !ok {
		return false, fmt.Errorf("unexpected validating webhook type %T", validatingLive)
	}
	for _, webhook := range validatingConfig.Webhooks {
		if len(webhook.ClientConfig.CABundle) == 0 {
			return false, nil
		}
	}

	return true, nil
}

func objectResource(resType string, obj ctrlclient.Object) Resource {
	return Resource{
		Type:      resType,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		Desired:   obj,
	}
}

func objectResourceType(obj ctrlclient.Object) string {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Group == "" {
		return gvk.Kind
	}
	return gvk.Kind + "." + gvk.Group
}

func objectForDiff(obj ctrlclient.Object) any {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return obj
	}
	delete(raw, "status")
	return raw
}

func mustCloudNativePGCRDs(version string) []apiextensionsv1.CustomResourceDefinition {
	crds, err := cloudNativePGCRDs(version)
	if err != nil {
		panic(err)
	}
	return crds
}

func cloudNativePGCRDs(version string) ([]apiextensionsv1.CustomResourceDefinition, error) {
	data, err := releases.OperatorManifests.ReadFile("cnpg-" + version + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read cloudnativepg release manifest: %w", err)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var crds []apiextensionsv1.CustomResourceDefinition
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode cloudnativepg manifest: %w", err)
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crd); err != nil {
			return nil, fmt.Errorf("convert cloudnativepg crd %q: %w", obj.GetName(), err)
		}
		crds = append(crds, crd)
	}

	return crds, nil
}

func borqPostgresCluster(cfg BorqPostgresConfig) *apiv1.Cluster {
	cluster := &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.SchemeGroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Name,
			Namespace: cfg.Namespace,
			Labels:    cloudNativePGLabels("postgres"),
		},
		Spec: apiv1.ClusterSpec{
			Instances: cfg.Instances,
			Bootstrap: &apiv1.BootstrapConfiguration{
				InitDB: &apiv1.BootstrapInitDB{
					Database: cfg.Database,
					Owner:    cfg.Owner,
				},
			},
			StorageConfiguration: apiv1.StorageConfiguration{
				Size: cfg.StorageSize,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cfg.CPURequest),
					corev1.ResourceMemory: resource.MustParse(cfg.MemoryRequest),
				},
			},
		},
	}
	if strings.TrimSpace(cfg.StorageClass) != "" {
		cluster.Spec.StorageConfiguration.StorageClass = &cfg.StorageClass
	}
	return cluster
}

func borqPostgresCredentialsPushSecret(cfg BorqPostgresConfig) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "external-secrets.io/v1alpha1",
			"kind":       "PushSecret",
			"metadata": map[string]any{
				"name":      cfg.Name + "-credentials",
				"namespace": cfg.Namespace,
			},
			"spec": map[string]any{
				"updatePolicy":   "Replace",
				"deletionPolicy": "None",
				"secretStoreRefs": []any{
					map[string]any{
						"kind": "SecretStore",
						"name": "openbao-backend",
					},
				},
				"selector": map[string]any{
					"secret": map[string]any{
						"name": cfg.Name + apiv1.ApplicationUserSecretSuffix,
					},
				},
				"dataTo": []any{
					map[string]any{
						"remoteKey": cfg.Namespace + "/" + cfg.Database,
						"storeRef": map[string]any{
							"kind": "SecretStore",
							"name": "openbao-backend",
						},
					},
				},
			},
		},
	}
}

func cloudNativePGNamespaceObject(cfg CloudNativePGOperatorConfig) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.Namespace,
			Labels: cloudNativePGLabels("operator"),
		},
	}
}

func cloudNativePGServiceAccountObject(cfg CloudNativePGOperatorConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudNativePGServiceAccount,
			Namespace: cfg.Namespace,
			Labels:    cloudNativePGLabels("operator"),
		},
	}
}

func cloudNativePGManagerClusterRole(cfg CloudNativePGOperatorConfig) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   cloudNativePGServiceAccount,
			Labels: cloudNativePGLabels("operator"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "services"}, Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"configmaps/status", "secrets/status"}, Verbs: []string{"get", "patch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims", "pods", "pods/exec"}, Verbs: []string{"create", "delete", "get", "list", "patch", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods/status"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"create", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, Verbs: []string{"get", "patch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "delete", "get", "list", "patch", "watch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create", "get", "update"}},
			{APIGroups: []string{"monitoring.coreos.com"}, Resources: []string{"podmonitors"}, Verbs: []string{"create", "delete", "get", "list", "patch", "watch"}},
			{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"backups", "clusters", "databases", "poolers", "publications", "scheduledbackups", "subscriptions"}, Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"backups/status", "databases/status", "publications/status", "scheduledbackups/status", "subscriptions/status"}, Verbs: []string{"get", "patch", "update"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusterimagecatalogs", "imagecatalogs"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters/finalizers", "poolers/finalizers"}, Verbs: []string{"update"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters/status", "failoverquorums/status", "poolers/status"}, Verbs: []string{"get", "patch", "update", "watch"}},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"failoverquorums"}, Verbs: []string{"create", "delete", "get", "list", "watch"}},
			{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings", "roles"}, Verbs: []string{"create", "get", "list", "patch", "update", "watch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshots"}, Verbs: []string{"create", "get", "list", "patch", "watch"}},
		},
	}
}

func cloudNativePGDatabaseEditorClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-database-editor-role", []string{"databases"}, []string{"create", "delete", "get", "list", "patch", "update", "watch"})
}

func cloudNativePGDatabaseViewerClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-database-viewer-role", []string{"databases"}, []string{"get", "list", "watch"})
}

func cloudNativePGPublicationEditorClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-publication-editor-role", []string{"publications"}, []string{"create", "delete", "get", "list", "patch", "update", "watch"})
}

func cloudNativePGPublicationViewerClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-publication-viewer-role", []string{"publications"}, []string{"get", "list", "watch"})
}

func cloudNativePGSubscriptionEditorClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-subscription-editor-role", []string{"subscriptions"}, []string{"create", "delete", "get", "list", "patch", "update", "watch"})
}

func cloudNativePGSubscriptionViewerClusterRole() *rbacv1.ClusterRole {
	return cloudNativePGEndUserClusterRole("cnpg-subscription-viewer-role", []string{"subscriptions"}, []string{"get", "list", "watch"})
}

func cloudNativePGEndUserClusterRole(name string, resources []string, verbs []string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: cloudNativePGLabels("operator"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: resources, Verbs: verbs},
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{resources[0] + "/status"}, Verbs: []string{"get"}},
		},
	}
}

func cloudNativePGClusterRoleBinding(cfg CloudNativePGOperatorConfig) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cnpg-manager-rolebinding",
			Labels: cloudNativePGLabels("operator"),
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: cloudNativePGServiceAccount},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: cloudNativePGServiceAccount, Namespace: cfg.Namespace},
		},
	}
}

func cloudNativePGMonitoringConfigMap(cfg CloudNativePGOperatorConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudNativePGMonitoringName,
			Namespace: cfg.Namespace,
			Labels:    mergeStringMaps(cloudNativePGLabels("operator"), map[string]string{"cnpg.io/reload": ""}),
		},
		Data: map[string]string{},
	}
}

func cloudNativePGWebhookServiceObject(cfg CloudNativePGOperatorConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudNativePGWebhookService,
			Namespace: cfg.Namespace,
			Labels:    cloudNativePGLabels("operator"),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": "cloudnative-pg"},
			Ports: []corev1.ServicePort{
				{Port: 443, TargetPort: int32OrString(9443)},
			},
		},
	}
}

func cloudNativePGDeploymentObject(cfg CloudNativePGOperatorConfig) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudNativePGDeploymentName,
			Namespace: cfg.Namespace,
			Labels:    cloudNativePGLabels("operator"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "cloudnative-pg"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: cloudNativePGLabels("operator")},
				Spec: corev1.PodSpec{
					ServiceAccountName: cloudNativePGServiceAccount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "manager",
							Image:           cfg.Image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/manager"},
							Args: []string{
								"controller",
								"--leader-elect",
								"--max-concurrent-reconciles=10",
								"--config-map-name=cnpg-controller-manager-config",
								"--secret-name=cnpg-controller-manager-config",
								"--webhook-port=9443",
							},
							Env: []corev1.EnvVar{
								{Name: "OPERATOR_IMAGE_NAME", Value: cfg.Image},
								{Name: "OPERATOR_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
								{Name: "MONITORING_QUERIES_CONFIGMAP", Value: cloudNativePGMonitoringName},
							},
							Ports: []corev1.ContainerPort{
								{Name: "metrics", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
								{Name: "webhook-server", ContainerPort: 9443, Protocol: corev1.ProtocolTCP},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cfg.CPURequest),
									corev1.ResourceMemory: resource.MustParse(cfg.MemoryRequest),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cfg.CPULimit),
									corev1.ResourceMemory: resource.MustParse(cfg.MemoryLimit),
								},
							},
							LivenessProbe:  cloudNativePGReadyProbe(),
							ReadinessProbe: cloudNativePGReadyProbe(),
							StartupProbe: &corev1.Probe{
								ProbeHandler:     cloudNativePGReadyProbe().ProbeHandler,
								FailureThreshold: 6,
								PeriodSeconds:    5,
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:                int64Ptr(10001),
								RunAsGroup:               int64Ptr(10001),
								ReadOnlyRootFilesystem:   boolPtr(true),
								AllowPrivilegeEscalation: boolPtr(false),
								SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "scratch-data", MountPath: "/controller"},
								{Name: "webhook-certificates", MountPath: "/run/secrets/cnpg.io/webhook"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "scratch-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "webhook-certificates", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "cnpg-webhook-cert", Optional: boolPtr(true), DefaultMode: int32Ptr(420)}}},
					},
					TerminationGracePeriodSeconds: int64Ptr(10),
				},
			},
		},
	}
}

func cloudNativePGReadyProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/readyz",
				Port:   int32OrString(9443),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
	}
}

func cloudNativePGMutatingWebhookConfiguration(cfg CloudNativePGOperatorConfig) *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "MutatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cnpg-mutating-webhook-configuration",
			Labels: cloudNativePGLabels("operator"),
		},
		Webhooks: []admissionv1.MutatingWebhook{
			mutatingWebhook(cfg, "mbackup.cnpg.io", "/mutate-postgresql-cnpg-io-v1-backup", "backups"),
			mutatingWebhook(cfg, "mcluster.cnpg.io", "/mutate-postgresql-cnpg-io-v1-cluster", "clusters"),
			mutatingWebhook(cfg, "mdatabase.cnpg.io", "/mutate-postgresql-cnpg-io-v1-database", "databases"),
			mutatingWebhook(cfg, "mscheduledbackup.cnpg.io", "/mutate-postgresql-cnpg-io-v1-scheduledbackup", "scheduledbackups"),
		},
	}
}

func cloudNativePGValidatingWebhookConfiguration(cfg CloudNativePGOperatorConfig) *admissionv1.ValidatingWebhookConfiguration {
	return &admissionv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cnpg-validating-webhook-configuration",
			Labels: cloudNativePGLabels("operator"),
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			validatingWebhook(cfg, "vbackup.cnpg.io", "/validate-postgresql-cnpg-io-v1-backup", "backups"),
			validatingWebhook(cfg, "vcluster.cnpg.io", "/validate-postgresql-cnpg-io-v1-cluster", "clusters"),
			validatingWebhook(cfg, "vdatabase.cnpg.io", "/validate-postgresql-cnpg-io-v1-database", "databases"),
			validatingWebhook(cfg, "vpooler.cnpg.io", "/validate-postgresql-cnpg-io-v1-pooler", "poolers"),
			validatingWebhook(cfg, "vscheduledbackup.cnpg.io", "/validate-postgresql-cnpg-io-v1-scheduledbackup", "scheduledbackups"),
		},
	}
}

func mutatingWebhook(cfg CloudNativePGOperatorConfig, name string, path string, resourceName string) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{
		Name:                    name,
		AdmissionReviewVersions: []string{"v1"},
		ClientConfig:            webhookClientConfig(cfg, path),
		FailurePolicy:           failurePolicyPtr(admissionv1.Fail),
		Rules:                   webhookRules(resourceName),
		SideEffects:             sideEffectsPtr(admissionv1.SideEffectClassNone),
	}
}

func validatingWebhook(cfg CloudNativePGOperatorConfig, name string, path string, resourceName string) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{
		Name:                    name,
		AdmissionReviewVersions: []string{"v1"},
		ClientConfig:            webhookClientConfig(cfg, path),
		FailurePolicy:           failurePolicyPtr(admissionv1.Fail),
		Rules:                   webhookRules(resourceName),
		SideEffects:             sideEffectsPtr(admissionv1.SideEffectClassNone),
	}
}

func webhookClientConfig(cfg CloudNativePGOperatorConfig, path string) admissionv1.WebhookClientConfig {
	return admissionv1.WebhookClientConfig{
		Service: &admissionv1.ServiceReference{
			Name:      cloudNativePGWebhookService,
			Namespace: cfg.Namespace,
			Path:      &path,
		},
	}
}

func webhookRules(resourceName string) []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{
		{
			Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
			Rule: admissionv1.Rule{
				APIGroups:   []string{"postgresql.cnpg.io"},
				APIVersions: []string{"v1"},
				Resources:   []string{resourceName},
			},
		},
	}
}

func cloudNativePGLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "cloudnative-pg",
		"app.kubernetes.io/managed-by": "borq",
		"app.kubernetes.io/component":  component,
	}
}

func mergeStringMaps(first map[string]string, second map[string]string) map[string]string {
	merged := make(map[string]string, len(first)+len(second))
	for key, value := range first {
		merged[key] = value
	}
	for key, value := range second {
		merged[key] = value
	}
	return merged
}

func boolPtr(value bool) *bool { return &value }

func int32Ptr(value int32) *int32 { return &value }

func int64Ptr(value int64) *int64 { return &value }

func failurePolicyPtr(value admissionv1.FailurePolicyType) *admissionv1.FailurePolicyType {
	return &value
}

func sideEffectsPtr(value admissionv1.SideEffectClass) *admissionv1.SideEffectClass {
	return &value
}

func int32OrString(value int32) intstr.IntOrString {
	return intstr.FromInt32(value)
}
