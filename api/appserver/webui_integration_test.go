//go:build integration

package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"golang.org/x/net/html"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestApplicationIntegrationWebUi proves the form-to-API deployment workflow
// against real Kubernetes, including read-only review, existing application port
// changes, app-scoped predeploy tasks, direct rolling restarts, and controller success.
func TestApplicationIntegrationWebUi(t *testing.T) {
	assert := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required")
	}
	configuration, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(configuration)
	assert.NoErr(err)
	kubeconfig, err := os.ReadFile(kubeconfigPath)
	assert.NoErr(err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime, err := New(ctx, NewInput{Port: 7833, DataDir: t.TempDir(), RolloutTimeout: time.Minute})
	assert.NoErr(err)
	defer func() { assert.NoErr(runtime.Close()) }()
	server := httptest.NewServer(runtime.http.Handler)
	defer server.Close()
	organization := fmt.Sprintf("webui-%x", time.Now().UnixNano())
	namespace := ""
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if namespace == "" {
			return
		}
		if err := kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up UI deployment: %v", err)
		}
	})
	_, body := submitUiForm(t, ctx, server, url.Values{"action": {"create-organization"}, "name": {organization}})
	assert.True(strings.Contains(body, "Organization created."))
	linkHeader, body := submitUiForm(t, ctx, server, url.Values{"action": {"register-cluster"}, "organization": {organization}, "space": {"default"}, "view": {"clusters"}, "name": {"local"}, "kubeconfig": {string(kubeconfig)}})
	assert.True(strings.Contains(body, "Download kubeconfig"))
	linkLocation, err := url.Parse(linkHeader.Get("HX-Push-Url"))
	assert.NoErr(err)
	clusterName := linkLocation.Query().Get("cluster")
	assert.True(clusterName != "")
	_, body = submitUiForm(t, ctx, server, url.Values{"action": {"create-space"}, "organization": {organization}, "space": {"default"}, "view": {"spaces"}, "name": {"default"}, "cluster": {clusterName}})
	assert.True(strings.Contains(body, "Space saved."))
	_, body = submitUiForm(t, ctx, server, url.Values{"action": {"publish-group"}, "organization": {organization}, "space": {"default"}, "view": {"environment"}, "name": {"ui-config"}, "baseVersion": {"0"}, "format": {"fields"}, "variableName": {"UI_LABEL"}, "variableValue": {"from-ui"}})
	assert.True(strings.Contains(body, "Environment group published."))
	assert.True(strings.Contains(body, `name="baseVersion" value="1"`))
	_, body = submitUiForm(t, ctx, server, url.Values{"action": {"clone-group"}, "organization": {organization}, "space": {"default"}, "view": {"environment"}, "name": {"ui-config-copy"}, "sourceName": {"ui-config"}, "sourceVersion": {"1"}})
	assert.True(strings.Contains(body, "Environment group cloned."))
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if image == "" {
		image = "nginx:1.27.5"
	}
	client, err := appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	firstDeployment := ""
	for index, update := range []struct{ port, replicas int }{{8080, 0}, {80, 1}} {
		header, body := submitUiForm(t, ctx, server, url.Values{
			"action": {"plan-app"}, "organization": {organization}, "space": {"default"}, "view": {"applications"},
			"name": {"web"}, "image": {image}, "port": {strconv.Itoa(update.port)}, "replicas": {strconv.Itoa(update.replicas)}, "readiness": {"on"}, "startup": {"on"}, "readinessPath": {"/"}, "startupPath": {"/"}, "environment": {"ui-config-copy"}, "environmentVersion": {"latest"},
			"readinessPort": {strconv.Itoa(update.port)}, "startupPort": {strconv.Itoa(update.port)},
			"predeployCommand": {"/bin/sh -c 'exit 0'"},
		})
		assert.True(strings.Contains(body, "Review deployment plan"))
		assert.True(strings.Contains(body, "Apply reviewed plan"))
		location, err := url.Parse(header.Get("HX-Push-Url"))
		assert.NoErr(err)
		plan := location.Query().Get("plan")
		assert.True(plan != "")
		savedPlan, err := runtime.manager.GetPlan(ctx, plan)
		assert.NoErr(err)
		namespace = savedPlan.Space.Namespace
		before, err := client.ListAppsWithResponse(ctx, organization, "default")
		assert.NoErr(err)
		assert.True(before.JSON200 != nil)
		if index == 0 {
			assert.Equal(len(before.JSON200.Configuration.Apps), 0)
		} else {
			assert.Equal(before.JSON200.Configuration.Apps[0].Port, 8080)
			deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort, int32(8080))
			service, err := kube.CoreV1().Services(namespace).Get(ctx, "web", metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(service.Spec.Ports[0].Port, int32(8080))
			for _, difference := range savedPlan.ResourceDiffs {
				if difference.Kind == "Deployment" || difference.Kind == "Service" {
					assert.Equal(difference.Action, "update")
				}
			}
		}
		header, body = submitUiForm(t, ctx, server, url.Values{"action": {"apply-plan"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "view": {"application"}, "tab": {"configuration"}, "subtab": {"yaml"}, "plan": {plan}})
		location, err = url.Parse(header.Get("HX-Push-Url"))
		assert.NoErr(err)
		assert.Equal(location.Query().Get("tab"), "overview")
		assert.Equal(location.Query().Get("subtab"), "")
		assert.True(strings.Contains(body, "Loading deployment history"))
		assert.True(strings.Contains(body, "http://web."+namespace+".svc:"+strconv.Itoa(update.port)))
		operation := location.Query().Get("operation")
		assert.True(operation != "")
		if index == 0 {
			firstDeployment = operation
		}
		assert.Equal(location.Query().Get("view"), "application")
		assert.Equal(location.Query().Get("app"), "web")
		for {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/ui/operations/"+operation, nil)
			assert.NoErr(err)
			response, err := server.Client().Do(request)
			assert.NoErr(err)
			data, err := io.ReadAll(response.Body)
			assert.NoErr(err)
			assert.NoErr(response.Body.Close())
			assert.Equal(response.StatusCode, http.StatusOK)
			if strings.Contains(string(data), ">succeeded</span>") {
				assert.Equal(response.Header.Get("HX-Trigger"), "deployment-complete")
				assert.True(!strings.Contains(string(data), "every 2s"))
				break
			}
			if strings.Contains(string(data), ">failed</span>") {
				t.Fatalf("UI deployment failed: %.1500s", data)
			}
			select {
			case <-ctx.Done():
				t.Fatal("UI deployment did not complete")
			case <-time.After(200 * time.Millisecond):
			}
		}
		after, err := client.ListAppsWithResponse(ctx, organization, "default")
		assert.NoErr(err)
		assert.True(after.JSON200 != nil)
		assert.Equal(after.JSON200.Configuration.Apps[0].Image, image)
		deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(deployment.Status.AvailableReplicas, int32(update.replicas))
		ports := deployment.Spec.Template.Spec.Containers[0].Ports
		assert.Equal(len(ports), 1)
		assert.Equal(ports[0].ContainerPort, int32(update.port))
		service, err := kube.CoreV1().Services(namespace).Get(ctx, "web", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(len(service.Spec.Ports), 1)
		assert.Equal(service.Spec.Ports[0].Port, int32(update.port))
		assert.Equal(service.Spec.Ports[0].TargetPort.IntVal, int32(update.port))
	}

	// Live health follows the real Deployment controller, independently of accepted state.
	health, err := client.GetAppWithResponse(ctx, organization, "default", "web")
	assert.NoErr(err)
	assert.True(health.JSON200 != nil)
	assert.Equal(string(health.JSON200.State), "healthy")
	beforeRestart, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
	assert.NoErr(err)
	beforePods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=web,borq.run/phase!=predeploy"})
	assert.NoErr(err)
	assert.Equal(len(beforePods.Items), 1)
	beforeJobs, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.True(len(beforeJobs.Items) > 0)
	beforeJobIds := map[string]string{}
	for _, job := range beforeJobs.Items {
		beforeJobIds[job.Name] = string(job.UID)
	}
	beforeSpace, err := runtime.manager.GetSpace(ctx, organization, "default")
	assert.NoErr(err)
	header, body := submitUiForm(t, ctx, server, url.Values{"action": {"restart-app"}, "application": {"web"}, "organization": {organization}, "space": {"default"}})
	restartLocation, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	assert.Equal(restartLocation.Query().Get("plan"), "")
	assert.True(strings.Contains(body, "Rolling restart started."))
	assert.True(!strings.Contains(body, "Review deployment plan"))
	for {
		health, err = client.GetAppWithResponse(ctx, organization, "default", "web")
		assert.NoErr(err)
		assert.True(health.JSON200 != nil)
		if health.JSON200.State == "healthy" {
			break
		}
		if health.JSON200.State == "degraded" {
			t.Fatalf("restart failed: %s", health.JSON200.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatal("restart did not complete")
		case <-time.After(200 * time.Millisecond):
		}
	}
	afterRestart, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(afterRestart.Generation > beforeRestart.Generation)
	assert.True(afterRestart.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] != "")
	assert.Equal(afterRestart.Spec.Template.Spec, beforeRestart.Spec.Template.Spec)
	assert.Equal(afterRestart.Status.ReadyReplicas, int32(1))
	afterPods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=web,borq.run/phase!=predeploy"})
	assert.NoErr(err)
	activePods := []string{}
	for _, pod := range afterPods.Items {
		if pod.DeletionTimestamp == nil {
			activePods = append(activePods, string(pod.UID))
		}
	}
	assert.Equal(len(activePods), 1)
	assert.True(activePods[0] != string(beforePods.Items[0].UID))
	afterJobs, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.Equal(len(afterJobs.Items), len(beforeJobs.Items))
	for _, job := range afterJobs.Items {
		assert.Equal(string(job.UID), beforeJobIds[job.Name])
	}
	afterSpace, err := runtime.manager.GetSpace(ctx, organization, "default")
	assert.NoErr(err)
	assert.Equal(afterSpace, beforeSpace)

	// Installing or updating an app without a task must not run another app's
	// configured task. Historical Jobs and the other app's settings stay intact.
	for _, port := range []string{"80", "8080"} {
		header, _ = submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "organization": {organization}, "space": {"default"}, "name": {"worker"}, "image": {image}, "port": {port}, "replicas": {"0"}, "predeployCommand": {""}, "readinessPath": {"/"}, "startupPath": {"/"}})
		workerLocation, err := url.Parse(header.Get("HX-Push-Url"))
		assert.NoErr(err)
		workerPlan, err := client.GetPlanWithResponse(ctx, workerLocation.Query().Get("plan"))
		assert.NoErr(err)
		assert.True(workerPlan.JSON200 != nil)
		for _, manifest := range workerPlan.JSON200.Manifests {
			assert.True(manifest["kind"] != "Job") // Only explicitly targeted apps may run predeploy.
		}
		workerApply, err := client.ApplyPlanWithResponse(ctx, workerPlan.JSON200.ID)
		assert.NoErr(err)
		assert.True(workerApply.JSON202 != nil)
		waitUiOperation(t, ctx, runtime, workerApply.JSON202.ID)
		jobs, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
		assert.NoErr(err)
		assert.Equal(len(jobs.Items), len(beforeJobs.Items))
		for _, job := range jobs.Items {
			assert.Equal(string(job.UID), beforeJobIds[job.Name])
		}
		web, err := client.GetAppWithResponse(ctx, organization, "default", "web")
		assert.NoErr(err)
		assert.True(web.JSON200 != nil)
		assert.Equal(web.JSON200.Configuration.Predeploy.Command, "/bin/sh -c 'exit 0'")
		worker, err := client.GetAppWithResponse(ctx, organization, "default", "worker")
		assert.NoErr(err)
		assert.True(worker.JSON200 != nil)
		assert.Equal(worker.JSON200.Configuration.Predeploy.Command, "")
		assert.Equal(worker.JSON200.State, appapi.Stopped)
	}
	// Reuse historical settings as editable input, then review a fresh plan.
	historical, err := client.GetOperationWithResponse(ctx, firstDeployment)
	assert.NoErr(err)
	assert.True(historical.JSON200 != nil)
	draftQuery := url.Values{"view": {"application"}, "organization": {organization}, "space": {"default"}, "app": {"web"}, "tab": {"configuration"}, "subtab": {"yaml"}, "draft": {historical.JSON200.PlanID}}
	draftResponse, err := server.Client().Get(server.URL + "/?" + draftQuery.Encode())
	assert.NoErr(err)
	assert.Equal(draftResponse.StatusCode, http.StatusOK)
	draftDocument, err := html.Parse(draftResponse.Body)
	assert.NoErr(err)
	assert.NoErr(draftResponse.Body.Close())
	draftYaml := ""
	for node := range draftDocument.Descendants() {
		if node.Data != "textarea" || node.FirstChild == nil {
			continue
		}
		for _, attribute := range node.Attr {
			if attribute.Key == "name" && attribute.Val == "configuration" {
				draftYaml = node.FirstChild.Data
			}
		}
	}
	assert.True(strings.Contains(draftYaml, "replicas: 0"))
	assert.True(strings.Contains(draftYaml, "port: 8080"))
	assert.True(strings.Contains(draftYaml, "timeoutSeconds: 300"))
	draftYaml = strings.Replace(draftYaml, "timeoutSeconds: 300", "timeoutSeconds: 450", 1)
	header, _ = submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "format": {"yaml"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "tab": {"configuration"}, "subtab": {"yaml"}, "configuration": {draftYaml}})
	versionLocation, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	assert.True(versionLocation.Query().Get("plan") != historical.JSON200.PlanID)
	versionOperation, err := client.ApplyPlanWithResponse(ctx, versionLocation.Query().Get("plan"))
	assert.NoErr(err)
	assert.True(versionOperation.JSON202 != nil)
	waitUiOperation(t, ctx, runtime, versionOperation.JSON202.ID)
	health, err = client.GetAppWithResponse(ctx, organization, "default", "web")
	assert.NoErr(err)
	assert.True(health.JSON200 != nil)
	assert.Equal(string(health.JSON200.State), "stopped")
	assert.Equal(health.JSON200.Configuration.Port, 8080)
	assert.Equal(health.JSON200.Configuration.Predeploy.TimeoutSeconds, 450)
	history, err := client.ListAppDeploymentsWithResponse(ctx, organization, "default", "web")
	assert.NoErr(err)
	assert.True(history.JSON200 != nil)
	assert.Equal(len(*history.JSON200), 3)

	// The YAML editor must retain an explicit zero as an intentional scale-down.
	header, _ = submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "format": {"yaml"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "view": {"application"}, "configuration": {"name: web\nreplicas: 0\n"}})
	location, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	scale, err := client.GetPlanWithResponse(ctx, location.Query().Get("plan"))
	assert.NoErr(err)
	assert.True(scale.JSON200 != nil)
	assert.Equal(scale.JSON200.Configuration.Apps[0].Replicas, 0)

	// A failed predeploy must be removable from the full configuration form.
	// Existing Jobs remain as history and must never gate this next deployment.
	header, _ = submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "format": {"yaml"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "configuration": {"name: web\npredeploy:\n  command: /does-not-exist\n  timeoutSeconds: 10\n"}})
	failureLocation, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	failure, err := client.ApplyPlanWithResponse(ctx, failureLocation.Query().Get("plan"))
	assert.NoErr(err)
	assert.True(failure.JSON202 != nil)
	waitUiOperationStatus(t, ctx, runtime, failure.JSON202.ID, "failed")
	jobsBeforeClear, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	accepted, err := client.GetAppWithResponse(ctx, organization, "default", "web")
	assert.NoErr(err)
	assert.True(accepted.JSON200 != nil)
	document, err := json.Marshal(accepted.JSON200.Configuration)
	assert.NoErr(err)
	header, body = submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "tab": {"configuration"}, "subtab": {"fields"}, "document": {string(document)}, "name": {"web"}, "image": {image}, "port": {"80"}, "replicas": {"1"}, "predeployCommand": {""}, "readiness": {"on"}, "startup": {"on"}, "readinessPath": {"/"}, "startupPath": {"/"}, "readinessPort": {"80"}, "startupPort": {"80"}})
	assert.True(!strings.Contains(body, `id="application-editor"`))
	clearedLocation, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	cleared, err := client.GetPlanWithResponse(ctx, clearedLocation.Query().Get("plan"))
	assert.NoErr(err)
	assert.True(cleared.JSON200 != nil)
	assert.Equal(cleared.JSON200.Configuration.Apps[0].Predeploy.Command, "")
	for _, manifest := range cleared.JSON200.Manifests {
		assert.True(manifest["kind"] != "Job")
	}
	recovery, err := client.ApplyPlanWithResponse(ctx, cleared.JSON200.ID)
	assert.NoErr(err)
	assert.True(recovery.JSON202 != nil)
	waitUiOperation(t, ctx, runtime, recovery.JSON202.ID)
	jobsAfterClear, err := kube.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.Equal(len(jobsAfterClear.Items), len(jobsBeforeClear.Items))
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(deployment.Status.ReadyReplicas, int32(1))

	download, err := server.Client().Get(server.URL + "/ui/kubeconfig?organization=" + organization + "&cluster=" + clusterName)
	assert.NoErr(err)
	defer download.Body.Close()
	assert.Equal(download.StatusCode, http.StatusOK)
	assert.Equal(download.Header.Get("Content-Type"), "application/yaml")
	assert.Equal(download.Header.Get("Cache-Control"), "no-store")
	assert.True(strings.Contains(download.Header.Get("Content-Disposition"), clusterName+".yaml"))
}

func waitUiOperation(t *testing.T, ctx context.Context, runtime Server, id string) {
	t.Helper()
	waitUiOperationStatus(t, ctx, runtime, id, "succeeded")
}

func waitUiOperationStatus(t *testing.T, ctx context.Context, runtime Server, id, expected string) {
	t.Helper()
	for {
		operation, err := runtime.manager.GetOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status == expected {
			return
		}
		if operation.Status == "failed" || operation.Status == "succeeded" {
			t.Fatalf("application operation unexpectedly %s: %s", operation.Status, operation.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatal("application operation did not complete")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func submitUiForm(t *testing.T, ctx context.Context, server *httptest.Server, form url.Values) (http.Header, string) {
	t.Helper()
	assert := is.New(t)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/ui/actions", strings.NewReader(form.Encode()))
	assert.NoErr(err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response, err := server.Client().Do(request)
	assert.NoErr(err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	assert.NoErr(err)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("UI action %s returned %d: %.1500s", form.Get("action"), response.StatusCode, body)
	}
	return response.Header, string(body)
}
