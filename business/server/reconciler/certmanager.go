package reconciler

import (
	"context"
	"fmt"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	certmanagercrds "github.com/stefanmcshane/borq/zarf/crds/certmanager"
)

const (
	certManagerReadyType         = "CertManagerReady"
	certManagerClusterIssuerType = "ClusterIssuer.cert-manager.io"
	certManagerNamespace         = "borq-system"
	certManagerDeploymentName    = "cert-manager"
	certManagerServiceAccount    = "cert-manager"
	certManagerVersion           = "v1.20.2"

	certManagerClusterIssuerName = "borq-selfsigned-issuer"
	cnpgSecretName               = "cnpg-webhook-cert"
)

// CertManagerConfig describes the cert-manager install.
type CertManagerConfig struct {
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
}

type certManagerReady struct {
	Available bool `json:"available"`
}

// CertManagerHandler implements ComponentHandler for cert-manager.
type CertManagerHandler struct{}

func (CertManagerHandler) Name() string { return "cert-manager" }

func (CertManagerHandler) DefaultConfig() any {
	return &CertManagerConfig{
		Version:   certManagerVersion,
		Namespace: certManagerNamespace,
		Image:     "quay.io/jetstack/cert-manager-controller:v1.20.2",
	}
}

func (CertManagerHandler) Plan(config any) Plan {
	cfg := config.(*CertManagerConfig)
	defaultCertManagerConfig(cfg)

	resources := certManagerResources(*cfg)
	return Plan{
		ApplyOrder: certManagerApplyOrder(resources),
		Desired:    resources,
		Current:    certManagerCurrentFunc(resources),
	}
}

func (CertManagerHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == certManagerReadyType {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return waitForCertManagerDeployment(waitCtx, conn, res.Namespace)
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("cert-manager: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Apply(ctx, obj)
}

func (CertManagerHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == certManagerReadyType {
		return nil
	}

	obj, ok := res.Desired.(ctrlclient.Object)
	if !ok {
		obj, ok = res.Actual.(ctrlclient.Object)
	}
	if !ok {
		return fmt.Errorf("cert-manager: unsupported resource %s/%s", res.Type, res.Name)
	}
	return conn.Objects().Delete(ctx, obj)
}

func defaultCertManagerConfig(cfg *CertManagerConfig) {
	defaults := CertManagerHandler{}.DefaultConfig().(*CertManagerConfig)
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

func certManagerResources(cfg CertManagerConfig) []Resource {
	objects := make([]ctrlclient.Object, 0)

	for _, crd := range mustCertManagerCRDs() {
		crd.Labels = mergeStringMaps(crd.Labels, certManagerLabels("crd"))
		objects = append(objects, crd.DeepCopy())
	}

	objects = append(objects,
		certManagerNamespaceObject(cfg),
		certManagerServiceAccountObject(cfg),
		certManagerClusterRole(),
		certManagerClusterRoleBinding(cfg),
		certManagerDeploymentObject(cfg),
		certManagerClusterIssuerObject(),
	)

	resources := make([]Resource, 0, len(objects)+1)
	for _, obj := range objects {
		resources = append(resources, objectResource(objectResourceType(obj), obj))
	}
	resources = append(resources, Resource{
		Type:      certManagerReadyType,
		Namespace: cfg.Namespace,
		Name:      certManagerDeploymentName,
		Desired:   certManagerReady{Available: true},
	})
	return resources
}

func certManagerApplyOrder(resources []Resource) []string {
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

func certManagerCurrentFunc(desired []Resource) func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
	return func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
		resources := make([]Resource, 0, len(desired))
		for _, res := range desired {
			if res.Type == certManagerReadyType {
				resources = append(resources, certManagerReadyResource(ctx, conn, res))
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

func certManagerReadyResource(ctx context.Context, conn kubernetes.Connection, res Resource) Resource {
	ready := conn.Deployments().IsAvailable(ctx, res.Namespace, certManagerDeploymentName)
	return Resource{
		Type:      res.Type,
		Namespace: res.Namespace,
		Name:      res.Name,
		Actual:    certManagerReady{Available: ready},
	}
}

func waitForCertManagerDeployment(ctx context.Context, conn kubernetes.Connection, namespace string) error {
	return conn.Deployments().WaitForAvailable(ctx, namespace, certManagerDeploymentName)
}

func mustCertManagerCRDs() []apiextensionsv1.CustomResourceDefinition {
	crds, err := certManagerCRDs()
	if err != nil {
		panic(err)
	}
	return crds
}

func certManagerCRDs() ([]apiextensionsv1.CustomResourceDefinition, error) {
	return kubernetes.ParseCRDs(certmanagercrds.CRDYAML)
}

func certManagerLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "cert-manager",
		"app.kubernetes.io/managed-by": "borq",
		"app.kubernetes.io/component":  component,
	}
}

func certManagerNamespaceObject(cfg CertManagerConfig) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.Namespace,
			Labels: certManagerLabels("cert-manager"),
		},
	}
}

func certManagerServiceAccountObject(cfg CertManagerConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      certManagerServiceAccount,
			Namespace: cfg.Namespace,
			Labels:    certManagerLabels("cert-manager"),
		},
	}
}

func certManagerClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cert-manager",
			Labels: certManagerLabels("cert-manager"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps", "services", "pods", "namespaces", "serviceaccounts", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"}, Verbs: []string{"create", "update", "patch", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			{APIGroups: []string{"cert-manager.io"}, Resources: []string{"certificates", "issuers", "clusterissuers", "certificaterequests"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"cert-manager.io"}, Resources: []string{"certificates/status", "issuers/status", "clusterissuers/status", "certificaterequests/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
			{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"acme.cert-manager.io"}, Resources: []string{"orders", "challenges"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"acme.cert-manager.io"}, Resources: []string{"orders/status", "challenges/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"apiregistration.k8s.io"}, Resources: []string{"apiservices"}, Verbs: []string{"get", "list", "watch"}},
		},
	}
}

func certManagerClusterRoleBinding(cfg CertManagerConfig) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cert-manager",
			Labels: certManagerLabels("cert-manager"),
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cert-manager"},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: certManagerServiceAccount, Namespace: cfg.Namespace},
		},
	}
}

func certManagerDeploymentObject(cfg CertManagerConfig) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      certManagerDeploymentName,
			Namespace: cfg.Namespace,
			Labels:    certManagerLabels("cert-manager"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "cert-manager"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: certManagerLabels("cert-manager")},
				Spec: corev1.PodSpec{
					ServiceAccountName: certManagerServiceAccount,
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
								"--v=2",
								"--cluster-resource-namespace=" + cfg.Namespace,
								"--leader-election-namespace=" + cfg.Namespace,
							},
							Ports: []corev1.ContainerPort{
								{Name: "http-prometheus", ContainerPort: 9402},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resourceMustParse("10m"),
									corev1.ResourceMemory: resourceMustParse("64Mi"),
								},
							},
						},
						{
							Name:            "cainjector",
							Image:           "quay.io/jetstack/cert-manager-cainjector:v1.20.2",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								"--v=2",
								"--leader-election-namespace=" + cfg.Namespace,
								"--metrics-listen-address=0.0.0.0:9404",
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

func certManagerClusterIssuerObject() *cmapi.ClusterIssuer {
	return &cmapi.ClusterIssuer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cert-manager.io/v1",
			Kind:       "ClusterIssuer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   certManagerClusterIssuerName,
			Labels: certManagerLabels("cert-manager"),
		},
		Spec: cmapi.IssuerSpec{
			IssuerConfig: cmapi.IssuerConfig{
				SelfSigned: &cmapi.SelfSignedIssuer{},
			},
		},
	}
}

func resourceMustParse(value string) resource.Quantity {
	return resource.MustParse(value)
}
