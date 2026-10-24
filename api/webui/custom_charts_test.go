package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/foundations/helm"
)

// TestCustomChartDefaultsEscapesChartText keeps downloaded chart comments inert in the browser.
func TestCustomChartDefaultsEscapesChartText(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/charts/defaults")
		assert.Equal(request.Method, http.MethodPost)
		_ = json.NewEncoder(writer).Encode(appapi.ChartDefaults{Chart: appapi.ChartInput{Name: "fixture", Version: "1.2.3"}, Values: "# <script>alert('chart')</script>\nreplicas: 1\n"})
	}))
	assert.NoErr(err)
	request := httptest.NewRequest(http.MethodPost, "/ui/custom-charts/defaults", strings.NewReader(url.Values{"repository": {"https://charts.example.com"}, "chart": {"fixture"}, "version": {"1.2.3"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), "<script>"))
	assert.True(strings.Contains(response.Body.String(), "&lt;script&gt;"))
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
}

// TestCustomChartRepositoryHelper exposes repository choices only to the UI, without
// invoking installation APIs or caching the browser's repository selection.
func TestCustomChartRepositoryHelper(t *testing.T) {
	assert := is.New(t)
	repository := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("apiVersion: v1\nentries:\n  fixture:\n    - name: fixture\n      version: 1.2.3\n      urls: [fixture-1.2.3.tgz]\n"))
	}))
	t.Cleanup(repository.Close)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("chart discovery must not invoke an installation API")
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/custom-charts/repository?"+url.Values{"repository": {repository.URL}}.Encode(), nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.Equal(response.Header().Get("Content-Type"), "application/json")
	var charts []helm.RepositoryChart
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &charts))
	assert.Equal(charts, []helm.RepositoryChart{{Name: "fixture", Versions: []string{"1.2.3"}}})
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/custom-charts/versions?"+url.Values{"repository": {repository.URL}, "chart": {"fixture"}}.Encode(), nil))
	assert.Equal(response.Code, http.StatusOK)
	var versions []string
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &versions))
	assert.Equal(versions, []string{"1.2.3"})
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/custom-charts/versions?"+url.Values{"repository": {repository.URL}, "chart": {"missing"}}.Encode(), nil))
	assert.Equal(response.Code, http.StatusNotFound)
	assert.True(strings.Contains(response.Body.String(), "selected chart was not found"))
}

// TestCustomChartDefaultsRequiresSelection gives a useful form error before a missing chart
// or version can reach the API's generic configuration validation response.
func TestCustomChartDefaultsRequiresSelection(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("incomplete chart selections must not be forwarded")
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	assert.NoErr(err)
	for _, form := range []url.Values{{"chart": {"fixture"}}, {"version": {"1.2.3"}}} {
		request := httptest.NewRequest(http.MethodPost, "/ui/custom-charts/defaults", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		assert.Equal(response.Code, http.StatusBadRequest)
		assert.True(strings.Contains(response.Body.String(), "Choose a chart and version"))
	}
}

// TestCustomChartBrowserForm preserves complete YAML overrides and explicit log disablement at the form boundary.
func TestCustomChartBrowserForm(t *testing.T) {
	assert := is.New(t)
	form := url.Values{"repository": {"oci://registry.example.com/charts"}, "chart": {"fixture"}, "version": {"1.2.3"}, "values": {"replicas: 0\nempty: ''\n"}, "selector": {"role: api\n"}}
	input, err := customChartFormInput(form)
	assert.NoErr(err)
	assert.True(input.Logs != nil && !*input.Logs)
	assert.Equal((*input.Values)["replicas"], float64(0))
	assert.Equal((*input.Values)["empty"], "")
	assert.Equal((*input.PodSelector)["role"], "api")
	for _, source := range []string{"null", "key: one\nkey: two", "{}\n---\n{}", "- value"} {
		form.Set("values", source)
		_, err := customChartFormInput(form)
		assert.True(err != nil)
	}
}

// TestCustomChartMonitoringUsesCustomChartScope prevents custom chart panels from reading an application with the same name.
func TestCustomChartMonitoringUsesCustomChartScope(t *testing.T) {
	for _, panel := range []string{"logs", "metrics", "events"} {
		t.Run(panel, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/custom-charts/cache/"+panel)
				assert.Equal(request.URL.Query().Get("range"), "12h")
				start := time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
				events := []appapi.ApplicationEvent{{Timestamp: start.Add(time.Minute), Kind: "deployment", Message: "Install <script>untrusted event</script>", Deployment: "addon-operation"}, {Timestamp: start.Add(2 * time.Minute), Kind: "restart", Message: "BackOff · Restarting container", Pod: "custom-cache-0"}}
				if panel == "logs" {
					assert.Equal(request.URL.Query().Get("search"), "error")
					_ = json.NewEncoder(writer).Encode(appapi.AppLogs{Events: events, Entries: []appapi.AppLogEntry{{Timestamp: start.Add(3 * time.Minute), Message: "<script>untrusted log</script>", Pod: "cache-1", Container: "redis"}}})
					return
				}
				if panel == "events" {
					_ = json.NewEncoder(writer).Encode(appapi.EventHistory{Events: events})
					return
				}
				_ = json.NewEncoder(writer).Encode(appapi.AppMetrics{Start: start, End: start.Add(12 * time.Hour), StepSeconds: 300, Events: events})
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/custom-charts?organization=team&space=dev&app=cache&range=12h&search=error&follow=on&panel="+panel, nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.True(!strings.Contains(response.Body.String(), "<script>"))
			if panel == "logs" {
				assert.True(strings.Contains(response.Body.String(), "cache-1 / redis"))
				assert.True(strings.Contains(response.Body.String(), `hx-trigger="every 5s"`))
			}
			if panel == "metrics" {
				assert.True(strings.Contains(response.Body.String(), `data-chart-event`))
				assert.True(strings.Contains(response.Body.String(), `data-message="Replica keeps restarting"`))
			} else {
				assert.True(strings.Contains(response.Body.String(), "&lt;script&gt;untrusted event&lt;/script&gt;"))
				assert.True(strings.Contains(response.Body.String(), `/ui/custom-charts/operations/addon-operation`))
				assert.True(!strings.Contains(response.Body.String(), `/ui/operations/addon-operation`))
			}
		})
	}
}

// TestCustomAddonTabs keeps deployed addons on an overview and reuses workload
// controls while reserving the complete editor for Configuration and new installs.
func TestCustomAddonTabs(t *testing.T) {
	for _, input := range []struct {
		query, present, absent string
		status                 int
	}{
		{"&customChart=cache", "Loading installation and pod events", "Values overrides", 200},
		{"&customChart=cache&tab=configuration", "Save and upgrade", "Loading installation and pod events", 200},
		{"&customChart=cache&tab=observability", `class="range-controls" action="/ui/custom-charts"`, "data-custom-chart-editor", 200},
		{"&customChart=cache&tab=observability&subtab=logs", `class="log-controls" action="/ui/custom-charts"`, "data-custom-chart-editor", 200},
		{"", "Install custom addon", "Custom addon sections", 200},
		{"&tab=overview", "Install a custom addon before", "Addon settings", 404},
		{"&customChart=cache&tab=unknown", "page was not found", "Addon settings", 404},
		{"&customChart=cache&tab=observability&subtab=unknown", "view was not found", "Addon settings", 404},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			addon := appapi.CustomChart{Name: "cache", State: "deployed", Namespace: "selected", ReleaseName: "addon-cache", Specification: appapi.CustomChartInput{Chart: "redis", Version: "1.2.3", Repository: "https://charts.example.com"}}
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch request.URL.Path {
				case "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}}
				case "/api/v1/organizations/team/spaces":
					output = []appapi.Space{{ID: "space", Name: "dev", Cluster: "target"}}
				case "/api/v1/organizations/team/clusters":
					output = []appapi.Cluster{}
				case "/api/v1/organizations/team/spaces/dev/custom-charts":
					output = []appapi.CustomChart{addon}
				case "/api/v1/organizations/team/spaces/dev/custom-charts/cache":
					output = addon
				default:
					t.Errorf("unexpected API request: %s", request.URL.Path)
					writer.WriteHeader(404)
					return
				}
				assert.NoErr(json.NewEncoder(writer).Encode(output))
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=custom-chart&organization=team&space=dev"+input.query, nil))
			assert.Equal(response.Code, input.status)
			assert.True(strings.Contains(response.Body.String(), input.present))
			assert.True(!strings.Contains(response.Body.String(), input.absent))
		})
	}
}

// TestCustomChartProgressStaysInPanel keeps progress updates local even when an
// already-open page sends the workspace's inherited target, and stops terminal polling.
func TestCustomChartProgressStaysInPanel(t *testing.T) {
	for _, status := range []appapi.OperationStatus{appapi.Pending, appapi.Applying, appapi.Succeeded, appapi.Failed} {
		t.Run(string(status), func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/custom-chart-operations/chart-install")
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "chart-install", Status: status, Message: "Chart installation " + string(status)})
			}))
			assert.NoErr(err)
			request := httptest.NewRequest(http.MethodGet, "/ui/custom-charts/operations/chart-install", nil)
			request.Header.Set("HX-Request", "true")
			request.Header.Set("HX-Target", "app")
			response := httptest.NewRecorder()
			web.ServeHTTP(response, request)
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("HX-Retarget"), "this")
			assert.Equal(response.Header().Get("HX-Reswap"), "outerHTML")
			assert.Equal(response.Header().Get("HX-Trigger") == "deployment-complete", status == appapi.Succeeded || status == appapi.Failed)
			assert.True(strings.Contains(response.Body.String(), "Chart installation "+string(status)))
			assert.Equal(strings.Contains(response.Body.String(), `hx-trigger="every 2s"`), status == appapi.Pending || status == appapi.Applying)
		})
	}
}
