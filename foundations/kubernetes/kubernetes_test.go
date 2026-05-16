package kubernetes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/matryer/is"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestBuildConnectionConfig verifies that buildConnectionConfig applies
// options and falls back to the default field owner.
func TestBuildConnectionConfig(t *testing.T) {
	is := is.New(t)

	config, err := buildConnectionConfig(nil)
	is.NoErr(err)
	is.Equal(config.fieldOwner, defaultFieldOwner)

	config, err = buildConnectionConfig([]Option{WithFieldOwner("borq-management-bootstrap")})
	is.NoErr(err)
	is.Equal(config.fieldOwner, "borq-management-bootstrap")

	_, err = buildConnectionConfig([]Option{WithFieldOwner(" ")})
	is.Equal(err.Error(), "field owner must not be empty")
}

// TestRestConfigForConfigLoadsKubeconfigPath verifies that a valid kubeconfig
// path is loaded into a rest.Config with the expected host.
func TestRestConfigForConfigLoadsKubeconfigPath(t *testing.T) {
	is := is.New(t)

	kubeconfigPath := writeTestKubeconfig(t)
	restConfig, err := restConfigForConfig(Config{KubeconfigPath: kubeconfigPath})
	is.NoErr(err)
	is.Equal(restConfig.Host, "https://127.0.0.1:6443")
}

// TestConnectionRestConfigReturnsCopy verifies that callers receive a cloned
// REST config instead of shared mutable connection state.
func TestConnectionRestConfigReturnsCopy(t *testing.T) {
	is := is.New(t)

	connection := Connection{
		restConfig: rest.Config{
			Host:  "https://127.0.0.1:6443",
			QPS:   5,
			Burst: 10,
		},
	}

	restConfig := connection.RestConfig()
	restConfig.Host = "https://changed.example"
	restConfig.QPS = 99

	is.Equal(connection.restConfig.Host, "https://127.0.0.1:6443")
	is.Equal(connection.restConfig.QPS, float32(5))
}

// TestRestConfigForConfigRejectsInvalidKubeconfigPath verifies that a
// non-existent kubeconfig path returns a descriptive error.
func TestRestConfigForConfigRejectsInvalidKubeconfigPath(t *testing.T) {
	is := is.New(t)

	_, err := restConfigForConfig(Config{KubeconfigPath: "/does/not/exist"})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), `load kubeconfig "/does/not/exist"`))
}

// TestRestConfigForConfigDefaultsToInCluster verifies that an empty config
// tries to load the in-cluster config (which fails outside a cluster).
func TestRestConfigForConfigDefaultsToInCluster(t *testing.T) {
	is := is.New(t)

	_, err := restConfigForConfig(Config{})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "load in-cluster kubernetes config"))
}

// TestNewSchemeRegistersCloudNativePG verifies the shared scheme can encode
// CloudNativePG SDK types used by the reconciler.
func TestNewSchemeRegistersCloudNativePG(t *testing.T) {
	is := is.New(t)

	scheme := NewScheme()
	gvks, _, err := scheme.ObjectKinds(&apiv1.Cluster{})
	is.NoErr(err)
	is.Equal(len(gvks), 1)
	is.Equal(gvks[0].Group, "postgresql.cnpg.io")
	is.Equal(gvks[0].Version, "v1")
	is.Equal(gvks[0].Kind, "Cluster")
}

// TestObjectManagerApplyGetDelete verifies the generic object manager can
// manage typed custom resources through a controller-runtime client.
func TestObjectManagerApplyGetDelete(t *testing.T) {
	is := is.New(t)

	scheme := NewScheme()
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	manager := ObjectManager{client: client, fieldOwner: defaultFieldOwner}
	cluster := &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1.SchemeGroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "borq-postgres",
			Namespace: "borq-system",
		},
		Spec: apiv1.ClusterSpec{
			Instances: 1,
			StorageConfiguration: apiv1.StorageConfiguration{
				Size: "5Gi",
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		},
	}

	ctx := context.Background()
	is.NoErr(manager.Apply(ctx, cluster))

	live, err := manager.Get(ctx, cluster)
	is.NoErr(err)
	liveCluster, ok := live.(*apiv1.Cluster)
	is.True(ok)
	is.Equal(liveCluster.Spec.StorageConfiguration.Size, "5Gi")

	cluster.Spec.StorageConfiguration.Size = "10Gi"
	is.NoErr(manager.Apply(ctx, cluster))
	live, err = manager.Get(ctx, cluster)
	is.NoErr(err)
	liveCluster = live.(*apiv1.Cluster)
	is.Equal(liveCluster.Spec.StorageConfiguration.Size, "10Gi")

	is.NoErr(manager.Delete(ctx, cluster))
	_, err = manager.Get(ctx, cluster)
	is.True(err != nil)
}

// TestCustomResourceListForCRDUsesListKind verifies CRD cleanup lists the
// list resource kind rather than the singular custom resource kind.
func TestCustomResourceListForCRDUsesListKind(t *testing.T) {
	is := is.New(t)

	list := customResourceListForCRD(apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "postgresql.cnpg.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "Cluster",
				ListKind: "ClusterList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1"},
			},
		},
	})

	is.Equal(list.GetAPIVersion(), "postgresql.cnpg.io/v1")
	is.Equal(list.GetKind(), "ClusterList")
}

// TestCanAdminNamespace verifies that CanAdminNamespace returns nil when the
// fake client reports the SSAR as allowed.
func TestCanAdminNamespace(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})

	connection := Connection{clientset: clientset}
	err := connection.CanAdminNamespace(context.Background(), "borq-system")
	is.NoErr(err)
}

// TestSelfSubjectAccessReviewUsesProvidedRule verifies that the SSAR request
// includes the correct namespace, verb, group, and resource from the input.
func TestSelfSubjectAccessReviewUsesProvidedRule(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		is.True(ok)
		review, ok := createAction.GetObject().(*authv1.SelfSubjectAccessReview)
		is.True(ok)
		is.Equal(review.Spec.ResourceAttributes.Namespace, "borq-system")
		is.Equal(review.Spec.ResourceAttributes.Verb, "get")
		is.Equal(review.Spec.ResourceAttributes.Group, "")
		is.Equal(review.Spec.ResourceAttributes.Resource, "pods")
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: false, Reason: "missing permission"},
		}, nil
	})

	connection := Connection{clientset: clientset}
	err := connection.SelfSubjectAccessReview(context.Background(), SelfSubjectAccessReviewInput{
		Namespace: "borq-system",
		Rule: PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get"},
		},
	})
	is.True(err != nil)
	is.Equal(err.Error(), `namespace access denied for "borq-system": missing permission`)
}

// TestSelfSubjectAccessReviewRejectsInvalidInput verifies that an SSAR with
// missing verbs is rejected before reaching the API.
func TestSelfSubjectAccessReviewRejectsInvalidInput(t *testing.T) {
	is := is.New(t)

	connection := Connection{clientset: fake.NewSimpleClientset()}
	err := connection.SelfSubjectAccessReview(context.Background(), SelfSubjectAccessReviewInput{
		Namespace: "borq-system",
		Rule: PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"pods"},
		},
	})

	is.True(err != nil)
	is.Equal(err.Error(), "self subject access review requires exactly one verb")
}

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	err := os.WriteFile(kubeconfigPath, []byte(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test-token
`), 0o600)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	return kubeconfigPath
}

// TestServiceManagerDiffIgnoresClusterAssignedFields verifies that
// cluster-assigned fields (ClusterIP, NodePort) are excluded from the diff.
func TestServiceManagerDiffIgnoresClusterAssignedFields(t *testing.T) {
	is := is.New(t)

	live := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "whoami",
			Namespace: "borq-system",
			Labels: map[string]string{
				"app.kubernetes.io/name": "whoami",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"app.kubernetes.io/name": "whoami",
			},
			ClusterIP: "10.96.0.1",
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     7833,
					NodePort: 31833,
				},
			},
		},
	}

	manager := ServiceManager{
		clientset: fake.NewSimpleClientset(&live),
	}

	diff, err := manager.Diff(context.Background(), Service{
		Name:      "whoami",
		Namespace: "borq-system",
		Labels: map[string]string{
			"app.kubernetes.io/name": "whoami",
		},
		Selector: map[string]string{
			"app.kubernetes.io/name": "whoami",
		},
		Type: NodePortService,
		Ports: []ServicePort{
			{Name: "http", Port: 7833, NodePort: 31833},
		},
	})
	is.NoErr(err)
	is.True(!diff.Changed)
}

// TestServiceManagerDiffMissingResource verifies that Diff returns Changed
// when the resource does not exist in the cluster.
func TestServiceManagerDiffMissingResource(t *testing.T) {
	is := is.New(t)

	manager := ServiceManager{
		clientset: fake.NewSimpleClientset(),
	}

	diff, err := manager.Diff(context.Background(), Service{
		Name:      "whoami",
		Namespace: "borq-system",
	})
	is.NoErr(err)
	is.True(diff.Changed)
	is.Equal(diff.Live, "")
}

// TestServiceManagerGetListDelete verifies the CRUD cycle for ServiceManager
// using a fake clientset.
func TestServiceManagerGetListDelete(t *testing.T) {
	is := is.New(t)

	manager := ServiceManager{
		clientset: fake.NewSimpleClientset(
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "whoami",
					Namespace: "borq-system",
				},
			},
		),
	}

	service, err := manager.Get(context.Background(), "borq-system", "whoami")
	is.NoErr(err)
	is.Equal(service.Name, "whoami")

	services, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(services), 1)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "whoami"))
	_, err = manager.Get(context.Background(), "borq-system", "whoami")
	is.True(err != nil)
}

// TestServiceResourceRoundTrip verifies that serviceToResource and
// serviceFromResource are inverses.
func TestServiceResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	service := Service{
		Name:                     "whoami",
		Namespace:                "borq-system",
		Labels:                   map[string]string{"app": "whoami"},
		Selector:                 map[string]string{"app": "whoami"},
		Type:                     NodePortService,
		PublishNotReadyAddresses: true,
		Ports: []ServicePort{
			{Name: "http", Port: 7833, TargetPort: 80, NodePort: 31833},
		},
	}

	is.Equal(serviceFromResource(serviceToResource(service)), service)
}

// TestPersistentVolumeClaimResourceRoundTrip verifies that
// persistentVolumeClaimToResource and persistentVolumeClaimFromResource are
// inverses.
func TestPersistentVolumeClaimResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	persistentVolumeClaim := PersistentVolumeClaim{
		Name:             "borq-data",
		Namespace:        "borq-system",
		Labels:           map[string]string{"app": "borq"},
		AccessModes:      []PersistentVolumeAccessMode{ReadWriteOnceVolume},
		RequestedStorage: "5Gi",
	}

	is.Equal(persistentVolumeClaimFromResource(persistentVolumeClaimToResource(persistentVolumeClaim)), persistentVolumeClaim)
}

// TestDeploymentResourceRoundTrip verifies that deploymentToResource and
// deploymentFromResource are inverses.
func TestDeploymentResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	deployment := Deployment{
		Name:      "whoami",
		Namespace: "borq-system",
		Labels:    map[string]string{"app": "whoami"},
		Selector:  map[string]string{"app": "whoami"},
		Replicas:  2,
		Pods: PodTemplate{
			Labels: map[string]string{"app": "whoami"},
			Volumes: []Volume{
				{Name: "data", PersistentVolumeClaimName: "whoami-data"},
			},
			Containers: []Container{
				{
					Name:         "whoami",
					Image:        "traefik/whoami:latest",
					Command:      []string{"whoami"},
					Args:         []string{"serve"},
					Env:          []EnvVar{{Name: "ENV", Value: "test"}},
					Ports:        []ContainerPort{{Name: "http", Port: 80}},
					VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/data"}},
					ReadinessProbe: HttpProbe{
						Path:                "/ready",
						Port:                80,
						InitialDelaySeconds: 1,
						PeriodSeconds:       2,
					},
				},
			},
		},
	}

	is.Equal(deploymentFromResource(deploymentToResource(deployment)), deployment)
}

// TestStatefulSetResourceRoundTrip verifies that statefulSetToResource and
// statefulSetFromResource are inverses for key fields.
func TestStatefulSetResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	statefulSet := StatefulSet{
		Name:        "openbao",
		Namespace:   "borq-system",
		Labels:      map[string]string{"app": "openbao"},
		Selector:    map[string]string{"app": "openbao"},
		Replicas:    1,
		ServiceName: "openbao-internal",
		Pods: PodTemplate{
			Labels: map[string]string{"app": "openbao"},
			Containers: []Container{
				{
					Name:  "openbao",
					Image: "ghcr.io/openbao/openbao:2.4.4",
					Ports: []ContainerPort{{Name: "http", Port: 8200}},
				},
			},
		},
	}

	roundTrip := statefulSetFromResource(statefulSetToResource(statefulSet))
	is.Equal(roundTrip.Name, statefulSet.Name)
	is.Equal(roundTrip.Namespace, statefulSet.Namespace)
	is.Equal(roundTrip.ServiceName, statefulSet.ServiceName)
	is.Equal(roundTrip.Replicas, statefulSet.Replicas)
	is.Equal(roundTrip.Pods.Containers[0].Name, statefulSet.Pods.Containers[0].Name)
	is.Equal(roundTrip.Pods.Containers[0].Image, statefulSet.Pods.Containers[0].Image)
	is.Equal(roundTrip.Pods.Containers[0].Ports[0], statefulSet.Pods.Containers[0].Ports[0])
}

// TestNamespaceResourceRoundTrip verifies that namespaceToResource and
// namespaceFromResource are inverses.
func TestNamespaceResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	namespace := Namespace{
		Name:   "borq-system",
		Labels: map[string]string{"managed-by": "borq"},
	}

	is.Equal(namespaceFromResource(namespaceToResource(namespace)), namespace)
}

// TestConfigMapResourceRoundTrip verifies that configMapToResource and
// configMapFromResource are inverses.
func TestConfigMapResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	configMap := ConfigMap{
		Name:      "borq-config",
		Namespace: "borq-system",
		Labels:    map[string]string{"managed-by": "borq"},
		Data:      map[string]string{"health_path": "/healthz"},
	}

	is.Equal(configMapFromResource(configMapToResource(configMap)), configMap)
}

// TestServiceAccountResourceRoundTrip verifies that serviceAccountToResource
// and serviceAccountFromResource are inverses.
func TestServiceAccountResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	serviceAccount := ServiceAccount{
		Name:      "borq",
		Namespace: "borq-system",
		Labels:    map[string]string{"managed-by": "borq"},
	}

	is.Equal(serviceAccountFromResource(serviceAccountToResource(serviceAccount)), serviceAccount)
}

// TestRoleResourceRoundTrip verifies that roleToResource and
// roleFromResource are inverses.
func TestRoleResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	role := Role{
		Name:      "borq",
		Namespace: "borq-system",
		Labels:    map[string]string{"managed-by": "borq"},
		Rules: []PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list"},
			},
		},
	}

	is.Equal(roleFromResource(roleToResource(role)), role)
}

// TestRoleBindingResourceRoundTrip verifies that roleBindingToResource and
// roleBindingFromResource are inverses.
func TestRoleBindingResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	roleBinding := RoleBinding{
		Name:      "borq",
		Namespace: "borq-system",
		Labels:    map[string]string{"managed-by": "borq"},
		RoleRef: RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "borq",
		},
		Subjects: []Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "borq",
				Namespace: "borq-system",
			},
		},
	}

	is.Equal(roleBindingFromResource(roleBindingToResource(roleBinding)), roleBinding)
}

// TestPodResourceRoundTrip verifies that podToResource and podFromResource
// are inverses for key fields.
func TestPodResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	pod := Pod{
		Name:      "debug",
		Namespace: "borq-system",
		Labels:    map[string]string{"app": "debug"},
		Spec: PodTemplate{
			Labels:             map[string]string{"app": "debug"},
			ServiceAccountName: "borq",
			Volumes: []Volume{
				{Name: "data", PersistentVolumeClaimName: "debug-data"},
			},
			Containers: []Container{
				{
					Name:         "debug",
					Image:        "busybox",
					Args:         []string{"sleep", "10"},
					VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/data"}},
				},
			},
		},
	}

	roundTrip := podFromResource(podToResource(pod))
	is.Equal(roundTrip.Name, pod.Name)
	is.Equal(roundTrip.Namespace, pod.Namespace)
	is.Equal(roundTrip.Labels, pod.Labels)
	is.Equal(roundTrip.Spec.Labels, pod.Spec.Labels)
	is.Equal(roundTrip.Spec.ServiceAccountName, pod.Spec.ServiceAccountName)
	is.Equal(roundTrip.Spec.Volumes, pod.Spec.Volumes)
	is.Equal(roundTrip.Spec.Containers[0].Name, pod.Spec.Containers[0].Name)
	is.Equal(roundTrip.Spec.Containers[0].Image, pod.Spec.Containers[0].Image)
	is.Equal(roundTrip.Spec.Containers[0].Args, pod.Spec.Containers[0].Args)
	is.Equal(roundTrip.Spec.Containers[0].VolumeMounts, pod.Spec.Containers[0].VolumeMounts)
}

// TestRbacAndConfigManagersGetListDelete verifies the Get/List/Delete cycle
// for ConfigMap, ServiceAccount, Role, and RoleBinding managers.
func TestRbacAndConfigManagersGetListDelete(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "borq-config", Namespace: "borq-system"},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: "borq-system"},
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: "borq-system"},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: "borq-system"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "borq"},
		},
	)

	configMapManager := ConfigMapManager{clientset: clientset}
	configMap, err := configMapManager.Get(context.Background(), "borq-system", "borq-config")
	is.NoErr(err)
	is.Equal(configMap.Name, "borq-config")
	configMaps, err := configMapManager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(configMaps), 1)
	is.NoErr(configMapManager.Delete(context.Background(), "borq-system", "borq-config"))

	serviceAccountManager := ServiceAccountManager{clientset: clientset}
	serviceAccount, err := serviceAccountManager.Get(context.Background(), "borq-system", "borq")
	is.NoErr(err)
	is.Equal(serviceAccount.Name, "borq")
	serviceAccounts, err := serviceAccountManager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(serviceAccounts), 1)
	is.NoErr(serviceAccountManager.Delete(context.Background(), "borq-system", "borq"))

	roleManager := RoleManager{clientset: clientset}
	role, err := roleManager.Get(context.Background(), "borq-system", "borq")
	is.NoErr(err)
	is.Equal(role.Name, "borq")
	roles, err := roleManager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(roles), 1)
	is.NoErr(roleManager.Delete(context.Background(), "borq-system", "borq"))

	roleBindingManager := RoleBindingManager{clientset: clientset}
	roleBinding, err := roleBindingManager.Get(context.Background(), "borq-system", "borq")
	is.NoErr(err)
	is.Equal(roleBinding.Name, "borq")
	roleBindings, err := roleBindingManager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(roleBindings), 1)
	is.NoErr(roleBindingManager.Delete(context.Background(), "borq-system", "borq"))
}

// TestDeploymentManagerWaitForAvailable verifies that WaitForAvailable
// returns nil when the deployment has available replicas.
func TestDeploymentManagerWaitForAvailable(t *testing.T) {
	is := is.New(t)

	manager := DeploymentManager{
		clientset: fake.NewSimpleClientset(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "whoami",
				Namespace: "borq-system",
			},
			Status: appsv1.DeploymentStatus{
				AvailableReplicas: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:   appsv1.DeploymentAvailable,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := manager.WaitForAvailable(ctx, "borq-system", "whoami")
	is.NoErr(err)
}

// TestDeploymentManagerGetListDeleteAndDiff verifies the full CRUD + Diff
// cycle for DeploymentManager.
func TestDeploymentManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := DeploymentManager{
		clientset: fake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "whoami",
					Namespace: "borq-system",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "whoami"},
					},
				},
			},
		),
	}

	deployment, err := manager.Get(context.Background(), "borq-system", "whoami")
	is.NoErr(err)
	is.Equal(deployment.Name, "whoami")

	deployments, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(deployments), 1)

	diff, err := manager.Diff(context.Background(), Deployment{Name: "missing", Namespace: "borq-system"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "whoami"))
}

// TestPersistentVolumeClaimManagerGetListDeleteAndDiff verifies the full
// CRUD + Diff cycle for PersistentVolumeClaimManager.
func TestPersistentVolumeClaimManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := PersistentVolumeClaimManager{
		clientset: fake.NewSimpleClientset(
			&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "borq-data",
					Namespace: "borq-system",
				},
			},
		),
	}

	persistentVolumeClaim, err := manager.Get(context.Background(), "borq-system", "borq-data")
	is.NoErr(err)
	is.Equal(persistentVolumeClaim.Name, "borq-data")

	persistentVolumeClaims, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(persistentVolumeClaims), 1)

	diff, err := manager.Diff(context.Background(), PersistentVolumeClaim{Name: "missing", Namespace: "borq-system"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "borq-data"))
}

// TestPersistentVolumeClaimManagerWaitForBound verifies that WaitForBound
// returns nil once the PVC transitions to Bound.
func TestPersistentVolumeClaimManagerWaitForBound(t *testing.T) {
	is := is.New(t)

	manager := PersistentVolumeClaimManager{
		clientset: fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "borq-data",
				Namespace: "borq-system",
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := manager.WaitForBound(ctx, "borq-system", "borq-data")
	is.NoErr(err)
}

// TestPersistentVolumeClaimManagerWaitForBoundTimeout verifies that
// WaitForBound returns an error when the PVC never binds before the timeout.
func TestPersistentVolumeClaimManagerWaitForBoundTimeout(t *testing.T) {
	is := is.New(t)

	manager := PersistentVolumeClaimManager{
		clientset: fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "borq-data",
				Namespace: "borq-system",
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := manager.WaitForBound(ctx, "borq-system", "borq-data")
	is.True(err != nil)
}

// TestStorageClassManagerEnsureRetainDefault verifies that
// EnsureRetainDefault copies a source StorageClass, sets Retain reclaim
// policy, marks it as default, and unmarks the previous default.
func TestStorageClassManagerEnsureRetainDefault(t *testing.T) {
	is := is.New(t)

	allowVolumeExpansion := true
	volumeBindingMode := storagev1.VolumeBindingWaitForFirstConsumer
	deleteReclaimPolicy := corev1.PersistentVolumeReclaimDelete

	manager := StorageClassManager{
		clientset: fake.NewSimpleClientset(&storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "standard",
				Annotations: storageClassDefaultAnnotations(true),
			},
			Provisioner:          "rancher.io/local-path",
			Parameters:           map[string]string{"type": "ssd"},
			ReclaimPolicy:        &deleteReclaimPolicy,
			AllowVolumeExpansion: &allowVolumeExpansion,
			VolumeBindingMode:    &volumeBindingMode,
			MountOptions:         []string{"discard"},
		}),
	}

	err := manager.EnsureRetainDefault(context.Background(), "retain")
	is.NoErr(err)

	retainStorageClass, err := manager.clientset.StorageV1().StorageClasses().Get(context.Background(), "retain", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(retainStorageClass.Provisioner, "rancher.io/local-path")
	is.Equal(retainStorageClass.Parameters, map[string]string{"type": "ssd"})
	is.True(retainStorageClass.ReclaimPolicy != nil)
	is.Equal(*retainStorageClass.ReclaimPolicy, corev1.PersistentVolumeReclaimRetain)
	is.True(retainStorageClass.AllowVolumeExpansion != nil)
	is.True(*retainStorageClass.AllowVolumeExpansion)
	is.True(retainStorageClass.VolumeBindingMode != nil)
	is.Equal(*retainStorageClass.VolumeBindingMode, storagev1.VolumeBindingWaitForFirstConsumer)
	is.Equal(retainStorageClass.Annotations[defaultStorageClassAnnotation], "true")

	standardStorageClass, err := manager.clientset.StorageV1().StorageClasses().Get(context.Background(), "standard", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(standardStorageClass.Annotations[defaultStorageClassAnnotation], "false")
}

// TestStorageClassManagerEnsureRetainDefaultRequiresSource verifies that
// EnsureRetainDefault returns an error when no source StorageClass exists.
func TestStorageClassManagerEnsureRetainDefaultRequiresSource(t *testing.T) {
	is := is.New(t)

	manager := StorageClassManager{
		clientset: fake.NewSimpleClientset(),
	}

	err := manager.EnsureRetainDefault(context.Background(), "retain")
	is.Equal(err.Error(), "no storage class is available to build retain")
}

// TestDeploymentManagerWaitForAvailableTimeout verifies that
// WaitForAvailable returns an error when the deployment never becomes
// available before the timeout.
func TestDeploymentManagerWaitForAvailableTimeout(t *testing.T) {
	is := is.New(t)

	manager := DeploymentManager{
		clientset: fake.NewSimpleClientset(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "whoami",
				Namespace: "borq-system",
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := manager.WaitForAvailable(ctx, "borq-system", "whoami")
	is.True(err != nil)
}

// TestStatefulSetManagerWaitForReady verifies that WaitForReady returns nil
// when the StatefulSet has ready replicas.
func TestStatefulSetManagerWaitForReady(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "openbao",
				Namespace: "borq-system",
			},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas: 1,
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := manager.WaitForReady(ctx, "borq-system", "openbao")
	is.NoErr(err)
}

// TestStatefulSetManagerGetListDeleteAndDiff verifies the full CRUD + Diff
// cycle for StatefulSetManager.
func TestStatefulSetManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(
			&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "openbao",
					Namespace: "borq-system",
				},
				Spec: appsv1.StatefulSetSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "openbao"},
					},
				},
			},
		),
	}

	statefulSet, err := manager.Get(context.Background(), "borq-system", "openbao")
	is.NoErr(err)
	is.Equal(statefulSet.Name, "openbao")

	statefulSets, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(statefulSets), 1)

	diff, err := manager.Diff(context.Background(), StatefulSet{Name: "missing", Namespace: "borq-system"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "openbao"))
}

// TestStatefulSetManagerWaitForReadyTimeout verifies that WaitForReady
// returns an error when the StatefulSet never becomes ready before the
// timeout.
func TestStatefulSetManagerWaitForReadyTimeout(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "openbao",
				Namespace: "borq-system",
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := manager.WaitForReady(ctx, "borq-system", "openbao")
	is.True(err != nil)
}

// TestNamespaceManagerGetListDeleteAndDiff verifies the full CRUD + Diff
// cycle for NamespaceManager.
func TestNamespaceManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := NamespaceManager{
		clientset: fake.NewSimpleClientset(
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "borq-system"},
			},
		),
	}

	namespace, err := manager.Get(context.Background(), "borq-system")
	is.NoErr(err)
	is.Equal(namespace.Name, "borq-system")

	namespaces, err := manager.List(context.Background(), "")
	is.NoErr(err)
	is.Equal(len(namespaces), 1)

	diff, err := manager.Diff(context.Background(), Namespace{Name: "missing"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system"))
}

// TestPodManagerGetListDeleteAndDiff verifies the full CRUD + Diff cycle for
// PodManager.
func TestPodManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := PodManager{
		clientset: fake.NewSimpleClientset(
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "debug",
					Namespace: "borq-system",
				},
			},
		),
	}

	pod, err := manager.Get(context.Background(), "borq-system", "debug")
	is.NoErr(err)
	is.Equal(pod.Name, "debug")

	pods, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(pods), 1)

	diff, err := manager.Diff(context.Background(), Pod{Name: "missing", Namespace: "borq-system"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "debug"))
}

// TestMarshalYAMLFailure verifies that marshalYAML returns an error when
// given an unserializable value.
func TestMarshalYAMLFailure(t *testing.T) {
	is := is.New(t)

	_, err := marshalYAML(func() {})
	is.True(err != nil)
}

// TestDiffNormalizedFailure verifies that diffNormalized returns an error
// when either input cannot be marshaled.
func TestDiffNormalizedFailure(t *testing.T) {
	is := is.New(t)

	_, err := diffNormalized(func() {}, Namespace{Name: "ok"})
	is.True(err != nil)
}
