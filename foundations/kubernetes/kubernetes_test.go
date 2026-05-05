package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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

func TestNewConnectionRequiresContextName(t *testing.T) {
	is := is.New(t)

	_, err := NewConnection(Config{}, logger.Logger{})
	is.Equal(err.Error(), "context name must not be empty")
}

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
			Containers: []Container{
				{
					Name:    "whoami",
					Image:   "traefik/whoami:latest",
					Command: []string{"whoami"},
					Args:    []string{"serve"},
					Env:     []EnvVar{{Name: "ENV", Value: "test"}},
					Ports:   []ContainerPort{{Name: "http", Port: 80}},
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

func TestStatefulSetResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	statefulSet := StatefulSet{
		Name:        "vault",
		Namespace:   "borq-system",
		Labels:      map[string]string{"app": "vault"},
		Selector:    map[string]string{"app": "vault"},
		Replicas:    1,
		ServiceName: "vault-internal",
		Pods: PodTemplate{
			Labels: map[string]string{"app": "vault"},
			Containers: []Container{
				{
					Name:  "vault",
					Image: "hashicorp/vault:1.21.2",
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

func TestNamespaceResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	namespace := Namespace{
		Name:   "borq-system",
		Labels: map[string]string{"managed-by": "borq"},
	}

	is.Equal(namespaceFromResource(namespaceToResource(namespace)), namespace)
}

func TestPodResourceRoundTrip(t *testing.T) {
	is := is.New(t)

	pod := Pod{
		Name:      "debug",
		Namespace: "borq-system",
		Labels:    map[string]string{"app": "debug"},
		Spec: PodTemplate{
			Labels: map[string]string{"app": "debug"},
			Containers: []Container{
				{
					Name:  "debug",
					Image: "busybox",
					Args:  []string{"sleep", "10"},
				},
			},
		},
	}

	roundTrip := podFromResource(podToResource(pod))
	is.Equal(roundTrip.Name, pod.Name)
	is.Equal(roundTrip.Namespace, pod.Namespace)
	is.Equal(roundTrip.Labels, pod.Labels)
	is.Equal(roundTrip.Spec.Labels, pod.Spec.Labels)
	is.Equal(roundTrip.Spec.Containers[0].Name, pod.Spec.Containers[0].Name)
	is.Equal(roundTrip.Spec.Containers[0].Image, pod.Spec.Containers[0].Image)
	is.Equal(roundTrip.Spec.Containers[0].Args, pod.Spec.Containers[0].Args)
}

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

func TestStatefulSetManagerWaitForReady(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vault",
				Namespace: "borq-system",
			},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas: 1,
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := manager.WaitForReady(ctx, "borq-system", "vault")
	is.NoErr(err)
}

func TestStatefulSetManagerGetListDeleteAndDiff(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(
			&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vault",
					Namespace: "borq-system",
				},
				Spec: appsv1.StatefulSetSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "vault"},
					},
				},
			},
		),
	}

	statefulSet, err := manager.Get(context.Background(), "borq-system", "vault")
	is.NoErr(err)
	is.Equal(statefulSet.Name, "vault")

	statefulSets, err := manager.List(context.Background(), "borq-system", "")
	is.NoErr(err)
	is.Equal(len(statefulSets), 1)

	diff, err := manager.Diff(context.Background(), StatefulSet{Name: "missing", Namespace: "borq-system"})
	is.NoErr(err)
	is.True(diff.Changed)

	is.NoErr(manager.Delete(context.Background(), "borq-system", "vault"))
}

func TestStatefulSetManagerWaitForReadyTimeout(t *testing.T) {
	is := is.New(t)

	manager := StatefulSetManager{
		clientset: fake.NewSimpleClientset(&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vault",
				Namespace: "borq-system",
			},
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := manager.WaitForReady(ctx, "borq-system", "vault")
	is.True(err != nil)
}

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

func TestMarshalYAMLFailure(t *testing.T) {
	is := is.New(t)

	_, err := marshalYAML(func() {})
	is.True(err != nil)
}

func TestDiffNormalizedFailure(t *testing.T) {
	is := is.New(t)

	_, err := diffNormalized(func() {}, Namespace{Name: "ok"})
	is.True(err != nil)
}
