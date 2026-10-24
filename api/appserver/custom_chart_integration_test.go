//go:build integration

package appserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/api/webui"
	"github.com/stefanmcshane/borq/business/server/apps"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	extensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestApplicationIntegrationCustomCharts exercises public chart defaults, real Helm install,
// selected workload metadata, CRDs, upgrade, persistence after reopen, and uninstall
// through the generated API, with a real Kubernetes deployment controller.
func TestApplicationIntegrationCustomCharts(t *testing.T) {
	assert := is.New(t)
	kubeconfig := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("BORQ_KUBECONFIG is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(config)
	assert.NoErr(err)
	crds, err := extensions.NewForConfig(config)
	assert.NoErr(err)
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	group := "addons-" + suffix + ".borq.run"
	repository := customChartRepository(t, group)
	path := filepath.Join(t.TempDir(), "addons.sqlite")
	workerContext, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	// Only the store transport is a fixture. Helm and workload readiness use the
	// real cluster; event names are selected from that release's persisted manifests.
	var lifecycleAt atomic.Int64
	telemetry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/query_range" {
			_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
			return
		}
		query := request.URL.Query().Get("query")
		values := [][2]string{}
		if strings.Contains(query, `job="borq-kubernetes-events"`) {
			assert.True(strings.Contains(query, "addon-service"))
			assert.True(strings.Contains(query, "addon-service-maintenance"))
			values = append(values, [2]string{strconv.FormatInt(lifecycleAt.Load(), 10), `{"kind":"Pod","name":"addon-service-7b89c56dd8-abc12","reason":"BackOff","count":2,"msg":"Restarting container"}`})
		}
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{}, "values": values}}}}))
	}))
	t.Cleanup(telemetry.Close)
	manager, err := apps.NewManager(workerContext, apps.NewInput{DatabasePath: path, RolloutTimeout: 90 * time.Second, PollInterval: 50 * time.Millisecond, LokiQueryUrl: telemetry.URL, PrometheusQueryUrl: telemetry.URL})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(api)
	t.Cleanup(func() { server.Close() })
	client, err := appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	organization := "addons-" + suffix
	created, err := client.PutOrganizationWithResponse(ctx, organization)
	assert.NoErr(err)
	assertApplicationResponse(t, created.StatusCode(), created.Body, http.StatusOK)
	registered, err := client.LinkClusterWithResponse(ctx, organization, appapi.LinkClusterInput{Name: "local", Connection: appapi.ClusterInput{KubeconfigPath: &kubeconfig}})
	assert.NoErr(err)
	assertApplicationResponse(t, registered.StatusCode(), registered.Body, http.StatusOK)
	space, err := client.GetSpaceWithResponse(ctx, organization, "default")
	assert.NoErr(err)
	assertApplicationResponse(t, space.StatusCode(), space.Body, http.StatusOK)
	namespace := space.JSON200.Namespace
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = crds.ApiextensionsV1().CustomResourceDefinitions().Delete(cleanup, "widgets."+group, metav1.DeleteOptions{})
	})
	defaults, err := client.GetChartDefaultsWithResponse(ctx, appapi.ChartInput{Repository: repository.URL, Name: "addon-fixture", Version: "0.1.0"})
	assert.NoErr(err)
	assertApplicationResponse(t, defaults.StatusCode(), defaults.Body, http.StatusOK)
	assert.True(strings.Contains(defaults.JSON200.Values, "# The original default comment"))
	assert.True(strings.Contains(defaults.JSON200.Values, "message: upstream-default"))
	_, err = kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err)) // Fetching defaults never installs resources.
	values := map[string]any{"message": "first override"}
	selector := map[string]string{"role": "api"}
	input := appapi.CustomChartInput{Repository: repository.URL, Chart: "addon-fixture", Version: "0.1.0", Values: &values, PodSelector: &selector}
	accepted, err := client.PutCustomChartWithResponse(ctx, organization, "default", "service", nil, input)
	assert.NoErr(err)
	assertApplicationResponse(t, accepted.StatusCode(), accepted.Body, http.StatusAccepted)
	assert.Equal(accepted.HTTPResponse.Header.Get("Cache-Control"), "no-store")
	waitCustomChartIntegration(t, ctx, client, accepted.JSON202.Operation)
	// Existing HTTP paths still read the same release and durable operation after the rename.
	legacy, err := server.Client().Get(server.URL + "/api/v1/organizations/" + organization + "/spaces/default/addons/service")
	assert.NoErr(err)
	assert.Equal(legacy.StatusCode, http.StatusOK)
	var existing appapi.CustomChart
	assert.NoErr(json.NewDecoder(legacy.Body).Decode(&existing))
	assert.NoErr(legacy.Body.Close())
	assert.Equal(existing.Name, "service")
	assert.Equal(existing.ReleaseName, accepted.JSON202.ReleaseName)
	legacy, err = server.Client().Get(server.URL + "/api/v1/addon-operations/" + accepted.JSON202.Operation)
	assert.NoErr(err)
	assert.Equal(legacy.StatusCode, http.StatusOK)
	var completed appapi.Operation
	assert.NoErr(json.NewDecoder(legacy.Body).Decode(&completed))
	assert.NoErr(legacy.Body.Close())
	assert.Equal(completed.Status, appapi.Succeeded)
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(deployment.Status.ReadyReplicas, int32(1))
	assert.Equal(deployment.Spec.Template.Labels["borq.run/addon-name"], "service")
	assert.Equal(deployment.Spec.Template.Labels["borq.run/logs"], "true")
	assert.Equal(deployment.Spec.Template.Labels["borq.run/metrics"], "true")
	lifecycleAt.Store(time.Now().UTC().UnixNano())
	history, err := client.GetCustomChartEventsWithResponse(ctx, organization, "default", "service", nil)
	assert.NoErr(err)
	assertApplicationResponse(t, history.StatusCode(), history.Body, http.StatusOK)
	assert.Equal(history.JSON200.EventsNotice, "")
	assert.Equal(len(history.JSON200.Events), 3)
	assert.Equal(history.JSON200.Events[0].Deployment, accepted.JSON202.Operation)
	assert.Equal(history.JSON200.Events[1].Kind, "succeeded")
	assert.Equal(history.JSON200.Events[2].Kind, "restart")
	metrics, err := client.GetCustomChartMetricsWithResponse(ctx, organization, "default", "service", nil)
	assert.NoErr(err)
	assertApplicationResponse(t, metrics.StatusCode(), metrics.Body, http.StatusOK)
	assert.Equal(len(metrics.JSON200.Events), 3)
	search := "no matching pod output" // Generated parameters use pointers for optional filters.
	logs, err := client.GetCustomChartLogsWithResponse(ctx, organization, "default", "service", &appapi.GetCustomChartLogsParams{Search: &search})
	assert.NoErr(err)
	assertApplicationResponse(t, logs.StatusCode(), logs.Body, http.StatusOK)
	assert.Equal(len(logs.JSON200.Entries), 0)
	assert.Equal(len(logs.JSON200.Events), 3) // Text filters must retain lifecycle context.
	maintenance, err := kube.BatchV1().CronJobs(namespace).Get(ctx, "addon-service-maintenance", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(maintenance.Spec.JobTemplate.Spec.Template.Labels["borq.run/logs"], "")
	live, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(live.Data["message"], "first override")
	_, err = crds.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "widgets."+group, metav1.GetOptions{})
	assert.NoErr(err)
	// The UI uses the same API and supplies chart editing and selected telemetry panels.
	ui, err := webui.NewHandler(api)
	assert.NoErr(err)
	// A new-install form must not overwrite an existing chart or enqueue an upgrade.
	duplicateForm := url.Values{"action": {"put-custom-chart"}, "organization": {organization}, "space": {"default"}, "name": {"service"}, "repository": {repository.URL}, "chart": {"addon-fixture"}, "version": {"0.1.1"}, "values": {"message: accidental overwrite"}}
	duplicateRequest := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(duplicateForm.Encode()))
	duplicateRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	duplicateRequest.Header.Set("HX-Request", "true")
	duplicateResponse := httptest.NewRecorder()
	ui.ServeHTTP(duplicateResponse, duplicateRequest)
	assert.Equal(duplicateResponse.Code, http.StatusConflict)
	assert.True(strings.Contains(duplicateResponse.Body.String(), "already exists in this space"))
	unchanged, err := client.GetCustomChartWithResponse(ctx, organization, "default", "service")
	assert.NoErr(err)
	assertApplicationResponse(t, unchanged.StatusCode(), unchanged.Body, http.StatusOK)
	assert.Equal(unchanged.JSON200.Operation, accepted.JSON202.Operation)
	assert.Equal(unchanged.JSON200.Specification.Version, "0.1.0")
	assert.Equal((*unchanged.JSON200.Specification.Values)["message"], "first override")
	unchangedLive, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(unchangedLive.ResourceVersion, live.ResourceVersion)
	for _, selection := range []string{"view=custom-chart&customChart=service", "view=addon&addon=service"} {
		page := httptest.NewRecorder()
		ui.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?organization="+organization+"&space=default&tab=configuration&"+selection, nil))
		assert.Equal(page.Code, http.StatusOK)
		for _, content := range []string{"Custom addons", "Save and upgrade", "Show defaults", "first override"} {
			assert.True(strings.Contains(page.Body.String(), content))
		}
		assert.True(!strings.Contains(page.Body.String(), "view=addon"))
		assert.True(strings.Contains(page.Body.String(), `name="apply" value="true"`))
	}
	input.Version = "0.1.1"
	values["message"] = "second override"
	duplicateForm.Set("apply", "true")
	duplicateForm.Set("values", "message: second override")
	upgradeRequest := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(duplicateForm.Encode()))
	upgradeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	upgradeRequest.Header.Set("HX-Request", "true")
	upgradeResponse := httptest.NewRecorder()
	ui.ServeHTTP(upgradeResponse, upgradeRequest)
	assert.Equal(upgradeResponse.Code, http.StatusOK)
	upgraded, err := client.GetCustomChartWithResponse(ctx, organization, "default", "service")
	assert.NoErr(err)
	assertApplicationResponse(t, upgraded.StatusCode(), upgraded.Body, http.StatusOK)
	waitCustomChartIntegration(t, ctx, client, upgraded.JSON200.Operation)
	live, err = kube.CoreV1().ConfigMaps(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(live.Data["message"], "second override")
	// Interrupt a real accepted upgrade during chart download. Shutdown retains
	// its operation; reopening must resume that same ID and accepted override map.
	repository.blocked.Store(true)
	values["message"] = "resumed override"
	apply := true // Generated query parameters use a pointer to preserve explicit intent.
	parameters := &appapi.PutCustomChartParams{Apply: &apply}
	interrupted, err := client.PutCustomChartWithResponse(ctx, organization, "default", "service", parameters, input)
	assert.NoErr(err)
	assertApplicationResponse(t, interrupted.StatusCode(), interrupted.Body, http.StatusAccepted)
	select {
	case <-repository.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	concurrent, err := client.PutCustomChartWithResponse(ctx, organization, "default", "service", parameters, input)
	assert.NoErr(err)
	assertApplicationResponse(t, concurrent.StatusCode(), concurrent.Body, http.StatusConflict)
	stopWorkers()
	repository.unblock()
	server.Close()
	assert.NoErr(manager.Close())
	manager, err = apps.NewManager(ctx, apps.NewInput{DatabasePath: path, RolloutTimeout: time.Minute})
	assert.NoErr(err)
	persisted, err := manager.GetAddon(ctx, organization, "default", "service")
	assert.NoErr(err)
	assert.True(persisted.State == "pending" || persisted.State == "deployed")
	assert.True(persisted.Revision >= 2)
	assert.Equal(persisted.Specification.Values["message"], "resumed override")
	api, err = appapi.NewHandler(manager)
	assert.NoErr(err)
	server = httptest.NewServer(api)
	client, err = appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	waitCustomChartIntegration(t, ctx, client, interrupted.JSON202.Operation)
	persisted, err = manager.GetAddon(ctx, organization, "default", "service")
	assert.NoErr(err)
	assert.Equal(persisted.State, "deployed")
	assert.True(persisted.Revision >= 3)
	live, err = kube.CoreV1().ConfigMaps(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(live.Data["message"], "resumed override")
	removed, err := client.DeleteCustomChartWithResponse(ctx, organization, "default", "service")
	assert.NoErr(err)
	assertApplicationResponse(t, removed.StatusCode(), removed.Body, http.StatusAccepted)
	waitCustomChartIntegration(t, ctx, client, removed.JSON202.Operation)
	_, err = kube.CoreV1().ConfigMaps(namespace).Get(ctx, "addon-service", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	_, err = crds.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "widgets."+group, metav1.GetOptions{})
	assert.NoErr(err) // Helm uninstall deliberately retains CRDs for future lifecycle separation.
	listed, err := client.ListCustomChartsWithResponse(ctx, organization, "default")
	assert.NoErr(err)
	assertApplicationResponse(t, listed.StatusCode(), listed.Body, http.StatusOK)
	assert.Equal(len(*listed.JSON200), 0)
	// A completed uninstall frees the name for a new install.
	reinstalled, err := client.PutCustomChartWithResponse(ctx, organization, "default", "service", nil, input)
	assert.NoErr(err)
	assertApplicationResponse(t, reinstalled.StatusCode(), reinstalled.Body, http.StatusAccepted)
	waitCustomChartIntegration(t, ctx, client, reinstalled.JSON202.Operation)
}

// The generated API client owns pointer-based HTTP state.
func waitCustomChartIntegration(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, id string) {
	t.Helper()
	for {
		response, err := client.GetCustomChartOperationWithResponse(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusOK)
		if response.JSON200.Status == appapi.Failed {
			t.Fatalf("addon operation failed: %s", response.JSON200.Message)
		}
		if response.JSON200.Status == appapi.Succeeded {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// The fixture shares its server and download gate across concurrent HTTP requests.
type customChartTestRepository struct {
	*httptest.Server
	blocked atomic.Bool
	started chan struct{}
	unblock func()
}

func customChartRepository(t *testing.T, group string) *customChartTestRepository {
	t.Helper()
	assert := is.New(t)
	archives := map[string][]byte{}
	for _, version := range []string{"0.1.0", "0.1.1"} {
		chart, err := loader.Load("testdata/addon-chart")
		assert.NoErr(err)
		chart.Metadata.Version = version
		for _, file := range chart.Files {
			file.Data = []byte(strings.ReplaceAll(string(file.Data), "addons.integration.borq.run", group))
		}
		archive, err := chartutil.Save(chart, t.TempDir())
		assert.NoErr(err)
		archives["/addon-fixture-"+version+".tgz"], err = os.ReadFile(archive)
		assert.NoErr(err)
	}
	repository := &customChartTestRepository{started: make(chan struct{}, 1)}
	release := make(chan struct{})
	repository.unblock = sync.OnceFunc(func() { repository.blocked.Store(false); close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/index.yaml" {
			if repository.blocked.Load() {
				select {
				case repository.started <- struct{}{}:
				default:
				}
				<-release
			}
			_, _ = fmt.Fprint(writer, "apiVersion: v1\nentries:\n  addon-fixture:\n    - name: addon-fixture\n      version: 0.1.0\n      urls: [addon-fixture-0.1.0.tgz]\n    - name: addon-fixture\n      version: 0.1.1\n      urls: [addon-fixture-0.1.1.tgz]\n")
			return
		}
		if archive, ok := archives[request.URL.Path]; ok {
			_, _ = writer.Write(archive)
			return
		}
		http.NotFound(writer, request)
	}))
	repository.Server = server
	t.Cleanup(func() { repository.unblock(); server.Close() })
	return repository
}
