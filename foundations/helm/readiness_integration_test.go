//go:build integration

package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"helm.sh/helm/v4/pkg/action"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

// TestReleaseLifecycleIntegrationReadiness verifies template-managed CRDs advance
// from live reads when watch data remains stale, while regular Jobs and install
// and upgrade hooks still block completion and failed hooks fail the release.
func TestReleaseLifecycleIntegrationReadiness(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires configuration pointers when constructing clients.
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	extensions, err := apiextensionsclient.NewForConfig(&configuration)
	assert.NoErr(err)
	upstream, err := url.Parse(configuration.Host)
	assert.NoErr(err)
	transport, err := rest.HTTPClientFor(&configuration)
	assert.NoErr(err)
	namespace := fmt.Sprintf("borq-helm-ready-%d", time.Now().UnixNano())
	crdName := "widgets." + namespace + ".borq.run"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = extensions.ApiextensionsV1().CustomResourceDefinitions().Delete(cleanup, crdName, metav1.DeleteOptions{})
	})
	var crdReads, crdWatches atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = transport.Transport
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method != http.MethodGet || response.StatusCode != http.StatusOK || !strings.HasSuffix(response.Request.URL.Path, "/customresourcedefinitions") {
			return nil
		}
		// A late or missing readiness event must not make a release depend on
		// a stale list snapshot. Individual GET requests retain the live status.
		var list map[string]any
		if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
			return err
		}
		_ = response.Body.Close()
		for _, item := range list["items"].([]any) {
			object := item.(map[string]any)
			if object["metadata"].(map[string]any)["name"] == crdName {
				delete(object, "status")
			}
		}
		encoded, err := json.Marshal(list)
		if err != nil {
			return err
		}
		response.Body = io.NopCloser(strings.NewReader(string(encoded)))
		response.ContentLength = int64(len(encoded))
		response.Header.Set("Content-Length", fmt.Sprint(len(encoded)))
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/customresourcedefinitions") {
			if request.URL.Query().Get("watch") == "true" {
				crdWatches.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				writer.(http.Flusher).Flush()
				<-request.Context().Done()
				return
			}
			request.Header.Set("Accept", "application/json")
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/customresourcedefinitions/"+crdName) {
			crdReads.Add(1)
		}
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(path, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %q\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\nusers:\n- name: test\n  user: {}\ncurrent-context: test\n", server.URL)), 0o600))
	proxied, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	manager, err := NewReleaseManager(NewReleaseManagerInput{Connection: proxied, KubeconfigPath: path})
	assert.NoErr(err)
	defer manager.Close()
	chartPath := t.TempDir()
	writeReadinessChart(t, chartPath, namespace)
	input := InstallOrUpgradeInput{Namespace: namespace, ReleaseName: "readiness", ChartRef: chartPath, Version: "0.1.0", Values: map[string]any{"message": "first"}, Timeout: 25 * time.Second}
	results := make(chan readinessResult, 1)
	go func() {
		state, err := manager.Reconcile(ctx, input)
		results <- readinessResult{state: state, err: err}
	}()
	ordinary := waitReadinessJob(t, ctx, client, namespace, "ordinary", "")
	assertReadinessBlocked(t, results)
	_, err = client.BatchV1().Jobs(namespace).Get(ctx, "readiness-hook", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	completeReadinessJob(t, ctx, client, ordinary, false)
	hook := waitReadinessJob(t, ctx, client, namespace, "readiness-hook", "")
	assertReadinessBlocked(t, results)
	completeReadinessJob(t, ctx, client, hook, false)
	installed := <-results
	assert.NoErr(installed.err)
	assert.Equal(installed.state.Status, "deployed")
	assert.Equal(installed.state.Revision, 1)
	assert.True(crdReads.Load() > 0)
	assert.Equal(crdWatches.Load(), int64(0))
	configurationAction := action.NewConfiguration()
	assert.NoErr(configurationAction.Init(manager.restClientGetter(namespace), namespace, "secrets"))
	stored, err := configurationAction.Releases.Last("readiness")
	assert.NoErr(err)
	record, err := releaseRecord(stored)
	assert.NoErr(err)
	assert.Equal(string(record.Hooks[0].LastRun.Phase), "Succeeded")
	input.Upgrade, input.Values = true, map[string]any{"message": "second"}
	go func() {
		state, err := manager.Reconcile(ctx, input)
		results <- readinessResult{state: state, err: err}
	}()
	upgradedHook := waitReadinessJob(t, ctx, client, namespace, "readiness-hook", hook.UID)
	assertReadinessBlocked(t, results)
	completeReadinessJob(t, ctx, client, upgradedHook, false)
	upgraded := <-results
	assert.NoErr(upgraded.err)
	assert.Equal(upgraded.state.Status, "deployed")
	assert.Equal(upgraded.state.Revision, 2)
	input.Values = map[string]any{"message": "third"}
	go func() {
		state, err := manager.Reconcile(ctx, input)
		results <- readinessResult{state: state, err: err}
	}()
	failedHook := waitReadinessJob(t, ctx, client, namespace, "readiness-hook", upgradedHook.UID)
	completeReadinessJob(t, ctx, client, failedHook, true)
	failed := <-results
	assert.True(failed.err != nil)
	assert.Equal(failed.state.Status, "failed")
	assert.Equal(failed.state.Revision, 3)
	assert.Equal(crdWatches.Load(), int64(0))
}

type readinessResult struct {
	state ReleaseState
	err   error
}

func assertReadinessBlocked(t *testing.T, results chan readinessResult) {
	t.Helper()
	select {
	case result := <-results:
		t.Fatalf("release finished before its Job completed: status=%s error=%v", result.state.Status, result.err)
	case <-time.After(200 * time.Millisecond):
	}
}

// Kubernetes owns the shared client pointer used by these integration helpers.
func waitReadinessJob(t *testing.T, ctx context.Context, client *kubeclient.Clientset, namespace, name string, previous types.UID) batchv1.Job {
	t.Helper()
	var job batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		current, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		job = *current
		return current.UID != previous, nil
	})
	is.New(t).NoErr(err)
	return job
}

// Suspended Jobs avoid image pulls; status updates simulate controller completion
// so this test exercises Helm's readiness gates independently of workload execution.
func completeReadinessJob(t *testing.T, ctx context.Context, client *kubeclient.Clientset, job batchv1.Job, failed bool) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		condition := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}
		started, completed := metav1.NewTime(time.Now().Add(-time.Second)), metav1.Now()
		current.Status.StartTime, current.Status.CompletionTime = &started, &completed
		criteria := batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: completed}
		current.Status.Succeeded = 1
		if failed {
			condition.Type, condition.Reason = batchv1.JobFailed, "IntentionalTestFailure"
			criteria.Type = batchv1.JobFailureTarget
			current.Status.CompletionTime = nil
			current.Status.Succeeded, current.Status.Failed = 0, 1
		}
		current.Status.Conditions = append(current.Status.Conditions, criteria, condition)
		_, err = client.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return err
	})
	is.New(t).NoErr(err)
}

func writeReadinessChart(t *testing.T, path, namespace string) {
	t.Helper()
	is.New(t).NoErr(os.MkdirAll(filepath.Join(path, "templates"), 0o700))
	files := map[string]string{
		"Chart.yaml":               "apiVersion: v2\nname: readiness\nversion: 0.1.0\n",
		"values.yaml":              "message: first\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: readiness\ndata:\n  message: {{ .Values.message | quote }}\n",
		"templates/crd.yaml": fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.%s.borq.run
spec:
  group: %s.borq.run
  scope: Namespaced
  names:
    plural: widgets
    singular: widget
    kind: Widget
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`, namespace, namespace),
		"templates/ordinary.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: ordinary\nspec:\n  suspend: true\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: unused\n          image: registry.k8s.io/pause:3.10\n",
		"templates/hook.yaml":     "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: readiness-hook\n  annotations:\n    helm.sh/hook: post-install,post-upgrade\n    helm.sh/hook-delete-policy: before-hook-creation\nspec:\n  suspend: true\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: unused\n          image: registry.k8s.io/pause:3.10\n",
	}
	for name, contents := range files {
		is.New(t).NoErr(os.WriteFile(filepath.Join(path, name), []byte(contents), 0o600))
	}
}
