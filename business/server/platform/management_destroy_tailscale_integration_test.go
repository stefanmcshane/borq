//go:build integration

package platform

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestManagementDestroyAdminEgressIntegration proves cross-namespace cleanup,
// real StatefulSet/Pod termination and completion gating on proxy state deletion.
// Finalizers are explicit fault fixtures, not a simulated successful Tailscale
// operator: no remote tailnet cleanup or HTTPS reachability is claimed here.
func TestManagementDestroyAdminEgressIntegration(t *testing.T) {
	for _, sameNamespace := range []bool{false, true} {
		name := "separate management namespace"
		if sameNamespace {
			name = "management in borq-system"
		}
		t.Run(name, func(t *testing.T) { testManagementDestroyAdminEgress(t, sameNamespace) })
	}
}

func testManagementDestroyAdminEgress(t *testing.T, sameNamespace bool) {
	path := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if path == "" {
		t.Skip("isolated BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	_, err = client.CoreV1().Namespaces().Get(ctx, SystemNamespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("isolated cluster without borq-system is required")
	}
	_, err = client.RbacV1().ClusterRoleBindings().Get(ctx, "borq", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("isolated cluster without the Borq bootstrap binding is required")
	}
	system, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: SystemNamespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	management := system
	if !sameNamespace {
		management, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "borq-admin-destroy-"}}, metav1.CreateOptions{})
		assert.NoErr(err)
	}
	var bindingUID types.UID
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		// These exact namespaces were created by this test in the isolated cluster.
		// Remove only the named test finalizers before deleting their namespace.
		service, err := client.CoreV1().Services(SystemNamespace).Get(cleanup, managementAdminEgressName, metav1.GetOptions{})
		if err == nil {
			service.Finalizers = nil
			_, _ = client.CoreV1().Services(SystemNamespace).Update(cleanup, service, metav1.UpdateOptions{})
		}
		secret, err := client.CoreV1().Secrets(SystemNamespace).Get(cleanup, "proxy-state-fixture", metav1.GetOptions{})
		if err == nil {
			secret.Finalizers = nil
			_, _ = client.CoreV1().Secrets(SystemNamespace).Update(cleanup, secret, metav1.UpdateOptions{})
		}
		if bindingUID != "" {
			_ = client.RbacV1().ClusterRoleBindings().Delete(cleanup, "borq", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &bindingUID}})
		}
		namespaces := []corev1.Namespace{*system}
		if !sameNamespace {
			namespaces = append(namespaces, *management)
		}
		for _, namespace := range namespaces {
			_ = client.CoreV1().Namespaces().Delete(cleanup, namespace.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &namespace.UID}})
		}
		_ = wait.PollUntilContextCancel(cleanup, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			_, err := client.CoreV1().Namespaces().Get(ctx, SystemNamespace, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
	})
	one := int32(1)
	core, err := client.AppsV1().Deployments(management.Name).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: destroyTestLabels("borq", "server")}, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "borq-core-fixture"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "borq-core-fixture"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "registry.k8s.io/pause:3.10"}}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	binding, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: destroyTestLabels("borq", "rbac")}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "borq"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "borq", Namespace: management.Name}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	bindingUID = binding.UID
	metadata := networkMetadata(management.Name, "borq-management")
	metadata.Annotations = map[string]string{"borq.run/proven-tailnet-url": "https://borq-production-5.example.ts.net"}
	_, err = client.NetworkingV1().Ingresses(management.Name).Create(ctx, &networkingv1.Ingress{ObjectMeta: metadata, Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "borq", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	admin := &corev1.Service{}
	fixture := destroyAdminEgressFixture()
	assert.NoErr(runtime.DefaultUnstructuredConverter.FromUnstructured(fixture.Object, admin))
	admin.UID = ""
	admin.ResourceVersion = ""
	admin.CreationTimestamp = metav1.Time{}
	admin.Finalizers = []string{"testing.borq.io/tailscale-cleanup"}
	_, err = client.CoreV1().Services(SystemNamespace).Create(ctx, admin, metav1.CreateOptions{})
	assert.NoErr(err)
	children := map[string]string{"tailscale.com/managed": "true", "tailscale.com/parent-resource": managementAdminEgressName, "tailscale.com/parent-resource-ns": SystemNamespace, "tailscale.com/parent-resource-type": "svc", "app": "egress-fixture"}
	_, err = client.CoreV1().Services(SystemNamespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "proxy-headless-fixture", Labels: children}, Spec: corev1.ServiceSpec{ClusterIP: "None", Selector: map[string]string{"app": "egress-fixture"}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	proxy, err := client.AppsV1().StatefulSets(SystemNamespace).Create(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "proxy-fixture", Labels: children}, Spec: appsv1.StatefulSetSpec{ServiceName: "proxy-headless-fixture", Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "egress-fixture"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: children}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "registry.k8s.io/pause:3.10"}}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Secrets(SystemNamespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "proxy-state-fixture", Labels: children, Finalizers: []string{"testing.borq.io/proxy-state"}}, Data: map[string][]byte{"fixture": []byte("test-only")}}, metav1.CreateOptions{})
	assert.NoErr(err)
	foreign, err := client.CoreV1().Secrets(SystemNamespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-proxy-state", Labels: map[string]string{"tailscale.com/managed": "true", "tailscale.com/parent-resource": "other-service", "tailscale.com/parent-resource-ns": SystemNamespace, "tailscale.com/parent-resource-type": "svc"}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	assert.NoErr(wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current, err := client.AppsV1().StatefulSets(SystemNamespace).Get(ctx, proxy.Name, metav1.GetOptions{})
		return err == nil && current.Status.ReadyReplicas == 1, err
	}))
	input := ManagementDestroyInput{KubeconfigPath: path, Namespace: management.Name, OperationID: "egress-" + management.Name, Timeout: time.Minute}
	plan, err := PlanManagementDestroy(ctx, input)
	assert.NoErr(err)
	found := false
	for _, identity := range plan.Resources {
		if identity.Namespace == SystemNamespace && identity.Name == managementAdminEgressName {
			found = true
		}
	}
	assert.True(found)
	done := make(chan error, 1)
	go func() { _, err := DestroyManagementResources(ctx, input, plan); done <- err }()
	assert.NoErr(wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current, err := client.CoreV1().Services(SystemNamespace).Get(ctx, managementAdminEgressName, metav1.GetOptions{})
		return err == nil && current.DeletionTimestamp != nil, err
	}))
	current, err := client.CoreV1().Services(SystemNamespace).Get(ctx, managementAdminEgressName, metav1.GetOptions{})
	assert.NoErr(err)
	current.Finalizers = nil
	_, err = client.CoreV1().Services(SystemNamespace).Update(ctx, current, metav1.UpdateOptions{})
	assert.NoErr(err)
	propagation := metav1.DeletePropagationForeground
	assert.NoErr(client.AppsV1().StatefulSets(SystemNamespace).Delete(ctx, proxy.Name, metav1.DeleteOptions{PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &proxy.UID}}))
	assert.NoErr(wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		pods, err := client.CoreV1().Pods(SystemNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=egress-fixture"})
		return err == nil && len(pods.Items) == 0, err
	}))
	assert.NoErr(client.CoreV1().Services(SystemNamespace).Delete(ctx, "proxy-headless-fixture", metav1.DeleteOptions{}))
	assert.NoErr(client.CoreV1().Secrets(SystemNamespace).Delete(ctx, "proxy-state-fixture", metav1.DeleteOptions{}))
	select {
	case err := <-done:
		t.Fatalf("server completed before proxy state cleanup: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_, err = ReadManagementDestroyReceipt(ctx, input)
	assert.True(apierrors.IsNotFound(err))
	retainedCore, err := client.AppsV1().Deployments(management.Name).Get(ctx, core.Name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retainedCore.UID, core.UID)
	assert.Equal(*retainedCore.Spec.Replicas, int32(1))
	secret, err := client.CoreV1().Secrets(SystemNamespace).Get(ctx, "proxy-state-fixture", metav1.GetOptions{})
	assert.NoErr(err)
	secret.Finalizers = nil
	_, err = client.CoreV1().Secrets(SystemNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	assert.NoErr(err)
	assert.NoErr(<-done)
	_, err = ReadManagementDestroyReceipt(ctx, input)
	assert.NoErr(err)
	other, err := client.CoreV1().Secrets(SystemNamespace).Get(ctx, foreign.Name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(other.UID, foreign.UID)
	_, err = client.CoreV1().Services(SystemNamespace).Get(ctx, managementAdminEgressName, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
}
