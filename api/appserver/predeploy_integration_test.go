//go:build integration

package appserver_test

import (
	"bytes"
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
	"github.com/stefanmcshane/borq/business/server/apps"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// TestPredeployIntegration verifies that real Kubernetes Jobs gate the complete
// application rollout, survive a Borq restart, and fail without changing apps.
func TestPredeployIntegration(t *testing.T) {
	is := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for predeploy integration tests")
	}
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	is.NoErr(err)
	kube, err := kubernetes.NewForConfig(kubeconfig)
	is.NoErr(err)
	databasePath := filepath.Join(t.TempDir(), "predeploy.sqlite")
	manager, err := apps.NewManager(ctx, apps.NewInput{
		DatabasePath: databasePath, RolloutTimeout: 90 * time.Second, PollInterval: 100 * time.Millisecond,
	})
	is.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	is.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		is.NoErr(manager.Close())
	})
	client, err := appapi.NewClientWithResponses(server.URL, appapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	is.NoErr(err)
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	organization, space, cluster := "predeploy-"+suffix, "production", "local"
	namespace := ""
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if namespace == "" {
			return
		}
		if err := kube.CoreV1().Namespaces().Delete(cleanupContext, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up predeploy namespace: %v", err)
		}
	})
	organizationResponse, err := client.PutOrganizationWithResponse(ctx, organization)
	is.NoErr(err)
	assertApplicationResponse(t, organizationResponse.StatusCode(), organizationResponse.Body, http.StatusOK)
	clusterResponse, err := client.PutClusterWithResponse(ctx, organization, cluster, appapi.ClusterInput{KubeconfigPath: &kubeconfigPath})
	is.NoErr(err)
	assertApplicationResponse(t, clusterResponse.StatusCode(), clusterResponse.Body, http.StatusOK)
	spaceResponse, err := client.PutSpaceWithResponse(ctx, organization, space, appapi.SpaceInput{Cluster: cluster})
	is.NoErr(err)
	assertApplicationResponse(t, spaceResponse.StatusCode(), spaceResponse.Body, http.StatusOK)

	t.Log("deploying two healthy applications before testing predeploy gates")
	initialPatch := predeployPatch(t, map[string]any{"apps": []any{
		predeployInitialApp("frontend", image), predeployInitialApp("backend", image),
	}})
	initialPlan := createApplicationPlan(t, ctx, client, organization, space, initialPatch)
	namespace = initialPlan.Values["namespace"].(string)
	applyApplicationPlan(t, ctx, client, initialPlan.ID)
	initialDeployments := predeployDeployments(t, ctx, kube, namespace)
	is.Equal(len(initialDeployments), 2)

	t.Log("a running predeploy Job blocks every app and is resumed after a server restart")
	// Kubernetes must preserve the literal reference, and image arguments must
	// not be appended to the command supplied by the application.
	gateScript := "literal='$'; [ '$(RELEASE)' = \"${literal}(RELEASE)\" ] || exit 19; [ \"$#\" -eq 0 ] || exit 20; while [ ! -f /tmp/borq-continue ]; do sleep 1; done; printf 'migration finished\\n'"
	gatePlan := createApplicationPlan(t, ctx, client, organization, space, predeployReleasePatch(t, ctx, server.URL, organization, "two", gateScript, 60))
	is.Equal(inspectApplicationManifests(t, ctx, server.URL, gatePlan.Manifests), gatePlan.Configuration)
	operation := startPredeployOperation(t, ctx, client, gatePlan.ID)
	job, pod := waitPredeployRunning(t, ctx, kube, namespace)
	if clusterResponse.JSON200 == nil || clusterResponse.JSON200.ID == nil || spaceResponse.JSON200 == nil {
		t.Fatal("predeploy target was missing its resource identities")
	}
	assertApplicationOwnership(t, &job, *spaceResponse.JSON200, *clusterResponse.JSON200.ID, "frontend")
	assertApplicationOwnership(t, &pod, *spaceResponse.JSON200, *clusterResponse.JSON200.ID, "frontend")
	is.True(strings.HasSuffix(job.Name, "-pre"))
	is.True(len(job.Name) <= 63)
	is.Equal(job.Labels["borq.run/predeploy-name"], "pre")
	is.Equal(job.Spec.BackoffLimit, int32Pointer(0))
	is.Equal(job.Spec.Template.Spec.Containers[0].Image, image)
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 60 {
		t.Fatalf("predeploy deadline was not rendered: %v", job.Spec.ActiveDeadlineSeconds)
	}
	assertPredeployEnv(t, ctx, kube, namespace, job.Spec.Template.Spec.Containers[0], "RELEASE", "two")
	assertPredeployTrafficIsolation(t, ctx, kube, namespace, pod)
	assertPredeployAppsUnchanged(t, initialDeployments, predeployDeployments(t, ctx, kube, namespace))
	repeated := startPredeployOperation(t, ctx, client, gatePlan.ID)
	is.Equal(repeated.ID, operation.ID)
	assertPredeployJobCount(t, ctx, kube, namespace, 1)
	server.Close()
	is.NoErr(manager.Close())
	manager, err = apps.NewManager(ctx, apps.NewInput{
		DatabasePath: databasePath, RolloutTimeout: 90 * time.Second, PollInterval: 100 * time.Millisecond,
	})
	is.NoErr(err)
	handler, err = appapi.NewHandler(manager)
	is.NoErr(err)
	server = httptest.NewServer(handler)
	client, err = appapi.NewClientWithResponses(server.URL, appapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	is.NoErr(err)
	resumed := startPredeployOperation(t, ctx, client, gatePlan.ID)
	is.Equal(resumed.ID, operation.ID)
	resumedJob, resumedPod := waitPredeployRunning(t, ctx, kube, namespace)
	is.Equal(resumedJob.UID, job.UID)
	is.Equal(resumedPod.UID, pod.UID)
	assertPredeployJobCount(t, ctx, kube, namespace, 1)
	assertPredeployAppsUnchanged(t, initialDeployments, predeployDeployments(t, ctx, kube, namespace))
	execRequest := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: pod.Spec.Containers[0].Name, Command: []string{"touch", "/tmp/borq-continue"}, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(kubeconfig, http.MethodPost, execRequest.URL())
	is.NoErr(err)
	var commandOutput bytes.Buffer
	is.NoErr(executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &commandOutput, Stderr: &commandOutput}))
	completed := waitPredeployOperation(t, ctx, client, operation.ID, appapi.Succeeded)
	is.Equal(completed.ID, operation.ID)
	completedJob, err := kube.BatchV1().Jobs(namespace).Get(ctx, job.Name, metav1.GetOptions{})
	is.NoErr(err)
	is.True(predeployJobCondition(*completedJob, batchv1.JobComplete))
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + job.Name})
	is.NoErr(err)
	is.Equal(len(pods.Items), 1)
	assertPredeployRelease(t, ctx, kube, namespace, "two")
	acceptedDeployments := predeployDeployments(t, ctx, kube, namespace)
	for name, deployment := range acceptedDeployments {
		is.True(deployment.Generation > initialDeployments[name].Generation)
	}

	t.Log("a failing migration leaves every existing Deployment unchanged")
	failurePlan := createApplicationPlan(t, ctx, client, organization, space, predeployReleasePatch(t, ctx, server.URL, organization, "bad", "printf 'migration failed\\n' >&2; exit 17", 30))
	failure := startPredeployOperation(t, ctx, client, failurePlan.ID)
	waitPredeployOperation(t, ctx, client, failure.ID, appapi.Failed)
	assertPredeployAppsUnchanged(t, acceptedDeployments, predeployDeployments(t, ctx, kube, namespace))
	assertPredeployJobCount(t, ctx, kube, namespace, 2)
	repeatedFailure := startPredeployOperation(t, ctx, client, failurePlan.ID)
	is.Equal(repeatedFailure.ID, failure.ID)
	is.Equal(repeatedFailure.Status, appapi.Failed)
	assertPredeployJobCount(t, ctx, kube, namespace, 2)

	t.Log("a fresh corrected apply creates a new Job and rolls forward after failure")
	recoveryPlan := createApplicationPlan(t, ctx, client, organization, space, predeployReleasePatch(t, ctx, server.URL, organization, "recovered", "printf 'migration recovered\\n'", 30))
	applyApplicationPlan(t, ctx, client, recoveryPlan.ID)
	assertPredeployJobCount(t, ctx, kube, namespace, 3)
	assertPredeployRelease(t, ctx, kube, namespace, "recovered")
	recoveredDeployments := predeployDeployments(t, ctx, kube, namespace)
	retained := createApplicationPlan(t, ctx, client, organization, space, predeployPatch(t, map[string]any{"apps": []any{
		map[string]any{"name": "frontend", "replicas": 1},
	}}))
	is.Equal(applicationByName(t, retained.Configuration, "frontend").Predeploy, applicationByName(t, recoveryPlan.Configuration, "frontend").Predeploy)

	t.Log("a webserver process must exit successfully and otherwise times out before rollout")
	timeoutPlan := createApplicationPlan(t, ctx, client, organization, space, predeployReleasePatch(t, ctx, server.URL, organization, "blocked", "exec nginx -g 'daemon off;'", 5))
	timeoutOperation := startPredeployOperation(t, ctx, client, timeoutPlan.ID)
	timedOut := waitPredeployOperation(t, ctx, client, timeoutOperation.ID, appapi.Failed)
	if !strings.Contains(strings.ToLower(timedOut.Message), "deadline") && !strings.Contains(strings.ToLower(timedOut.Message), "timeout") && !strings.Contains(strings.ToLower(timedOut.Message), "timed out") {
		t.Fatalf("predeploy timeout did not explain the failure: %s", timedOut.Message)
	}
	assertPredeployAppsUnchanged(t, recoveredDeployments, predeployDeployments(t, ctx, kube, namespace))
	assertPredeployJobCount(t, ctx, kube, namespace, 4)

	t.Log("an empty predeploy command disables the gate without removing historical Jobs")
	disabledPlan := createApplicationPlan(t, ctx, client, organization, space, predeployPatch(t, map[string]any{"apps": []any{
		map[string]any{"name": "frontend", "predeploy": map[string]any{"command": ""}},
	}}))
	is.Equal(applicationByName(t, disabledPlan.Configuration, "frontend").Predeploy.Command, "")
	is.Equal(inspectApplicationManifests(t, ctx, server.URL, disabledPlan.Manifests), disabledPlan.Configuration)
	applyApplicationPlan(t, ctx, client, disabledPlan.ID)
	assertPredeployJobCount(t, ctx, kube, namespace, 4)
	assertPredeployRelease(t, ctx, kube, namespace, "blocked")
}

func predeployInitialApp(name, image string) map[string]any {
	return map[string]any{
		"name": name, "image": image, "port": 80,
		"command": predeployShellCommand("printf 'ready\\n' > /usr/share/nginx/html/healthz; exec nginx -g 'daemon off;'"),
	}
}

func predeployReleasePatch(t *testing.T, ctx context.Context, serverUrl, organization, release, script string, timeoutSeconds int) appapi.Patch {
	t.Helper()
	publishApplicationEnvironmentGroup(t, ctx, serverUrl, organization, "production", "release-"+release, map[string]string{"RELEASE": release})
	return predeployPatch(t, map[string]any{"apps": []any{
		map[string]any{
			"name": "frontend", "env": map[string]any{"name": "release-" + release, "version": "latest"},
			"predeploy": map[string]any{
				"command": predeployShellCommand(script), "timeoutSeconds": timeoutSeconds,
			},
		},
		map[string]any{"name": "backend", "env": map[string]any{"name": "release-" + release, "version": "latest"}},
	}})
}

// predeployShellCommand preserves script quotes and newlines as one shell argument.
// An explicit shell name keeps any accidentally appended image arguments in $#.
func predeployShellCommand(script string) string {
	return "/bin/sh -c '" + strings.ReplaceAll(script, "'", "'\\''") + "' borq-test"
}

func predeployPatch(t *testing.T, value map[string]any) appapi.Patch {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var patch appapi.Patch
	if err := json.Unmarshal(encoded, &patch); err != nil {
		t.Fatal(err)
	}
	return patch
}

// SDK clients use pointers because connection transports and discovery state are shared.
func predeployDeployments(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string) map[string]appsv1.Deployment {
	t.Helper()
	deployments, err := kube.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]appsv1.Deployment, len(deployments.Items))
	for _, deployment := range deployments.Items {
		result[deployment.Name] = deployment
	}
	return result
}

func assertPredeployAppsUnchanged(t *testing.T, before, after map[string]appsv1.Deployment) {
	t.Helper()
	is := is.New(t)
	is.Equal(len(before), len(after))
	for name, deployment := range before {
		is.Equal(after[name].Generation, deployment.Generation)
		is.Equal(after[name].Spec, deployment.Spec)
	}
}

// SDK clients use pointers because connection transports and discovery state are shared.
func assertPredeployRelease(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace, release string) {
	t.Helper()
	for _, deployment := range predeployDeployments(t, ctx, kube, namespace) {
		assertPredeployEnv(t, ctx, kube, namespace, deployment.Spec.Template.Spec.Containers[0], "RELEASE", release)
	}
}

// Kubernetes clients retain their connection transports through pointer receivers.
func assertPredeployEnv(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string, container corev1.Container, name, value string) {
	t.Helper()
	if applicationEnvironment(t, ctx, kube, namespace, container)[name] == value {
		return
	}
	t.Fatalf("missing environment variable %s=%s", name, value)
}

// SDK clients share connection transports through pointer receivers.
func assertPredeployTrafficIsolation(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string, jobPod corev1.Pod) {
	t.Helper()
	if jobPod.Labels["borq.run/app-name"] != "frontend" {
		t.Fatalf("predeploy pod lost its parent app label: %v", jobPod.Labels)
	}
	if _, exists := jobPod.Labels["app.kubernetes.io/name"]; exists {
		t.Fatalf("predeploy pod must not carry an app Service label: %v", jobPod.Labels)
	}
	jobAddresses := make(map[string]bool, len(jobPod.Status.PodIPs))
	for _, address := range jobPod.Status.PodIPs {
		jobAddresses[address.IP] = true
	}
	jobAddresses[jobPod.Status.PodIP] = true
	endpointContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		readyServices := 0
		for _, name := range []string{"frontend", "backend"} {
			service, err := kube.CoreV1().Services(namespace).Get(endpointContext, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(jobPod.Labels)) {
				t.Fatalf("app Service %s selects the running predeploy pod", name)
			}
			slices, err := kube.DiscoveryV1().EndpointSlices(namespace).List(endpointContext, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + name})
			if err != nil {
				t.Fatal(err)
			}
			ready := false
			for _, slice := range slices.Items {
				for _, endpoint := range slice.Endpoints {
					if endpoint.TargetRef != nil && endpoint.TargetRef.UID == jobPod.UID {
						t.Fatalf("app Service %s routes to the running predeploy pod", name)
					}
					for _, address := range endpoint.Addresses {
						if jobAddresses[address] {
							t.Fatalf("app Service %s routes to predeploy address %s", name, address)
						}
					}
					ready = ready || endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready
				}
			}
			if ready {
				readyServices++
			}
		}
		if readyServices == 2 {
			return
		}
		select {
		case <-endpointContext.Done():
			t.Fatal("both app Services must retain ready endpoints while predeploy runs")
		case <-ticker.C:
		}
	}
}

// SDK clients use pointers because connection transports and discovery state are shared.
func assertPredeployJobCount(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string, count int) {
	t.Helper()
	jobs, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	is.New(t).Equal(len(jobs.Items), count)
}

// SDK clients use pointers because connection transports and discovery state are shared.
func waitPredeployRunning(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string) (batchv1.Job, corev1.Pod) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		jobs, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs.Items {
			pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + job.Name})
			if err != nil {
				t.Fatal(err)
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodRunning {
					return job, pod
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("predeploy Job did not start before the test deadline")
		case <-ticker.C:
		}
	}
}

func predeployJobCondition(job batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// Generated clients share their configured transport through pointer receivers.
func startPredeployOperation(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, planId string) appapi.Operation {
	t.Helper()
	response, err := client.ApplyPlanWithResponse(ctx, planId)
	if err != nil {
		t.Fatal(err)
	}
	assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusAccepted)
	if response.JSON202 == nil {
		t.Fatal("apply response did not contain an operation")
	}
	return *response.JSON202
}

// Generated clients share their configured transport through pointer receivers.
func waitPredeployOperation(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, operationId string, expected appapi.OperationStatus) appapi.Operation {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.GetOperationWithResponse(ctx, operationId)
		if err != nil {
			t.Fatal(err)
		}
		assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusOK)
		if response.JSON200 == nil {
			t.Fatal("operation response did not contain progress")
		}
		operation := *response.JSON200
		if operation.Status == appapi.Succeeded || operation.Status == appapi.Failed {
			if operation.Status != expected {
				t.Fatalf("predeploy operation ended %s, expected %s: %s", operation.Status, expected, operation.Message)
			}
			return operation
		}
		select {
		case <-ctx.Done():
			t.Fatalf("predeploy operation did not finish: %s", operation.Message)
		case <-ticker.C:
		}
	}
}
