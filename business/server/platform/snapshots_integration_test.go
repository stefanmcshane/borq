//go:build integration

package platform

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestSnapshotControllerIntegration installs the default upstream chart, observes
// real reconciliation without a CSI driver, and protects existing CRD ownership.
func TestSnapshotControllerIntegration(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	// Kubernetes clients require a pointer to their REST configuration.
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	extensions, err := apiextensionsclient.NewForConfig(&configuration)
	assert.NoErr(err)
	resources, err := dynamic.NewForConfig(&configuration)
	assert.NoErr(err)
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfigPath})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(releases.Close()) })
	namespace := fmt.Sprintf("borq-snapshots-%d", time.Now().UnixNano())
	definitions, err := extensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	for _, definition := range definitions.Items {
		if definition.Spec.Group == "snapshot.storage.k8s.io" || definition.Spec.Group == "groupsnapshot.storage.k8s.io" {
			t.Fatalf("refuse to test with existing snapshot CRD %s", definition.Name)
		}
	}
	conflicts, err := targetComponentConflicts(ctx, connection, releases, namespace)
	assert.NoErr(err)
	if reason := conflicts["snapshot-controller"]; reason != "" {
		t.Fatalf("refuse to test with an existing snapshot controller: %s", reason)
	}
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	foreignNamespace := namespace + "-foreign"
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreignNamespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	helmConfiguration := action.NewConfiguration()
	// Helm's flag adapter requires a pointer to the explicit kubeconfig path.
	assert.NoErr(helmConfiguration.Init(&genericclioptions.ConfigFlags{KubeConfig: &kubeconfigPath}, namespace, "secrets"))
	snapshots := resources.Resource(schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}).Namespace(namespace)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		if err := snapshots.Delete(cleanup, "missing-source", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete test snapshot: %v", err)
		}
		uninstall := action.NewUninstall(helmConfiguration)
		uninstall.WaitStrategy = kube.HookOnlyStrategy
		uninstall.Timeout = 30 * time.Second
		if _, err := uninstall.Run("snapshot-controller"); err != nil && err != driver.ErrReleaseNotFound {
			t.Errorf("uninstall test snapshot controller: %v", err)
		}
		for _, name := range []string{namespace, foreignNamespace} {
			if err := client.CoreV1().Namespaces().Delete(cleanup, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete test namespace %s: %v", name, err)
			}
		}
	})
	var definition component
	for _, candidate := range targetCatalog(false) {
		if candidate.Name == "snapshot-controller" {
			definition = candidate
		}
	}
	assert.Equal(definition.Name, "snapshot-controller")
	values := valuesInput{Settings: Settings{Profile: "dev", Namespace: namespace, Timeout: 3 * time.Minute}}
	first, err := reconcileComponent(ctx, releases, definition, values, false, false)
	assert.NoErr(err)
	assert.Equal(first.Status, "deployed")
	assert.Equal(first.Version, "5.2.0")
	assert.Equal(first.Revision, 1)
	assert.True(first.Changed)
	ownedCrds := map[string]apiextensionsv1.CustomResourceDefinition{}
	for _, name := range []string{"volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io", "volumesnapshotclasses.snapshot.storage.k8s.io"} {
		err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			crd, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			for _, condition := range crd.Status.Conditions {
				if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
					ownedCrds[name] = *crd
					return true, nil
				}
			}
			return false, nil
		})
		assert.NoErr(err)
		assert.Equal(ownedCrds[name].Annotations["meta.helm.sh/release-namespace"], namespace)
	}
	controller, err := client.AppsV1().Deployments(namespace).Get(ctx, "snapshot-controller", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(controller.Status.AvailableReplicas, int32(1))
	assert.Equal(controller.Spec.Template.Spec.Containers[0].Image, "registry.k8s.io/sig-storage/snapshot-controller:v8.6.0")
	classes, err := resources.Resource(schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.Equal(len(classes.Items), 0)
	foreignValues := values
	foreignValues.Namespace = foreignNamespace
	_, err = reconcileComponent(ctx, releases, definition, foreignValues, false, false)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "CustomResourceDefinition"))
	assert.True(strings.Contains(err.Error(), "invalid ownership metadata"))
	for name, before := range ownedCrds {
		after, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(after.UID, before.UID)
		assert.Equal(after.ResourceVersion, before.ResourceVersion)
		assert.Equal(after.Annotations, before.Annotations)
		assert.Equal(after.Labels, before.Labels)
		assert.Equal(after.Spec, before.Spec)
	}
	_, err = snapshots.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]any{"name": "missing-source", "namespace": namespace},
		"spec":     map[string]any{"source": map[string]any{"persistentVolumeClaimName": "missing-pvc"}},
	}}, metav1.CreateOptions{})
	assert.NoErr(err)
	err = wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		snapshot, err := snapshots.Get(ctx, "missing-source", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		message, _, err := unstructured.NestedString(snapshot.Object, "status", "error", "message")
		if err != nil {
			return false, err
		}
		ready, _, err := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
		return strings.Contains(message, "missing-pvc") && !ready, err
	})
	assert.NoErr(err)
	repeated, err := reconcileComponent(ctx, releases, definition, values, false, false)
	assert.NoErr(err)
	assert.Equal(repeated.Status, "deployed")
	assert.Equal(repeated.Revision, first.Revision)
	assert.True(!repeated.Changed)
	after, err := client.AppsV1().Deployments(namespace).Get(ctx, "snapshot-controller", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(after.UID, controller.UID)
}
