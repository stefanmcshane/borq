//go:build integration

package appserver_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestApplicationIntegrationQueue proves replacement and manual cancellation stop
// real predeploy Jobs, preserve live apps, and remain terminal after server restart.
func TestApplicationIntegrationQueue(t *testing.T) {
	assert := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required")
	}
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(config)
	assert.NoErr(err)
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	organization := fmt.Sprintf("queue-%x", time.Now().UnixNano())
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: path, RolloutTimeout: 70 * time.Second, PollInterval: 100 * time.Millisecond})
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, organization)
	assert.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); assert.NoErr(manager.Close()) })
	client, err := appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	namespace, otherNamespace := "", ""
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range []string{namespace, otherNamespace} {
			if name == "" {
				continue
			}
			if err := kube.CoreV1().Namespaces().Delete(cleanup, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("clean up queue namespace: %v", err)
			}
		}
	})
	cluster, err := client.PutClusterWithResponse(ctx, organization, "local", appapi.ClusterInput{KubeconfigPath: &kubeconfigPath})
	assert.NoErr(err)
	assertApplicationResponse(t, cluster.StatusCode(), cluster.Body, http.StatusOK)
	for _, name := range []string{"production", "staging"} {
		response, err := client.PutSpaceWithResponse(ctx, organization, name, appapi.SpaceInput{Cluster: "local"})
		assert.NoErr(err)
		assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusOK)
	}
	initial := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{predeployInitialApp("worker", image)}}))
	namespace = initial.Values["namespace"].(string)
	applyApplicationPlan(t, ctx, client, initial.ID)
	baseline := predeployDeployments(t, ctx, kube, namespace)
	a := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "worker", "replicas": 2, "predeploy": map[string]any{"command": predeployShellCommand("while [ ! -f /tmp/borq-continue ]; do sleep 1; done"), "timeoutSeconds": 60},
	}}}))
	// An abandoned plan takes no place in the execution queue.
	_ = createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{map[string]any{"name": "worker", "replicas": 4}}}))
	operationA := startPredeployOperation(t, ctx, client, a.ID)
	jobA, _ := waitPredeployRunning(t, ctx, kube, namespace)
	assertPredeployAppsUnchanged(t, baseline, predeployDeployments(t, ctx, kube, namespace))

	t.Log("a different space can finish while the first is waiting")
	zero := 0
	otherApps := []appapi.AppPatch{{Name: "worker", Image: &image, Replicas: &zero}}
	otherPlan, err := client.PostPlanWithResponse(ctx, organization, "staging", appapi.Patch{Apps: &otherApps})
	assert.NoErr(err)
	assertApplicationResponse(t, otherPlan.StatusCode(), otherPlan.Body, http.StatusCreated)
	otherNamespace = otherPlan.JSON201.Values["namespace"].(string)
	applyApplicationPlan(t, ctx, client, otherPlan.JSON201.ID)

	t.Log("a new apply replaces the blocked predeploy and completes")
	b := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "worker", "replicas": 1, "predeploy": map[string]any{"command": ""},
	}}}))
	operationB := startPredeployOperation(t, ctx, client, b.ID)
	waitPredeployOperation(t, ctx, client, operationA.ID, appapi.Superseded)
	waitPredeployOperation(t, ctx, client, operationB.ID, appapi.Succeeded)
	waitQueueJobStopped(t, ctx, kube, namespace, jobA.Name)
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "worker", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(deployment.Spec.Replicas, int32Pointer(1))

	t.Log("manual cancellation releases a hung deployment without changing live apps")
	c := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "worker", "replicas": 2, "predeploy": map[string]any{"command": predeployShellCommand("sleep 120"), "timeoutSeconds": 180},
	}}}))
	operationC := startPredeployOperation(t, ctx, client, c.ID)
	jobC, _ := waitPredeployRunning(t, ctx, kube, namespace)
	canceled, err := client.CancelOperationWithResponse(ctx, operationC.ID)
	assert.NoErr(err)
	assertApplicationResponse(t, canceled.StatusCode(), canceled.Body, http.StatusOK)
	waitPredeployOperation(t, ctx, client, operationC.ID, appapi.Canceled)
	waitQueueJobStopped(t, ctx, kube, namespace, jobC.Name)
	deployment, err = kube.AppsV1().Deployments(namespace).Get(ctx, "worker", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(deployment.Spec.Replicas, int32Pointer(1))

	t.Log("canceled and superseded operations remain terminal after restart")
	server.Close()
	assert.NoErr(manager.Close())
	manager, err = apps.NewManager(ctx, apps.NewInput{DatabasePath: path, RolloutTimeout: 70 * time.Second, PollInterval: 100 * time.Millisecond})
	assert.NoErr(err)
	handler, err = appapi.NewHandler(manager)
	assert.NoErr(err)
	server = httptest.NewServer(handler)
	client, err = appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	for id, status := range map[string]appapi.OperationStatus{operationA.ID: appapi.Superseded, operationC.ID: appapi.Canceled} {
		repeated := startPredeployOperation(t, ctx, client, id)
		assert.Equal(repeated.Status, status)
	}
	final, err := client.GetSpaceWithResponse(ctx, organization, "production")
	assert.NoErr(err)
	assert.Equal(final.JSON200.Revision, 4)
	assert.Equal(final.JSON200.LastSuccessfulRevision, 3)
	assert.Equal(final.JSON200.Configuration, c.Configuration)
}

// SDK clients share transports through pointers. Controller readback proves the
// suspended Job has stopped its active pods, rather than only accepting a patch.
func waitQueueJobStopped(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	for {
		job, err := kube.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + name})
		if err != nil {
			t.Fatal(err)
		}
		active := false
		for _, pod := range pods.Items {
			active = active || pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending
		}
		if job.Spec.Suspend != nil && *job.Spec.Suspend && job.Status.Active == 0 && !active {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("canceled predeploy Job did not stop its pods")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
