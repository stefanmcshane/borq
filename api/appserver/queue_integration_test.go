//go:build integration

package appserver_test

import (
	"bytes"
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
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// TestApplicationIntegrationQueue verifies applies run in accepted order, survive
// restart, and allow another space to deploy while the first is gated.
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
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: path, RolloutTimeout: 70 * time.Second, PollInterval: 100 * time.Millisecond})
	assert.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); assert.NoErr(manager.Close()) })
	client, err := appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	organization := fmt.Sprintf("queue-%x", time.Now().UnixNano())
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
	organizationResponse, err := client.PutOrganizationWithResponse(ctx, organization)
	assert.NoErr(err)
	assertApplicationResponse(t, organizationResponse.StatusCode(), organizationResponse.Body, http.StatusOK)
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
	b := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "worker", "replicas": 1, "predeploy": map[string]any{"command": ""},
	}}}))
	operationA := startPredeployOperation(t, ctx, client, a.ID)
	_, pod := waitPredeployRunning(t, ctx, kube, namespace)
	operationB := startPredeployOperation(t, ctx, client, b.ID)
	assert.Equal(string(operationB.Status), "queued")
	assertPredeployAppsUnchanged(t, baseline, predeployDeployments(t, ctx, kube, namespace))

	t.Log("a different space can finish while A gates B")
	zero := 0
	otherApps := []appapi.AppPatch{{Name: "worker", Image: &image, Replicas: &zero}}
	otherPlan, err := client.PostPlanWithResponse(ctx, organization, "staging", appapi.Patch{Apps: &otherApps})
	assert.NoErr(err)
	assertApplicationResponse(t, otherPlan.StatusCode(), otherPlan.Body, http.StatusCreated)
	otherNamespace = otherPlan.JSON201.Values["namespace"].(string)
	applyApplicationPlan(t, ctx, client, otherPlan.JSON201.ID)
	stillQueued, err := client.GetOperationWithResponse(ctx, operationB.ID)
	assert.NoErr(err)
	assert.Equal(string(stillQueued.JSON200.Status), "queued")

	t.Log("both active and queued exact plans survive server restart")
	server.Close()
	assert.NoErr(manager.Close())
	manager, err = apps.NewManager(ctx, apps.NewInput{DatabasePath: path, RolloutTimeout: 70 * time.Second, PollInterval: 100 * time.Millisecond})
	assert.NoErr(err)
	handler, err = appapi.NewHandler(manager)
	assert.NoErr(err)
	server = httptest.NewServer(handler)
	client, err = appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	repeated := startPredeployOperation(t, ctx, client, b.ID)
	assert.Equal(repeated.ID, operationB.ID)
	assert.Equal(string(repeated.Status), "queued")
	request := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "worker", Command: []string{"touch", "/tmp/borq-continue"}, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
	assert.NoErr(err)
	var output bytes.Buffer
	assert.NoErr(executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output, Stderr: &output}))
	waitPredeployOperation(t, ctx, client, operationA.ID, appapi.Succeeded)
	waitPredeployOperation(t, ctx, client, operationB.ID, appapi.Succeeded)
	final, err := client.GetSpaceWithResponse(ctx, organization, "production")
	assert.NoErr(err)
	assert.Equal(final.JSON200.Revision, 3)
	assert.Equal(final.JSON200.LastSuccessfulRevision, 3)
	assert.Equal(final.JSON200.Configuration, b.Configuration)
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "worker", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(deployment.Spec.Replicas, int32Pointer(1))
}
