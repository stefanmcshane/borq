//go:build integration

package appserver

import (
	"context"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestApplicationIntegrationWebUi proves the form-to-API deployment workflow
// against real Kubernetes, including read-only review, existing application port
// changes, apply, and HTML polling through a successful controller rollout.
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	_, body = submitUiForm(t, ctx, server, url.Values{"action": {"register-cluster"}, "organization": {organization}, "space": {"default"}, "view": {"clusters"}, "name": {"local"}, "kubeconfig": {string(kubeconfig)}})
	assert.True(strings.Contains(body, "Download kubeconfig"))
	_, body = submitUiForm(t, ctx, server, url.Values{"action": {"create-space"}, "organization": {organization}, "space": {"default"}, "view": {"spaces"}, "name": {"default"}, "cluster": {"local"}})
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
	for index, update := range []struct{ port, replicas int }{{8080, 0}, {80, 1}} {
		header, body := submitUiForm(t, ctx, server, url.Values{
			"action": {"plan-app"}, "organization": {organization}, "space": {"default"}, "view": {"applications"},
			"name": {"web"}, "image": {image}, "port": {strconv.Itoa(update.port)}, "replicas": {strconv.Itoa(update.replicas)}, "readiness": {"on"}, "startup": {"on"}, "readinessPath": {"/"}, "startupPath": {"/"}, "environment": {"ui-config-copy"}, "environmentVersion": {"latest"},
			"readinessPort": {strconv.Itoa(update.port)}, "startupPort": {strconv.Itoa(update.port)},
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
		header, body = submitUiForm(t, ctx, server, url.Values{"action": {"apply-plan"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "view": {"application"}, "plan": {plan}})
		assert.True(strings.Contains(body, "http://web."+namespace+".svc:"+strconv.Itoa(update.port)))
		location, err = url.Parse(header.Get("HX-Push-Url"))
		assert.NoErr(err)
		operation := location.Query().Get("operation")
		assert.True(operation != "")
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

	// The YAML editor must retain an explicit zero as an intentional scale-down.
	header, _ := submitUiForm(t, ctx, server, url.Values{"action": {"plan-app"}, "format": {"yaml"}, "application": {"web"}, "organization": {organization}, "space": {"default"}, "view": {"application"}, "configuration": {"name: web\nreplicas: 0\n"}})
	location, err := url.Parse(header.Get("HX-Push-Url"))
	assert.NoErr(err)
	scale, err := client.GetPlanWithResponse(ctx, location.Query().Get("plan"))
	assert.NoErr(err)
	assert.True(scale.JSON200 != nil)
	assert.Equal(scale.JSON200.Configuration.Apps[0].Replicas, 0)

	download, err := server.Client().Get(server.URL + "/ui/kubeconfig?organization=" + organization + "&cluster=local")
	assert.NoErr(err)
	defer download.Body.Close()
	assert.Equal(download.StatusCode, http.StatusOK)
	assert.Equal(download.Header.Get("Content-Type"), "application/yaml")
	assert.Equal(download.Header.Get("Cache-Control"), "no-store")
	assert.True(strings.Contains(download.Header.Get("Content-Disposition"), "local.yaml"))
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
