//go:build integration

package platform

import (
	"context"
	"errors"
	"os"
	"strings"
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestManagementDestroyIntegration proves real controllers stop only managed
// workloads, records completion after finalizer waits, and fences core removal.
// Finalizers are explicit fixtures; no cloud load-balancer or VPN success is fabricated.
func TestManagementDestroyIntegration(t *testing.T) {
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
	namespace, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "borq-destroy-"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	var bindingUID types.UID
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if bindingUID != "" {
			_ = client.RbacV1().ClusterRoleBindings().Delete(cleanup, "borq", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &bindingUID}})
		}
		_ = client.CoreV1().Namespaces().Delete(cleanup, namespace.Name, metav1.DeleteOptions{})
	})
	one := int32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: namespace.Name, Labels: destroyTestLabels("borq", "server")}, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "destroy-borq"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "destroy-borq"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "registry.k8s.io/pause:3.10"}}}}}}
	originalServer, err := client.AppsV1().Deployments(namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
	assert.NoErr(err)
	pocketName := "pocketid-" + strings.Repeat("a", 32)
	pocket := deployment.DeepCopy()
	pocket.Name = pocketName
	pocket.Labels = map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": "pocket-id", "borq.io/pocket-id": strings.Repeat("a", 32)}
	pocket.Annotations = map[string]string{"borq.io/pocket-id-issuer": "https://id.destroy.test"}
	pocket.Spec.Selector.MatchLabels = map[string]string{"app": "destroy-pocket"}
	pocket.Spec.Template.Labels = map[string]string{"app": "destroy-pocket"}
	_, err = client.AppsV1().Deployments(namespace.Name).Create(ctx, pocket, metav1.CreateOptions{})
	assert.NoErr(err)
	for _, name := range []string{"borq", pocketName} {
		assert.NoErr(wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			current, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, name, metav1.GetOptions{})
			return err == nil && current.Status.ReadyReplicas == 1, err
		}))
	}
	for _, name := range []string{"borq", "borq-telemetry"} {
		object := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Labels: destroyTestLabels("borq", "server")}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
		if name == "borq" {
			object.Finalizers = []string{"testing.borq.io/core-cleanup"}
		}
		_, err = client.CoreV1().Services(namespace.Name).Create(ctx, object, metav1.CreateOptions{})
		assert.NoErr(err)
	}
	_, err = client.CoreV1().ServiceAccounts(namespace.Name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: destroyTestLabels("borq", "rbac")}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Services(namespace.Name).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: pocketName, Labels: pocket.Labels, Annotations: pocket.Annotations, Finalizers: []string{"testing.borq.io/provider-cleanup"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 1411}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	networkLabels := map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "borq.run/network": "true"}
	_, err = client.NetworkingV1().Ingresses(namespace.Name).Create(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "borq-management", Labels: networkLabels, Finalizers: []string{"testing.borq.io/route-cleanup"}}, Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "borq", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Services(namespace.Name).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Labels: map[string]string{"app.kubernetes.io/managed-by": "someone-else"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().ConfigMaps(namespace.Name).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-settings", Labels: destroyTestLabels("borq-network-settings", "network")}, Data: map[string]string{"network.json": `{"tailscale":{"managementHostname":"destroy-test","operatorTag":"tag:borq-operator","proxyTag":"tag:borq-proxy"}}`}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Secrets(namespace.Name).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "retained-credentials"}, Data: map[string][]byte{"token": []byte("fixture-private-token")}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().PersistentVolumeClaims(namespace.Name).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained-data"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	binding, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: destroyTestLabels("borq", "rbac")}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "borq"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "borq", Namespace: namespace.Name}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	bindingUID = binding.UID
	input := ManagementDestroyInput{KubeconfigPath: path, Namespace: namespace.Name, OperationID: "integration-" + namespace.Name, Timeout: time.Minute}
	plan, err := PlanManagementDestroy(ctx, input)
	assert.NoErr(err)
	assert.True(!plan.Executed)
	assert.Equal(plan.NamespaceUID, string(namespace.UID))
	assert.Equal(len(plan.Server), 5)
	assert.Equal(len(plan.Dependencies), 1)
	_, err = FinalizeManagementServer(ctx, input, ManagementDestroyReceipt{OperationID: input.OperationID, Plan: plan})
	assert.True(apierrors.IsNotFound(err))
	done := make(chan error, 1)
	go func() { _, err := DestroyManagementResources(ctx, input, plan); done <- err }()
	for _, kind := range []string{"ingress", "pocket service"} {
		assert.NoErr(wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			if kind == "ingress" {
				object, err := client.NetworkingV1().Ingresses(namespace.Name).Get(ctx, "borq-management", metav1.GetOptions{})
				return err == nil && object.DeletionTimestamp != nil, err
			}
			object, err := client.CoreV1().Services(namespace.Name).Get(ctx, pocketName, metav1.GetOptions{})
			return err == nil && object.DeletionTimestamp != nil, err
		}))
		select {
		case err := <-done:
			t.Fatalf("server cleanup skipped its finalizer wait: %v", err)
		default:
		}
		_, err := ReadManagementDestroyReceipt(ctx, input)
		assert.True(apierrors.IsNotFound(err))
		running, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(running.UID, originalServer.UID)
		assert.Equal(*running.Spec.Replicas, int32(1))
		core, err := client.CoreV1().Services(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.True(core.DeletionTimestamp == nil)
		if kind == "ingress" {
			object, err := client.NetworkingV1().Ingresses(namespace.Name).Get(ctx, "borq-management", metav1.GetOptions{})
			assert.NoErr(err)
			object.Finalizers = nil
			_, err = client.NetworkingV1().Ingresses(namespace.Name).Update(ctx, object, metav1.UpdateOptions{})
			assert.NoErr(err)
		} else {
			object, err := client.CoreV1().Services(namespace.Name).Get(ctx, pocketName, metav1.GetOptions{})
			assert.NoErr(err)
			object.Finalizers = nil
			_, err = client.CoreV1().Services(namespace.Name).Update(ctx, object, metav1.UpdateOptions{})
			assert.NoErr(err)
		}
	}
	assert.NoErr(<-done)
	receipt, err := ReadManagementDestroyReceipt(ctx, input)
	assert.NoErr(err)
	assert.True(receipt.Plan.Executed)
	record, err := client.CoreV1().ConfigMaps(namespace.Name).Get(ctx, "borq-management-destroy", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(record.Immutable != nil && *record.Immutable)
	record.Data["receipt.json"] = "{}"
	_, err = client.CoreV1().ConfigMaps(namespace.Name).Update(ctx, record, metav1.UpdateOptions{})
	assert.True(err != nil)
	verified, err := ReadManagementDestroyReceipt(ctx, input)
	assert.NoErr(err)
	assert.Equal(verified.OperationID, input.OperationID)
	retry, err := DestroyManagementResources(ctx, input, plan)
	assert.NoErr(err)
	assert.True(retry.Executed)
	settings, err := client.CoreV1().ConfigMaps(namespace.Name).Get(ctx, "borq-network-settings", metav1.GetOptions{})
	assert.NoErr(err)
	originalNetwork := settings.Data["network.json"]
	settings.Data["network.json"] = `{"tailscale":{"managementHostname":"replacement-host","operatorTag":"tag:borq-operator","proxyTag":"tag:borq-proxy"}}`
	_, err = client.CoreV1().ConfigMaps(namespace.Name).Update(ctx, settings, metav1.UpdateOptions{})
	assert.NoErr(err)
	_, err = FinalizeManagementServer(ctx, input, receipt)
	assert.True(err != nil && strings.Contains(err.Error(), "retained provider configuration changed"))
	protected, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(*protected.Spec.Replicas, int32(1))
	settings, err = client.CoreV1().ConfigMaps(namespace.Name).Get(ctx, "borq-network-settings", metav1.GetOptions{})
	assert.NoErr(err)
	settings.Data["network.json"] = originalNetwork
	_, err = client.CoreV1().ConfigMaps(namespace.Name).Update(ctx, settings, metav1.UpdateOptions{})
	assert.NoErr(err)
	go func() { _, err := FinalizeManagementServer(ctx, input, receipt); done <- err }()
	assert.NoErr(wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		object, err := client.CoreV1().Services(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		return err == nil && object.DeletionTimestamp != nil, err
	}))
	select {
	case err := <-done:
		t.Fatalf("core finalizer skipped its finalizer wait: %v", err)
	default:
	}
	core, err := client.CoreV1().Services(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
	assert.NoErr(err)
	core.Finalizers = nil
	_, err = client.CoreV1().Services(namespace.Name).Update(ctx, core, metav1.UpdateOptions{})
	assert.NoErr(err)
	assert.NoErr(<-done)
	_, err = FinalizeManagementServer(ctx, input, receipt)
	assert.NoErr(err)
	for _, name := range []string{"borq", "borq-telemetry"} {
		_, err = client.CoreV1().Services(namespace.Name).Get(ctx, name, metav1.GetOptions{})
		assert.True(apierrors.IsNotFound(err))
	}
	_, err = client.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	_, err = client.RbacV1().ClusterRoleBindings().Get(ctx, "borq", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	for _, name := range []string{"foreign"} {
		_, err = client.CoreV1().Services(namespace.Name).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
	}
	_, err = client.CoreV1().PersistentVolumeClaims(namespace.Name).Get(ctx, "retained-data", metav1.GetOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Secrets(namespace.Name).Get(ctx, "retained-credentials", metav1.GetOptions{})
	assert.NoErr(err)
	t.Run("new bootstrap cannot consume old receipt", func(t *testing.T) {
		assert := is.New(t)
		replacement, err := client.AppsV1().Deployments(namespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
		assert.NoErr(err)
		assert.True(replacement.UID != originalServer.UID)
		recreated, err := ManagementServerRecreated(ctx, input, plan)
		assert.NoErr(err)
		assert.True(recreated)
		_, err = ReadManagementDestroyReceipt(ctx, input)
		assert.True(errors.Is(err, ErrManagementDestroyReceiptStale))
		_, err = FinalizeManagementServer(ctx, input, receipt)
		assert.True(errors.Is(err, ErrManagementDestroyReceiptStale))
		saved, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(saved.UID, replacement.UID)
		assert.Equal(*saved.Spec.Replicas, int32(1))
		fresh, err := PlanManagementDestroy(ctx, input)
		assert.NoErr(err)
		next := input
		next.OperationID += "-next"
		completed, err := ManagementDestroyCompleted(ctx, next, fresh)
		assert.NoErr(err)
		assert.True(!completed)
		_, err = DestroyManagementResources(ctx, next, fresh)
		assert.NoErr(err)
		current, err := ReadManagementDestroyReceipt(ctx, next)
		assert.NoErr(err)
		assert.Equal(current.OperationID, next.OperationID)
		completed, err = ManagementDestroyCompleted(ctx, next, fresh)
		assert.NoErr(err)
		assert.True(completed)
		_, err = FinalizeManagementServer(ctx, next, current)
		assert.NoErr(err)
	})
	t.Run("orphaned tunnel receipt requires provider metadata", func(t *testing.T) {
		assert := is.New(t)
		_, err := client.CoreV1().ConfigMaps(namespace.Name).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-cloudflared", Labels: networkLabels}, Data: map[string]string{"tunnelId": "retained-recovery-receipt"}}, metav1.CreateOptions{})
		assert.NoErr(err)
		_, err = PlanManagementDestroy(ctx, input)
		assert.True(err != nil && strings.Contains(err.Error(), "Cloudflare"))
		assert.NoErr(client.CoreV1().ConfigMaps(namespace.Name).Delete(ctx, "borq-network-settings", metav1.DeleteOptions{}))
		_, err = PlanManagementDestroy(ctx, input)
		assert.True(err != nil && strings.Contains(err.Error(), "network settings missing"))
		_, err = client.CoreV1().ConfigMaps(namespace.Name).Get(ctx, "borq-cloudflared", metav1.GetOptions{})
		assert.NoErr(err)
		assert.NoErr(client.CoreV1().ConfigMaps(namespace.Name).Delete(ctx, "borq-cloudflared", metav1.DeleteOptions{}))
	})
}
