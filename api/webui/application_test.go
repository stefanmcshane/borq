package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"golang.org/x/net/html"
)

// TestApplicationPages keeps the list separate from editing and limits saved
// configuration and manifests to the application selected in this space.
func TestApplicationPages(t *testing.T) {
	for _, input := range []struct {
		query, present, absent string
		status                 int
	}{
		{"view=applications", "Add an application", "Accepted configuration", 200},
		{"view=application", "Paste application as YAML instead", "Plan from apps.yaml", 200},
		{"view=application&app=web", "Edit application as YAML instead", "Applications in dev", 200},
		{"view=application&app=web", "http://web.borq-fixture-dev.svc:8080", "do-not-render-environment-value", 200},
		{"view=application&app=worker", "No network service", "http://worker.borq-fixture-dev.svc:0", 200},
		{"view=application&app=web&tab=configuration", "web-image", "worker-image", 200},
		{"view=application&app=web&tab=manifests", "web-deployment", "worker-deployment", 200},
		{"view=application&app=web&tab=manifests", "web-deployment", "shared-namespace", 200},
		{"view=application&app=planned&plan=new-plan", "<h1>planned</h1>", "Applications in dev", 200},
		{"view=application&app=missing", "selected application was not found", "web-image", 404},
		{"view=application&app=web&tab=missing", "page was not found", "web-image", 404},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			web := applicationTestHandler(t)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?organization=team&space=dev&"+input.query, nil))
			assert.Equal(response.Code, input.status)
			assert.True(strings.Contains(response.Body.String(), input.present))
			assert.True(!strings.Contains(response.Body.String(), input.absent))
		})
	}
}

// TestApplicationEnvironmentDropdown keeps selection in the app editor, lists
// published group names without values, and preserves a choice through YAML mode.
func TestApplicationEnvironmentDropdown(t *testing.T) {
	assert := is.New(t)
	web := applicationTestHandler(t)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=application&organization=team&space=dev", nil))
	assert.Equal(response.Code, http.StatusOK)
	document, err := html.Parse(strings.NewReader(response.Body.String()))
	assert.NoErr(err)
	kind := ""
	var choices []string
	for node := range document.Descendants() {
		for _, attribute := range node.Attr {
			if attribute.Key != "name" || attribute.Val != "environment" {
				continue
			}
			kind = node.Data
			for option := range node.Descendants() {
				if option.Data != "option" {
					continue
				}
				for _, attribute := range option.Attr {
					if attribute.Key == "value" {
						choices = append(choices, attribute.Val)
					}
				}
			}
		}
	}
	assert.Equal(kind, "select")
	assert.Equal(choices, []string{"", "web-config"})
	assert.True(!strings.Contains(response.Body.String(), "do-not-render-environment-value"))
	form := applicationEditorFields(t, response.Body.String())
	form.Set("name", "new-app")
	form.Set("image", "nginx")
	form.Set("environment", "web-config")
	form.Set("environmentVersion", "latest")
	form.Set("yamlMode", "on")
	form = convertApplicationEditor(t, web, form)
	form.Del("yamlMode")
	form = convertApplicationEditor(t, web, form)
	assert.Equal(form.Get("environment"), "web-config")
	assert.Equal(form.Get("environmentVersion"), "latest")
	assert.Equal(form.Get("serviceNamespace"), "borq-fixture-dev")
}

func applicationTestHandler(t *testing.T) http.Handler {
	t.Helper()
	assert := is.New(t)
	state := appapi.ApplicationState{Configuration: appapi.Configuration{Apps: []appapi.App{
		{Name: "web", Image: "web-image", Port: 8080, Replicas: 1},
		{Name: "worker", Image: "worker-image", Port: 0, Replicas: 1},
	}}, Manifests: []appapi.Manifest{
		{"kind": "Deployment", "metadata": map[string]any{"name": "web-deployment", "labels": map[string]any{"app.kubernetes.io/name": "web"}}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "worker-deployment", "labels": map[string]any{"app.kubernetes.io/name": "worker"}}},
		{"kind": "Namespace", "metadata": map[string]any{"name": "shared-namespace"}},
	}}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{{ID: "space-id", Organization: "team", Name: "dev", Cluster: "target", Namespace: "borq-fixture-dev", Configuration: state.Configuration}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/organizations/team/spaces/dev/apps":
			output = state
		case "/api/v1/organizations/team/spaces/dev/environment-groups":
			output = []appapi.EnvironmentGroup{{Name: "web-config", Version: 4, Values: []appapi.Variable{{Name: "PRIVATE", Value: "do-not-render-environment-value"}}}}
		case "/api/v1/plans/new-plan":
			output = appapi.Plan{ID: "new-plan", Space: appapi.Space{ID: "space-id"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "planned", Image: "new-image"}}}}
		default:
			t.Errorf("unexpected API request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	return web
}

// TestApplicationEditorRoundTrip owns editing the complete application form and
// switching to YAML without losing independent probes, routes, task budgets or zeros.
func TestApplicationEditorRoundTrip(t *testing.T) {
	assert := is.New(t)
	var submitted appapi.Patch
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/plans")
		assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
		_, _ = writer.Write([]byte(`{"id":"app-plan"}`))
	}))
	assert.NoErr(err)
	baseline := `{"name":"web","image":"old-image","port":8080,"replicas":1,"checks":{"readiness":{"enabled":true,"port":8081,"periodSeconds":13,"failureThreshold":4},"startup":{"enabled":true,"port":8080}},"ingresses":[{"name":"site","host":"web.example.com"}],"predeploy":{"command":"run-migrations","timeoutSeconds":300}}`
	form := url.Values{
		"format": {"fields"}, "application": {"web"}, "document": {baseline}, "name": {"web"}, "image": {"new-image"}, "port": {"9090"}, "replicas": {"0"}, "command": {`./app serve --label "hello world"`},
		"environmentVersion": {"4"}, "readiness": {"on"}, "readinessPath": {"/ready"}, "readinessPort": {"8082"}, "readinessPeriodSeconds": {"17"}, "readinessTimeoutSeconds": {"5"}, "readinessFailureThreshold": {"6"},
		"startupPath": {"/start"}, "startupPort": {"8083"}, "startupPeriodSeconds": {"19"}, "startupTimeoutSeconds": {"7"}, "startupFailureThreshold": {"8"},
		"predeployCommand": {`./app migrate --name "release one"`}, "predeployTimeoutSeconds": {"601"}, "predeployBackoffLimit": {"0"},
		"ingressFields": {"on"}, "savedIngress": {"site"}, "ingressName": {"site", "api"}, "ingressHost": {"new.example.com", "api.example.com"}, "ingressPath": {"/web", "/v1"}, "yamlMode": {"on"},
		"serviceNamespace": {"borq-fixture-dev"}, "environmentChoice": {"web-config", "worker-config"},
	}
	yamlForm := convertApplicationEditor(t, web, form)
	assert.Equal(yamlForm.Get("format"), "yaml")
	assert.True(!strings.Contains(yamlForm.Get("configuration"), "apps:"))
	assert.True(strings.Contains(yamlForm.Get("configuration"), "replicas: 0"))
	yamlForm.Del("yamlMode")
	fields := convertApplicationEditor(t, web, yamlForm)
	assert.Equal(fields.Get("image"), "new-image")
	assert.Equal(fields.Get("replicas"), "0")
	assert.Equal(fields.Get("application"), "web")
	assert.Equal(fields.Get("savedIngress"), "site")
	assert.Equal(fields.Get("serviceNamespace"), "borq-fixture-dev")
	assert.Equal(fields["environmentChoice"], []string{"web-config", "worker-config"})
	fields.Set("action", "plan-app")
	fields.Set("organization", "team")
	fields.Set("space", "dev")
	response := postForm(web, fields, false)
	assert.Equal(response.Code, http.StatusSeeOther)
	location, err := url.Parse(response.Header().Get("Location"))
	assert.NoErr(err)
	assert.Equal(location.Query().Get("view"), "application")
	assert.Equal(location.Query().Get("app"), "web")
	assert.Equal(len(*submitted.Apps), 1)
	patch := (*submitted.Apps)[0]
	assert.Equal(*patch.Replicas, 0)
	assert.Equal(*patch.Image, "new-image")
	assert.Equal(*patch.Command, `./app serve --label "hello world"`)
	assert.Equal(*patch.Env.Version, "4")
	assert.True(patch.Env.Name == nil)
	assert.Equal(*patch.Checks.Readiness.Enabled, true)
	assert.Equal(*patch.Checks.Readiness.Path, "/ready")
	assert.Equal(*patch.Checks.Readiness.Port, 8082)
	assert.Equal(*patch.Checks.Readiness.PeriodSeconds, 17)
	assert.Equal(*patch.Checks.Readiness.TimeoutSeconds, 5)
	assert.Equal(*patch.Checks.Readiness.FailureThreshold, 6)
	assert.Equal(*patch.Checks.Startup.Port, 8083)
	assert.Equal(*patch.Checks.Startup.Enabled, false)
	assert.Equal(*patch.Checks.Startup.Path, "/start")
	assert.Equal(*patch.Checks.Startup.PeriodSeconds, 19)
	assert.Equal(*patch.Checks.Startup.TimeoutSeconds, 7)
	assert.Equal(*patch.Checks.Startup.FailureThreshold, 8)
	assert.Equal(len(*patch.Ingresses), 2)
	assert.Equal((*patch.Ingresses)[0].Name, "site")
	assert.Equal((*patch.Ingresses)[0].Host, "new.example.com")
	assert.Equal(*(*patch.Ingresses)[0].Path, "/web")
	assert.Equal((*patch.Ingresses)[1].Name, "api")
	assert.Equal((*patch.Ingresses)[1].Host, "api.example.com")
	assert.Equal(*(*patch.Ingresses)[1].Path, "/v1")
	assert.Equal(*patch.Predeploy.Command, `./app migrate --name "release one"`)
	assert.Equal(*patch.Predeploy.TimeoutSeconds, 601)
	assert.Equal(*patch.Predeploy.BackoffLimit, 0)
	// A partial YAML edit also gets the existing fields back when switching to the form.
	yamlForm.Set("configuration", "name: web\nreplicas: 2\n")
	partial := convertApplicationEditor(t, web, yamlForm)
	assert.Equal(partial.Get("image"), "new-image")
	assert.Equal(partial.Get("replicas"), "2")
	assert.Equal(partial.Get("readinessPort"), "8082")
	assert.Equal(partial.Get("predeployTimeoutSeconds"), "601")
	assert.Equal(partial["ingressHost"], []string{"new.example.com", "api.example.com"})
	// An empty command explicitly disables a task without discarding its budget.
	partial.Set("predeployCommand", "")
	partial.Set("port", "0")
	partial.Set("yamlMode", "on")
	cleared := convertApplicationEditor(t, web, partial)
	var disabled appapi.AppPatch
	document, err := decodeYaml(cleared.Get("configuration"))
	assert.NoErr(err)
	assert.NoErr(json.Unmarshal(document, &disabled))
	assert.Equal(*disabled.Predeploy.Command, "")
	assert.Equal(*disabled.Predeploy.TimeoutSeconds, 601)
	assert.Equal(*disabled.Checks.Readiness.Enabled, true)
	assert.Equal(*disabled.Checks.Readiness.Port, 8082)
}

// TestApplicationRouteDrafts exercises route addition and removal through the
// actual editor endpoint, including explicit empty lists and protected saved rows.
func TestApplicationRouteDrafts(t *testing.T) {
	assert := is.New(t)
	web := applicationTestHandler(t)
	form := url.Values{"format": {"yaml"}, "configuration": {"name: new-app\nimage: nginx\nport: 8080\n"}}
	fields := convertApplicationEditor(t, web, form)
	assert.Equal(fields.Get("readinessPort"), "")
	assert.Equal(fields.Get("startupPort"), "")
	fields.Set("port", "80")
	fields.Set("editorAction", "add-ingress")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(fields.Get("readinessPort"), "")
	assert.Equal(fields.Get("startupPort"), "")
	assert.Equal(fields["ingressName"], []string{""})
	assert.Equal(fields["ingressPath"], []string{"/"})
	fields.Set("ingressName", "public")
	fields.Set("ingressHost", "app.example.com")
	fields.Set("editorAction", "add-ingress")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(len(fields["ingressName"]), 2)
	fields["ingressName"][1], fields["ingressHost"][1], fields["ingressPath"][1] = "api", "api.example.com", "/v1"
	fields.Set("removeIngress", "0")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(fields["ingressName"], []string{"api"})
	assert.Equal(fields["ingressHost"], []string{"api.example.com"})
	assert.Equal(fields["ingressPath"], []string{"/v1"})
	fields.Set("removeIngress", "0")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(len(fields["ingressName"]), 0)
	fields.Set("yamlMode", "on")
	cleared := convertApplicationEditor(t, web, fields)
	assert.True(strings.Contains(cleared.Get("configuration"), "ingresses: []"))
	// Normal form submissions also return the draft, rather than navigating away.
	fields.Del("yamlMode")
	fields.Set("editorAction", "add-ingress")
	fields.Set("organization", "team")
	fields.Set("space", "dev")
	request := httptest.NewRequest(http.MethodPost, "/ui/application-editor", strings.NewReader(fields.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "<!doctype html>"))
	assert.Equal(applicationEditorFields(t, response.Body.String()).Get("name"), "new-app")
	fields.Set("ingressName", "public")
	fields.Set("ingressHost", "app.example.com")
	fields.Set("ingressPath", "/")
	fields.Set("savedIngress", "public")
	fields.Set("removeIngress", "0")
	response = postApplicationEditor(web, fields)
	assert.Equal(response.Code, http.StatusBadRequest)
	assert.True(strings.Contains(response.Body.String(), "cannot be removed yet"))
}

// TestApplicationFieldErrors reaches form-specific parsing guards before an API
// request can receive truncated arrays, non-integers, or invalid execution budgets.
func TestApplicationFieldErrors(t *testing.T) {
	for _, input := range []struct{ field, value, message string }{
		{"readinessPort", "not-a-port", "readiness port must be"},
		{"startupPeriodSeconds", "0", "startup interval must be"},
		{"readinessTimeoutSeconds", "-1", "readiness timeout must be"},
		{"startupFailureThreshold", "2147483648", "startup failure threshold must be"},
		{"predeployTimeoutSeconds", "0", "Predeploy timeout must be"},
		{"predeployBackoffLimit", "-1", "Predeploy retries must be"},
		{"ingressName", "public", "Each route needs"},
	} {
		t.Run(input.field, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid fields reached the API") }))
			assert.NoErr(err)
			form := url.Values{"format": {"fields"}, "port": {"8080"}, "replicas": {"1"}, "predeployCommand": {"./app migrate"}, "ingressFields": {"on"}}
			form.Set(input.field, input.value)
			response := postApplicationEditor(web, form)
			assert.Equal(response.Code, http.StatusBadRequest)
			assert.True(strings.Contains(response.Body.String(), input.message))
		})
	}
}

func postApplicationEditor(web http.Handler, form url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/ui/application-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	return response
}

func convertApplicationEditor(t *testing.T, web http.Handler, form url.Values) url.Values {
	t.Helper()
	response := postApplicationEditor(web, form)
	if response.Code != http.StatusOK {
		t.Fatalf("application editor returned %d: %s", response.Code, response.Body.String())
	}
	return applicationEditorFields(t, response.Body.String())
}

// TestApplicationEditorInvalidYaml keeps malformed conversions from dropping
// unsupported or null settings while leaving schema validation with the API on review.
func TestApplicationEditorInvalidYaml(t *testing.T) {
	for _, input := range []struct{ source, message string }{
		{"name: web\nprivateKey: do-not-echo-secret", "supported application fields"},
		{"name: web\nreplicas: false", "supported application fields"},
		{"name: web\nchecks:\n  readiness: null", "instead of null"},
	} {
		t.Run(input.message+input.source[:9], func(t *testing.T) {
			assert := is.New(t)
			web := newTestHandler(t)
			request := httptest.NewRequest(http.MethodPost, "/ui/application-editor", strings.NewReader(url.Values{"format": {"yaml"}, "application": {"web"}, "configuration": {input.source}}.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("HX-Request", "true")
			response := httptest.NewRecorder()
			web.ServeHTTP(response, request)
			assert.Equal(response.Code, http.StatusBadRequest)
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
}

func applicationEditorFields(t *testing.T, source string) url.Values {
	t.Helper()
	assert := is.New(t)
	document, err := html.Parse(strings.NewReader(source))
	assert.NoErr(err)
	var editor *html.Node
	for node := range document.Descendants() {
		for _, attribute := range node.Attr {
			if attribute.Key == "id" && attribute.Val == "application-editor" {
				editor = node
			}
		}
	}
	if editor == nil {
		t.Fatal("application editor was not rendered")
	}
	values := url.Values{}
	for node := range editor.Descendants() {
		if node.Data != "input" && node.Data != "textarea" && node.Data != "select" {
			continue
		}
		attributes := map[string]string{}
		for _, attribute := range node.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		if attributes["type"] == "checkbox" {
			if _, checked := attributes["checked"]; checked {
				values.Add(attributes["name"], "on")
			}
			continue
		}
		value := attributes["value"]
		if node.Data == "select" {
			first := true
			for option := range node.Descendants() {
				if option.Data != "option" {
					continue
				}
				optionValue, selected := "", false
				for _, attribute := range option.Attr {
					if attribute.Key == "value" {
						optionValue = attribute.Val
					}
					if attribute.Key == "selected" {
						selected = true
					}
				}
				if first || selected {
					value = optionValue
				}
				first = false
			}
		}
		if node.Data == "textarea" && node.FirstChild != nil {
			value = node.FirstChild.Data
		}
		values.Add(attributes["name"], value)
	}
	return values
}
