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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/openbao"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/yaml"
)

// TestApplicationIntegrationEnvironmentVault proves app plans pin existing config
// versions, ESO gates predeploy, and app apply never publishes or restores values.
func TestApplicationIntegrationEnvironmentVault(t *testing.T) {
	assert := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for environment integration tests")
	}
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(kubeconfig)
	assert.NoErr(err)
	manager, err := apps.NewManager(ctx, apps.NewInput{
		DatabasePath: filepath.Join(t.TempDir(), "environment.sqlite"), RolloutTimeout: 60 * time.Second, PollInterval: 100 * time.Millisecond,
	})
	assert.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); assert.NoErr(manager.Close()) })
	client, err := appapi.NewClientWithResponses(server.URL, appapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	assert.NoErr(err)
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	organization, namespace := "env-"+suffix, ""
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if namespace == "" {
			return
		}
		if err := kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up space namespace: %v", err)
		}
	})
	organizationResponse, err := client.PutOrganizationWithResponse(ctx, organization)
	assert.NoErr(err)
	assertApplicationResponse(t, organizationResponse.StatusCode(), organizationResponse.Body, http.StatusOK)
	cluster, err := client.PutClusterWithResponse(ctx, organization, "local", appapi.ClusterInput{KubeconfigPath: &kubeconfigPath})
	assert.NoErr(err)
	assertApplicationResponse(t, cluster.StatusCode(), cluster.Body, http.StatusOK)
	spaceState, err := client.PutSpaceWithResponse(ctx, organization, "production", appapi.SpaceInput{Cluster: "local"})
	assert.NoErr(err)
	assertApplicationResponse(t, spaceState.StatusCode(), spaceState.Body, http.StatusOK)
	kubernetesClient, err := kubeapps.New(kubeapps.NewInput{KubeconfigPath: kubeconfigPath})
	assert.NoErr(err)
	access, err := kubernetesClient.OpenBaoStore(ctx, "borq-openbao")
	assert.NoErr(err)
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: access.Address, Token: access.Token, HttpClient: access.HttpClient})
	assert.NoErr(err)

	t.Log("default config names resolve without putting values in the app specification")
	publishApplicationEnvironmentGroup(t, ctx, server.URL, organization, "production", organization+"-production-default-app", map[string]string{"DEFAULT": "present"})
	zero := 0
	defaultApps := []appapi.AppPatch{{Name: "default-app", Image: &image, Replicas: &zero}}
	defaultPlan, err := client.PostPlanWithResponse(ctx, organization, "production", appapi.Patch{Apps: &defaultApps})
	assert.NoErr(err)
	assertApplicationResponse(t, defaultPlan.StatusCode(), defaultPlan.Body, http.StatusCreated)
	namespace = defaultPlan.JSON201.Values["namespace"].(string)
	assert.Equal(defaultPlan.JSON201.Configuration.Apps[0].Env, appapi.EnvReference{Name: organization + "-production-default-app", Version: "1"})
	missingApps := []appapi.AppPatch{{Name: "missing", Image: &image, Env: applicationEnvPatch("missing-config", "latest")}}
	missingPlan, err := client.PostPlanWithResponse(ctx, organization, "production", appapi.Patch{Apps: &missingApps})
	assert.NoErr(err)
	assertApplicationResponse(t, missingPlan.StatusCode(), missingPlan.Body, http.StatusNotFound)

	firstValues := map[string]string{"RELEASE": "first", "EMPTY_TEXT": ""}
	firstConfig := publishApplicationEnvironmentGroup(t, ctx, server.URL, organization, "production", "backend-config", firstValues)
	assert.Equal(firstConfig.Version, 1)
	duplicate := publishApplicationEnvironmentGroup(t, ctx, server.URL, organization, "production", "backend-config", firstValues)
	assert.Equal(duplicate.Version, 1) // Identical complete values do not write a version.
	fixture := []byte(fmt.Sprintf(`apps:
  - name: backend
    image: %s
    command: %s
    env:
      name: backend-config
      version: latest
    predeploy:
      command: %s
`, image, predeployShellCommand(`printf 'ready' > /usr/share/nginx/html/healthz; printf '%s:%s' "$RELEASE" "$EMPTY_TEXT" > /usr/share/nginx/html/index.html; exec nginx -g 'daemon off;'`), predeployShellCommand(`printf '%s' "$RELEASE"`)))
	payload, err := yaml.YAMLToJSONStrict(fixture)
	assert.NoErr(err)
	expanded := applicationPost(t, ctx, server.URL+"/api/v1/apps/expand", payload, http.StatusOK)
	assert.True(bytes.Contains(expanded, []byte(`"version":"latest"`)))
	planResponse, err := client.PostPlanWithBodyWithResponse(ctx, organization, "production", "application/json", bytes.NewReader(payload))
	assert.NoErr(err)
	assertApplicationResponse(t, planResponse.StatusCode(), planResponse.Body, http.StatusCreated)
	if planResponse.JSON201 == nil {
		t.Fatal("plan missing artifacts")
	}
	first := *planResponse.JSON201
	assert.Equal(applicationByName(t, first.Configuration, "backend").Env, appapi.EnvReference{Name: "backend-config", Version: "1"})
	firstSecret, path, version := applicationEnvReference(t, first, "backend")
	assert.Equal(version, 1)
	metadata, err := vault.ReadKvMetadata(ctx, access.Mount, path)
	assert.NoErr(err)
	assert.Equal(metadata.CurrentVersion, 1)
	assert.Equal(metadata.MaxVersions, 11)

	t.Log("publishing v2 after planning leaves the saved plan pinned to v1")
	secondValues := map[string]string{"RELEASE": "second", "EMPTY_TEXT": ""}
	secondConfig := publishApplicationEnvironmentGroup(t, ctx, server.URL, organization, "production", "backend-config", secondValues)
	assert.Equal(secondConfig.Version, 2)

	baseVersion := 1
	conflict, err := client.PublishEnvironmentGroupWithResponse(ctx, organization, "production", "backend-config", appapi.PublishEnvironmentGroupInput{BaseVersion: &baseVersion, Values: []appapi.Variable{{Name: "RELEASE", Value: "stale"}}})
	assert.NoErr(err)
	assertApplicationResponse(t, conflict.StatusCode(), conflict.Body, http.StatusConflict)

	applyApplicationPlan(t, ctx, client, first.ID)
	assertEnvironmentRollout(t, ctx, kube, namespace, first, firstSecret, firstValues)
	assertEnvironmentConfiguration(t, ctx, kube, server.URL, namespace, first)
	metadata, err = vault.ReadKvMetadata(ctx, access.Mount, path)
	assert.NoErr(err)
	assert.Equal(metadata.CurrentVersion, 2) // App apply cannot write config versions.

	t.Log("the next plan resolves latest while later direct vault edits remain independent")
	second := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{})
	assert.Equal(applicationByName(t, second.Configuration, "backend").Env.Version, "2")
	secondSecret, _, secondVersion := applicationEnvReference(t, second, "backend")
	assert.Equal(secondVersion, 2)
	assert.True(secondSecret != firstSecret)
	thirdValues := map[string]string{"RELEASE": "third"} // EMPTY_TEXT was deleted in the vault.
	_, err = vault.WriteKv(ctx, access.Mount, path, openbao.WriteKvInput{Values: thirdValues, PlanID: "manual-deletion", CheckVersion: 2})
	assert.NoErr(err)
	applyApplicationPlan(t, ctx, client, second.ID)
	assertEnvironmentRollout(t, ctx, kube, namespace, second, secondSecret, secondValues)
	third := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{})
	thirdSecret, _, thirdVersion := applicationEnvReference(t, third, "backend")
	assert.Equal(thirdVersion, 3)
	applyApplicationPlan(t, ctx, client, third.ID)
	assertEnvironmentRollout(t, ctx, kube, namespace, third, thirdSecret, thirdValues)
	entry, err := vault.ReadKv(ctx, access.Mount, path, 0)
	assert.NoErr(err)
	assert.Equal(entry.Values, thirdValues)
	assert.Equal(entry.Version, 3) // Applying a stale full app cannot recreate EMPTY_TEXT.

	t.Log("cloning a pinned config creates an independently selectable release")
	clone, err := client.CloneEnvironmentGroupWithResponse(ctx, organization, "production", "backend-next", appapi.CloneEnvironmentGroupInput{SourceName: "backend-config", SourceVersion: stringPointer("1")})
	assert.NoErr(err)
	assertApplicationResponse(t, clone.StatusCode(), clone.Body, http.StatusCreated)
	assert.Equal(clone.JSON201.Version, 1)
	assertApplicationVariables(t, clone.JSON201.Values, firstValues)
	cloneApps := []appapi.AppPatch{{Name: "backend", Image: &image, Env: applicationEnvPatch("backend-next", "latest")}}
	clonePlan := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{Apps: &cloneApps})
	assert.Equal(applicationByName(t, clonePlan.Configuration, "backend").Image, image)
	assert.Equal(applicationByName(t, clonePlan.Configuration, "backend").Env, appapi.EnvReference{Name: "backend-next", Version: "1"})
	cloneSecret, clonePath, _ := applicationEnvReference(t, clonePlan, "backend")
	assert.True(clonePath != path)
	applyApplicationPlan(t, ctx, client, clonePlan.ID)
	assertEnvironmentRollout(t, ctx, kube, namespace, clonePlan, cloneSecret, firstValues)
	assertEnvironmentConfiguration(t, ctx, kube, server.URL, namespace, clonePlan)

	// Historical pairs are retained; each generated immutable Secret stays intact.
	oldSecret, err := kube.CoreV1().Secrets(namespace).Get(ctx, firstSecret, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(string(oldSecret.Data["RELEASE"]), "first")
}

// TestApplicationIntegrationEnvironmentVersionWindow checks selection and apply
// reject versions outside the latest plus ten previous versions, even when the
// vault still retains those values and when a saved apply ages while queued.
func TestApplicationIntegrationEnvironmentVersionWindow(t *testing.T) {
	assert := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for environment integration tests")
	}
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(kubeconfig)
	assert.NoErr(err)
	manager, err := apps.NewManager(ctx, apps.NewInput{
		DatabasePath: filepath.Join(t.TempDir(), "version-window.sqlite"), RolloutTimeout: 60 * time.Second, PollInterval: 100 * time.Millisecond,
	})
	assert.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); assert.NoErr(manager.Close()) })
	client, err := appapi.NewClientWithResponses(server.URL, appapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	assert.NoErr(err)
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	organization, namespace := "window-"+suffix, ""
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if namespace == "" {
			return
		}
		if err := kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up space namespace: %v", err)
		}
	})
	organizationResponse, err := client.PutOrganizationWithResponse(ctx, organization)
	assert.NoErr(err)
	assertApplicationResponse(t, organizationResponse.StatusCode(), organizationResponse.Body, http.StatusOK)
	cluster, err := client.PutClusterWithResponse(ctx, organization, "local", appapi.ClusterInput{KubeconfigPath: &kubeconfigPath})
	assert.NoErr(err)
	assertApplicationResponse(t, cluster.StatusCode(), cluster.Body, http.StatusOK)
	spaceState, err := client.PutSpaceWithResponse(ctx, organization, "production", appapi.SpaceInput{Cluster: "local"})
	assert.NoErr(err)
	assertApplicationResponse(t, spaceState.StatusCode(), spaceState.Body, http.StatusOK)
	kubernetesClient, err := kubeapps.New(kubeapps.NewInput{KubeconfigPath: kubeconfigPath})
	assert.NoErr(err)
	access, err := kubernetesClient.OpenBaoStore(ctx, "borq-openbao")
	assert.NoErr(err)
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: access.Address, Token: access.Token, HttpClient: access.HttpClient})
	assert.NoErr(err)

	const configName = "release-config"
	published := publishApplicationEnvironmentGroup(t, ctx, server.URL, organization, "production", configName, map[string]string{"RELEASE": "1"})
	assert.Equal(published.Version, 1)
	app := predeployInitialApp("backend", image)
	app["env"] = map[string]any{"name": configName, "version": "1"}
	initial := createApplicationPlan(t, ctx, client, organization, "production", predeployPatch(t, map[string]any{"apps": []any{app}}))
	namespace = initial.Values["namespace"].(string)
	_, path, _ := applicationEnvReference(t, initial, "backend")
	applyApplicationPlan(t, ctx, client, initial.ID)

	// Keep old values physically present to distinguish Borq's logical selection
	// window from OpenBao's normal max_versions pruning.
	assert.NoErr(vault.SetKvRetention(ctx, access.Mount, path, 50))
	advance := func(version int) {
		t.Helper()
		entry, err := vault.WriteKv(ctx, access.Mount, path, openbao.WriteKvInput{
			Values: map[string]string{"RELEASE": strconv.Itoa(version)}, PlanID: "manual-" + strconv.Itoa(version), CheckVersion: version - 1,
		})
		assert.NoErr(err)
		assert.Equal(entry.Version, version)
	}
	advance(2)
	// Explicitly selecting a config without version resets a previously pinned
	// selection to latest rather than retaining the prior numerical pin.
	withoutVersion := []appapi.AppPatch{{Name: "backend", Env: &appapi.EnvReferencePatch{Name: stringPointer(configName)}}}
	latest := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{Apps: &withoutVersion})
	assert.Equal(applicationByName(t, latest.Configuration, "backend").Env.Version, "2")
	for version := 3; version <= 11; version++ {
		advance(version)
	}

	t.Log("latest minus ten is selectable; the next older version is rejected")
	two := 2
	oldestApps := []appapi.AppPatch{{Name: "backend", Replicas: &two, Env: applicationEnvPatch(configName, "1")}}
	oldest := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{Apps: &oldestApps})
	assert.Equal(applicationByName(t, oldest.Configuration, "backend").Env.Version, "1")
	baseline := predeployDeployments(t, ctx, kube, namespace)
	advance(12)
	retained, err := vault.ReadKv(ctx, access.Mount, path, 1)
	assert.NoErr(err)
	assert.Equal(retained.Values, map[string]string{"RELEASE": "1"})
	expiredPlan, err := client.PostPlanWithResponse(ctx, organization, "production", appapi.Patch{Apps: &oldestApps})
	assert.NoErr(err)
	assertApplicationResponse(t, expiredPlan.StatusCode(), expiredPlan.Body, http.StatusBadRequest)
	expiredApply, err := client.ApplyPlanWithResponse(ctx, oldest.ID)
	assert.NoErr(err)
	assertApplicationResponse(t, expiredApply.StatusCode(), expiredApply.Body, http.StatusBadRequest)
	assertPredeployAppsUnchanged(t, baseline, predeployDeployments(t, ctx, kube, namespace))
	// Historical reads and cloning remain useful even when the original version
	// is no longer selectable for an application rollout.
	historical, err := client.GetEnvironmentGroupVersionWithResponse(ctx, organization, "production", configName, "1")
	assert.NoErr(err)
	assertApplicationResponse(t, historical.StatusCode(), historical.Body, http.StatusOK)
	clone, err := client.CloneEnvironmentGroupWithResponse(ctx, organization, "production", "historical-copy", appapi.CloneEnvironmentGroupInput{SourceName: configName, SourceVersion: stringPointer("1")})
	assert.NoErr(err)
	assertApplicationResponse(t, clone.StatusCode(), clone.Body, http.StatusCreated)
	assert.Equal(clone.JSON201.Version, 1)

	// Deploy the current config before introducing a queue gate. The active gate
	// makes no Deployment spec changes, so the final comparison detects any
	// accidental mutation by the expired queued operation.
	currentApps := []appapi.AppPatch{{Name: "backend", Env: applicationEnvPatch(configName, "latest")}}
	current := createApplicationPlan(t, ctx, client, organization, "production", appapi.Patch{Apps: &currentApps})
	applyApplicationPlan(t, ctx, client, current.ID)
	baseline = predeployDeployments(t, ctx, kube, namespace)
	activePatch := predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "backend", "env": map[string]any{"name": configName, "version": "12"},
		"predeploy": map[string]any{"command": predeployShellCommand("while [ ! -f /tmp/borq-continue ]; do sleep 1; done"), "timeoutSeconds": 60},
	}}})
	active := createApplicationPlan(t, ctx, client, organization, "production", activePatch)
	queuedPatch := predeployPatch(t, map[string]any{"apps": []any{map[string]any{
		"name": "backend", "replicas": 2, "env": map[string]any{"name": configName, "version": "2"},
		"predeploy": map[string]any{"command": ""},
	}}})
	queued := createApplicationPlan(t, ctx, client, organization, "production", queuedPatch)
	activeOperation := startPredeployOperation(t, ctx, client, active.ID)
	_, pod := waitPredeployRunning(t, ctx, kube, namespace)
	queuedOperation := startPredeployOperation(t, ctx, client, queued.ID)
	assert.Equal(string(queuedOperation.Status), "queued")
	advance(13) // v2 was eligible at acceptance and expires while A is still gated.
	retained, err = vault.ReadKv(ctx, access.Mount, path, 2)
	assert.NoErr(err)
	assert.Equal(retained.Values, map[string]string{"RELEASE": "2"})
	request := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "backend", Command: []string{"touch", "/tmp/borq-continue"}, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(kubeconfig, http.MethodPost, request.URL())
	assert.NoErr(err)
	var output bytes.Buffer
	assert.NoErr(executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output, Stderr: &output}))
	waitPredeployOperation(t, ctx, client, activeOperation.ID, appapi.Succeeded)
	failed := waitPredeployOperation(t, ctx, client, queuedOperation.ID, appapi.Failed)
	assert.True(strings.Contains(strings.ToLower(failed.Message), "version"))
	assertPredeployAppsUnchanged(t, baseline, predeployDeployments(t, ctx, kube, namespace))
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "backend", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(applicationEnvironment(t, ctx, kube, namespace, deployment.Spec.Template.Spec.Containers[0]), map[string]string{"RELEASE": "12"})
	metadata, err := vault.ReadKvMetadata(ctx, access.Mount, path)
	assert.NoErr(err)
	assert.Equal(metadata.CurrentVersion, 13)
	assert.Equal(metadata.MaxVersions, 50)
}

// Optional generated request fields require pointers to distinguish omissions.
func stringPointer(value string) *string { return &value }

func applicationEnvReference(t *testing.T, plan appapi.Plan, appName string) (string, string, int) {
	t.Helper()
	for _, manifest := range plan.Manifests {
		if manifest["kind"] != "ExternalSecret" {
			continue
		}
		metadata := manifest["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]any)
		if labels["app.kubernetes.io/name"] != appName {
			continue
		}
		specification := manifest["spec"].(map[string]any)
		target := specification["target"].(map[string]any)
		source := specification["dataFrom"].([]any)[0].(map[string]any)["extract"].(map[string]any)
		version, err := strconv.Atoi(source["version"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return target["name"].(string), source["key"].(string), version
	}
	t.Fatalf("plan has no pinned environment for %s", appName)
	return "", "", 0
}

// Kubernetes clients retain connection transports through SDK pointer receivers.
func assertEnvironmentConfiguration(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, serverUrl, namespace string, plan appapi.Plan) {
	t.Helper()
	manifests := liveApplicationManifests(t, ctx, kube, namespace)
	for _, manifest := range plan.Manifests {
		if manifest["kind"] != "Job" {
			continue
		}
		name := manifest["metadata"].(map[string]any)["name"].(string)
		job, err := kube.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		job.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
		manifests = append(manifests, applicationManifest(t, job))
	}
	is.New(t).Equal(inspectApplicationManifests(t, ctx, serverUrl, manifests), plan.Configuration)
}

// Kubernetes clients retain connection transports through SDK pointer receivers.
func assertEnvironmentRollout(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string, plan appapi.Plan, secretName string, values map[string]string) {
	t.Helper()
	assert := is.New(t)
	secret, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(secret.Type, corev1.SecretTypeOpaque)
	assert.True(secret.Immutable != nil && *secret.Immutable)
	actual := map[string]string{}
	for key, value := range secret.Data {
		actual[key] = string(value)
	}
	assert.Equal(actual, values)
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "backend", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(applicationEnvironment(t, ctx, kube, namespace, deployment.Spec.Template.Spec.Containers[0]), values)
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=backend"})
	assert.NoErr(err)
	runningPod := ""
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			runningPod = pod.Name
			break
		}
	}
	assert.True(runningPod != "")
	// The application writes its environment at process startup. Reading its
	// actual response proves the values reached the process, not just its Pod spec.
	response, err := kube.CoreV1().Pods(namespace).ProxyGet("http", runningPod, "80", "/", nil).DoRaw(ctx)
	assert.NoErr(err)
	assert.Equal(string(response), values["RELEASE"]+":"+values["EMPTY_TEXT"])
	for _, manifest := range plan.Manifests {
		if manifest["kind"] != "Job" {
			continue
		}
		name := manifest["metadata"].(map[string]any)["name"].(string)
		job, err := kube.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(applicationEnvironment(t, ctx, kube, namespace, job.Spec.Template.Spec.Containers[0]), values)
		jobPods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + name})
		assert.NoErr(err)
		assert.Equal(len(jobPods.Items), 1)
		log, err := kube.CoreV1().Pods(namespace).GetLogs(jobPods.Items[0].Name, &corev1.PodLogOptions{Container: "backend"}).Do(ctx).Raw()
		assert.NoErr(err)
		assert.Equal(string(log), values["RELEASE"])
	}
}
