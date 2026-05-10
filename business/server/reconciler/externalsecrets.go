package reconciler

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	externalsecretscrds "github.com/stefanmcshane/borq/zarf/crds/externalsecrets"
)

const (
	externalSecretsReadyType      = "ExternalSecretsReady"
	externalSecretsNamespace      = "borq-system"
	externalSecretsDeploymentName = "external-secrets"
	externalSecretsServiceAccount = "external-secrets"
	externalSecretsVersion        = "v2.4.0"
	clusterSecretStoreType        = "ClusterSecretStore.external-secrets.io"

	vaultServiceName  = "vault"
	vaultUnsealSecret = "vault-unseal"
	vaultTokenKey     = "root_token"
	vaultSecretPath   = "secret"
)

// ExternalSecretsConfig describes the External Secrets Operator install.
type ExternalSecretsConfig struct {
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
}

type externalSecretsReady struct {
	Available bool `json:"available"`
}

// ExternalSecretsHandler implements ComponentHandler for ESO.
type ExternalSecretsHandler struct{}

func (ExternalSecretsHandler) Name() string { return "external-secrets" }

func (ExternalSecretsHandler) DefaultConfig() any {
	return &ExternalSecretsConfig{
		Version:   externalSecretsVersion,
		Namespace: externalSecretsNamespace,
		Image:     "ghcr.io/external-secrets/external-secrets:v2.4.0",
	}
}

func (ExternalSecretsHandler) Plan(config any) Plan {
	cfg := config.(*ExternalSecretsConfig)
	defaultExternalSecretsConfig(cfg)

	resources := externalSecretsResources(*cfg)
	return Plan{
		ApplyOrder: externalSecretsApplyOrder(resources),
		Desired:    resources,
		Current:    externalSecretsCurrentFunc(resources),
	}
}

func (ExternalSecretsHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == externalSecretsReadyType {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return waitForExternalSecretsDeployment(waitCtx, conn, res.Namespace)
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("external-secrets: unsupported resource %s/%s", res.Type, res.Name)
	}

	return conn.Objects().Apply(ctx, obj)
}

func (ExternalSecretsHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == externalSecretsReadyType {
		return nil
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		obj, ok = res.Actual.(ctrlclient.Object)
	}
	if !ok {
		return fmt.Errorf("external-secrets: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Delete(ctx, obj)
}

func defaultExternalSecretsConfig(cfg *ExternalSecretsConfig) {
	defaults := ExternalSecretsHandler{}.DefaultConfig().(*ExternalSecretsConfig)
	if cfg.Version == "" {
		cfg.Version = defaults.Version
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaults.Namespace
	}
	if cfg.Image == "" {
		cfg.Image = defaults.Image
	}
}

func externalSecretsResources(cfg ExternalSecretsConfig) []Resource {
	objects := make([]ctrlclient.Object, 0)

	for _, crd := range mustESOCrds() {
		crd.Labels = mergeStringMaps(crd.Labels, externalSecretsLabels("crd"))
		objects = append(objects, crd.DeepCopy())
	}

	objects = append(objects,
		externalSecretsNamespaceObject(cfg),
		externalSecretsServiceAccountObject(cfg),
		externalSecretsClusterRole(),
		externalSecretsClusterRoleBinding(cfg),
		externalSecretsDeploymentObject(cfg),
		externalSecretsClusterSecretStore(cfg),
	)

	resources := make([]Resource, 0, len(objects)+1)
	for _, obj := range objects {
		resources = append(resources, objectResource(objectResourceType(obj), obj))
	}
	resources = append(resources, Resource{
		Type:      externalSecretsReadyType,
		Namespace: cfg.Namespace,
		Name:      externalSecretsDeploymentName,
		Desired:   externalSecretsReady{Available: true},
	})
	return resources
}

func externalSecretsApplyOrder(resources []Resource) []string {
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

func externalSecretsCurrentFunc(desired []Resource) func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
	return func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
		resources := make([]Resource, 0, len(desired))
		for _, res := range desired {
			if res.Type == externalSecretsReadyType {
				resources = append(resources, externalSecretsReadyResource(ctx, conn, res))
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

func externalSecretsReadyResource(ctx context.Context, conn kubernetes.Connection, res Resource) Resource {
	ready := conn.Deployments().IsAvailable(ctx, res.Namespace, externalSecretsDeploymentName)
	return Resource{
		Type:      res.Type,
		Namespace: res.Namespace,
		Name:      res.Name,
		Actual:    externalSecretsReady{Available: ready},
	}
}

func waitForExternalSecretsDeployment(ctx context.Context, conn kubernetes.Connection, namespace string) error {
	return conn.Deployments().WaitForAvailable(ctx, namespace, externalSecretsDeploymentName)
}

func mustESOCrds() []apiextensionsv1.CustomResourceDefinition {
	crds, err := kubernetes.ParseCRDs(externalsecretscrds.CRDYAML)
	if err != nil {
		panic(err)
	}
	return crds
}

func externalSecretsLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "external-secrets",
		"app.kubernetes.io/managed-by": "borq",
		"app.kubernetes.io/component":  component,
	}
}

func externalSecretsNamespaceObject(cfg ExternalSecretsConfig) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.Namespace,
			Labels: externalSecretsLabels("namespace"),
		},
	}
}

func externalSecretsServiceAccountObject(cfg ExternalSecretsConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalSecretsServiceAccount,
			Namespace: cfg.Namespace,
			Labels:    externalSecretsLabels("externalsecrets"),
		},
	}
}

func externalSecretsClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "external-secrets",
			Labels: externalSecretsLabels("externalsecrets"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps", "events", "serviceaccounts"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"pods", "namespaces", "services"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"external-secrets.io"}, Resources: []string{"secretstores", "clustersecretstores", "externalsecrets", "clusterexternalsecrets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"external-secrets.io"}, Resources: []string{"secretstores/status", "clustersecretstores/status", "externalsecrets/status", "clusterexternalsecrets/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
		},
	}
}

func externalSecretsClusterRoleBinding(cfg ExternalSecretsConfig) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "external-secrets",
			Labels: externalSecretsLabels("externalsecrets"),
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "external-secrets"},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: externalSecretsServiceAccount, Namespace: cfg.Namespace},
		},
	}
}

func externalSecretsDeploymentObject(cfg ExternalSecretsConfig) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalSecretsDeploymentName,
			Namespace: cfg.Namespace,
			Labels:    externalSecretsLabels("externalsecrets"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "external-secrets"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: externalSecretsLabels("externalsecrets")},
				Spec: corev1.PodSpec{
					ServiceAccountName: externalSecretsServiceAccount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{
						{
							Name:            "controller",
							Image:           cfg.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								"--concurrent=1",
								"--metrics-addr=:8080",
							},
							Ports: []corev1.ContainerPort{
								{Name: "metrics", ContainerPort: 8080},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resourceMustParse("10m"),
									corev1.ResourceMemory: resourceMustParse("64Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

func externalSecretsClusterSecretStore(cfg ExternalSecretsConfig) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ClusterSecretStore",
			"metadata": map[string]any{
				"name": "vault-backend",
				"labels": map[string]any{
					"app.kubernetes.io/name":       "external-secrets",
					"app.kubernetes.io/managed-by": "borq",
					"app.kubernetes.io/component":  "externalsecrets",
				},
			},
			"spec": map[string]any{
				"provider": map[string]any{
					"vault": map[string]any{
						"server":  fmt.Sprintf("http://%s.%s.svc:8200", vaultServiceName, cfg.Namespace),
						"path":    vaultSecretPath,
						"version": "v2",
						"auth": map[string]any{
							"tokenSecretRef": map[string]any{
								"name": vaultUnsealSecret,
								"key":  vaultTokenKey,
							},
						},
					},
				},
			},
		},
	}
}
