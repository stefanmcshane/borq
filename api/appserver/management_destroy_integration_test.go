//go:build integration

package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestManagementDestroyServerIntegration drives authenticated HTTP, durable
// manager state and real Kubernetes teardown together. The server retains its
// running core through cleanup and restart; only receipt-based CLI finalization
// removes it. This checks local resource lifecycle, not cloud-provider cleanup
// or a complete management component installation.
func TestManagementDestroyServerIntegration(t *testing.T) {
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
	kube, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	_, err = kube.RbacV1().ClusterRoleBindings().Get(ctx, "borq", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("isolated cluster must not contain the global borq binding before this test: %v", err)
	}
	namespace, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "borq-server-destroy-"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	var bindingUID types.UID
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if bindingUID != "" {
			_ = kube.RbacV1().ClusterRoleBindings().Delete(cleanup, "borq", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &bindingUID}})
		}
		_ = kube.CoreV1().Namespaces().Delete(cleanup, namespace.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &namespace.UID}})
	})
	labels := func(component string) map[string]string {
		return map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": "borq", "app.kubernetes.io/component": component}
	}
	createCore := func() map[string]types.UID {
		one := int32(1)
		deployment, err := kube.AppsV1().Deployments(namespace.Name).Create(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: labels("server")},
			Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "borq-destroy-server"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "borq-destroy-server"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "registry.k8s.io/pause:3.10"}}}}},
		}, metav1.CreateOptions{})
		assert.NoErr(err)
		identities := map[string]types.UID{"deployment": deployment.UID}
		for _, name := range []string{"borq", "borq-telemetry"} {
			service, err := kube.CoreV1().Services(namespace.Name).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels("server")}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
			assert.NoErr(err)
			identities[name] = service.UID
		}
		account, err := kube.CoreV1().ServiceAccounts(namespace.Name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: labels("rbac")}}, metav1.CreateOptions{})
		assert.NoErr(err)
		identities["account"] = account.UID
		binding, err := kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "borq", Labels: labels("rbac")},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "borq"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "borq", Namespace: namespace.Name}}}, metav1.CreateOptions{})
		assert.NoErr(err)
		bindingUID = binding.UID
		identities["binding"] = binding.UID
		assert.NoErr(wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			current, err := kube.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
			return err == nil && current.Status.ReadyReplicas == 1, err
		}))
		return identities
	}
	original := createCore()
	assertCore := func() {
		deployment, err := kube.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(deployment.UID, original["deployment"])
		assert.True(deployment.DeletionTimestamp == nil && deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 1 && deployment.Status.ReadyReplicas == 1)
		for _, name := range []string{"borq", "borq-telemetry"} {
			service, err := kube.CoreV1().Services(namespace.Name).Get(ctx, name, metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(service.UID, original[name])
			assert.True(service.DeletionTimestamp == nil)
		}
		account, err := kube.CoreV1().ServiceAccounts(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(account.UID, original["account"])
		binding, err := kube.RbacV1().ClusterRoleBindings().Get(ctx, "borq", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(binding.UID, original["binding"])
	}
	networkLabels := map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "borq.run/network": "true"}
	_, err = kube.NetworkingV1().Ingresses(namespace.Name).Create(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "borq-management", Labels: networkLabels},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "borq", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().Services(namespace.Name).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "borq-management", Labels: networkLabels}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
	assert.NoErr(err)

	directory := t.TempDir()
	token := strings.Repeat("s", 64)
	tokenFile := filepath.Join(directory, "bootstrap-token")
	assert.NoErr(os.WriteFile(tokenFile, []byte(token), 0600))
	input := NewInput{Port: 7833, TelemetryPort: 7834, DataDir: directory, AdminTokenFile: tokenFile, InfrastructureEnabled: true, ManagementEnabled: true,
		ManagementKubeconfigPath: path, RolloutTimeout: 30 * time.Second,
		PlatformSettings: platform.Settings{Profile: "dev", Namespace: namespace.Name, OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://borq.test:7834"}}
	openServer := func() (Server, *appapi.ClientWithResponses, string, func()) {
		server, err := New(ctx, input)
		assert.NoErr(err)
		// Serving the actual handler preserves auth and API composition without
		// starting unrelated Helm installation against this isolated fixture.
		httpServer := httptest.NewServer(server.http.Handler)
		client, err := appapi.NewClientWithResponses(httpServer.URL, appapi.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
		assert.NoErr(err)
		closed := false
		stop := func() {
			if closed {
				return
			}
			closed = true
			httpServer.Close()
			assert.NoErr(server.Close())
		}
		t.Cleanup(stop)
		return server, client, httpServer.URL, stop
	}
	assertReady := func(address string) {
		for _, path := range []string{"/api/readyz", "/api/livez"} {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+path, nil)
			assert.NoErr(err)
			response, err := http.DefaultClient.Do(request)
			assert.NoErr(err)
			assert.Equal(response.StatusCode, http.StatusOK)
			assert.NoErr(response.Body.Close())
		}
	}
	_, client, address, stop := openServer()
	planned, err := client.PlanManagementDestroyWithResponse(ctx)
	assert.NoErr(err)
	if planned.JSON200 == nil {
		t.Fatalf("plan returned %d: %s", planned.StatusCode(), planned.Body)
	}
	accepted, err := client.DestroyManagementWithResponse(ctx)
	assert.NoErr(err)
	if accepted.JSON202 == nil {
		t.Fatalf("submit returned %d: %s", accepted.StatusCode(), accepted.Body)
	}
	operation := accepted.JSON202.ID
	assert.True(operation != "")
	assert.NoErr(wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		assertReady(address)
		assertCore()
		ordinary, err := client.ListOrganizationsWithResponse(ctx)
		if err != nil {
			return false, err
		}
		if ordinary.StatusCode() != http.StatusConflict {
			return false, fmt.Errorf("ordinary API returned %d during teardown", ordinary.StatusCode())
		}
		status, err := client.GetManagementDestroyWithResponse(ctx, operation)
		if err != nil {
			return false, err
		}
		if status.JSON200 == nil {
			return false, fmt.Errorf("progress returned %d: %s", status.StatusCode(), status.Body)
		}
		if status.JSON200.Operation.Status == appapi.Failed {
			return false, fmt.Errorf("server teardown failed: %s", status.JSON200.Operation.Message)
		}
		return status.JSON200.Operation.Status == appapi.Succeeded, nil
	}))
	for _, kind := range []string{"ingress", "service"} {
		if kind == "ingress" {
			_, err = kube.NetworkingV1().Ingresses(namespace.Name).Get(ctx, "borq-management", metav1.GetOptions{})
		} else {
			_, err = kube.CoreV1().Services(namespace.Name).Get(ctx, "borq-management", metav1.GetOptions{})
		}
		assert.True(apierrors.IsNotFound(err))
	}
	assertCore()
	stop()

	// Reproduce a crash after the genuine immutable Kubernetes receipt was
	// committed but before the terminal operation/task transaction reached SQLite.
	database, err := sqlite.New(ctx, filepath.Join(directory, "borq.sqlite"))
	assert.NoErr(err)
	var interrupted []sqlite.Write
	for _, collection := range []string{"management-operation", "management-destroy-task"} {
		record, err := database.Get(ctx, collection, operation)
		assert.NoErr(err)
		var document map[string]json.RawMessage
		assert.NoErr(json.Unmarshal(record.Value, &document))
		if collection == "management-operation" {
			document["status"] = json.RawMessage(`"running"`)
		} else {
			var plan platform.ManagementDestroyPlan
			assert.NoErr(json.Unmarshal(document["plan"], &plan))
			plan.Executed = false
			document["plan"], err = json.Marshal(plan)
			assert.NoErr(err)
		}
		encoded, err := json.Marshal(document)
		assert.NoErr(err)
		interrupted = append(interrupted, sqlite.Write{Collection: collection, Key: operation, Value: encoded, ExpectedRevision: record.Revision})
	}
	assert.NoErr(database.Commit(ctx, interrupted))
	assert.NoErr(database.Close())

	reopened, client, address, stop := openServer()
	reopened.management.start()
	assert.Equal(reopened.management.snapshot().Phase, "destroying")
	assert.Equal(reopened.management.snapshot().Attempt, 0)
	assertReady(address)
	ordinary, err := client.ListOrganizationsWithResponse(ctx)
	assert.NoErr(err)
	assert.Equal(ordinary.StatusCode(), http.StatusConflict)
	status, err := client.GetManagementDestroyWithResponse(ctx, operation)
	assert.NoErr(err)
	assert.True(status.JSON200 != nil && status.JSON200.Operation.Status == appapi.Succeeded && status.JSON200.Plan.Executed)
	retried, err := client.DestroyManagementWithResponse(ctx)
	assert.NoErr(err)
	assert.True(retried.JSON202 != nil && retried.JSON202.ID == operation)
	assertCore()
	stop()

	finalize := platform.ManagementDestroyInput{KubeconfigPath: path, Namespace: namespace.Name, OperationID: operation, Timeout: time.Minute}
	receipt, err := platform.ReadManagementDestroyReceipt(ctx, finalize)
	assert.NoErr(err)
	_, err = platform.FinalizeManagementServer(ctx, finalize, receipt)
	assert.NoErr(err)
	_, err = kube.AppsV1().Deployments(namespace.Name).Get(ctx, "borq", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	fresh := createCore()
	assert.True(fresh["deployment"] != original["deployment"])
	freshServer, client, address, _ := openServer()
	assert.True(!freshServer.manager.ManagementDestroyActive())
	assertReady(address)
	ordinary, err = client.ListOrganizationsWithResponse(ctx)
	assert.NoErr(err)
	assert.Equal(ordinary.StatusCode(), http.StatusOK)
}
