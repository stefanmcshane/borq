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
	"strconv"
)

// TestApplicationSqliteFields owns checkbox submission and preservation across editor modes.
func TestApplicationSqliteFields(t *testing.T) {
	assert := is.New(t)
	form := url.Values{"name": {"web"}, "image": {"example/app"}, "port": {"0"}, "replicas": {"1"}, "readinessPath": {"/"}, "startupPath": {"/"}, "sqliteFields": {"true"}, "sqliteEnabled": {"on"}}
	patch, err := applicationFields(form)
	assert.NoErr(err)
	assert.True(*patch.Sqlite.Enabled && *patch.Sqlite.Backups)
	form.Del("sqliteEnabled")
	form.Set("sqliteRetained", "true")
	form.Set("sqliteBackupFields", "true")
	patch, err = applicationFields(form)
	assert.NoErr(err)
	assert.True(*patch.Sqlite.Enabled && !*patch.Sqlite.Backups)
	encoded, err := json.Marshal(patch)
	assert.NoErr(err)
	form.Set("document", string(encoded))
	form.Del("sqliteFields")
	patch, err = applicationFields(form)
	assert.NoErr(err)
	assert.True(*patch.Sqlite.Enabled && !*patch.Sqlite.Backups)
}

// TestApplicationSqliteDrafts keeps unsaved storage reversible across route and YAML edits.
func TestApplicationSqliteDrafts(t *testing.T) {
	for _, name := range []string{"", "web", "worker"} {
		t.Run("app="+name, func(t *testing.T) {
			assert := is.New(t)
			web := applicationTestHandler(t)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=application&organization=team&space=dev&tab=configuration&app="+name, nil))
			page := decodeView[pageData](t, response, "workspace")
			retained := name == "worker"
			assert.Equal(page.ApplicationEditor.SqliteRetained, retained)
			form := applicationEditorFields(t, response.Body.String())
			form.Set("predeployTimeoutSeconds", "300")
			for _, check := range []string{"readiness", "startup"} {
				form.Set(check+"PeriodSeconds", "10")
				form.Set(check+"TimeoutSeconds", "1")
				form.Set(check+"FailureThreshold", "3")
			}
			if name == "" {
				form.Set("name", "new-app")
				form.Set("image", "example/app")
			}
			if !retained {
				form.Set("sqliteEnabled", "on")
			}
			form.Set("editorAction", "add-ingress")
			response = postApplicationEditor(web, form)
			editor := decodeView[applicationEditorData](t, response, "application-editor")
			assert.True(editor.Draft.Sqlite.Enabled && editor.Draft.Sqlite.Backups)
			assert.Equal(editor.SqliteRetained, retained)
			form = applicationEditorFields(t, response.Body.String())
			form.Set("editorFormat", "yaml")
			form = convertApplicationEditor(t, web, form)
			form.Set("configuration", strings.ReplaceAll(form.Get("configuration"), "backups: true", "backups: false"))
			form.Set("editorFormat", "fields")
			form = convertApplicationEditor(t, web, form)
			assert.Equal(form.Get("sqliteRetained"), strconv.FormatBool(retained))
			assert.Equal(form.Get("sqliteBackups"), "")
			if !retained {
				assert.Equal(form.Get("sqliteEnabled"), "on")
				form.Del("sqliteEnabled")
			}
			response = postApplicationEditor(web, form)
			editor = decodeView[applicationEditorData](t, response, "application-editor")
			assert.Equal(editor.Draft.Sqlite.Enabled, retained)
			assert.True(!editor.Draft.Sqlite.Backups)
		})
	}
}

// TestApplicationPages keeps the list separate from editing and limits saved
// configuration and manifests to the application selected in this space.
func TestApplicationPages(t *testing.T) {
	for _, input := range []struct {
		query, tab, subtab, app string
		status                  int
	}{
		{"view=applications", "", "", "", 200}, {"view=application", "configuration", "fields", "", 200},
		{"view=application&app=web&tab=general", "configuration", "fields", "web", 200},
		{"view=application&app=web", "overview", "", "web", 200},
		{"view=application&app=web&tab=observability", "observability", "metrics", "web", 200},
		{"view=application&app=web&tab=logs", "observability", "logs", "web", 200},
		{"view=application&app=web&tab=deployments", "overview", "", "web", 200},
		{"view=application&app=worker", "overview", "", "worker", 200},
		{"view=application&app=web&tab=configuration&subtab=manifests", "configuration", "manifests", "web", 200},
		{"view=application&app=planned&plan=new-plan", "configuration", "fields", "planned", 200},
		{"view=application&app=planned&tab=configuration&draft=new-plan", "configuration", "fields", "planned", 200},
		{"view=application&app=web&tab=configuration&draft=new-plan", "", "", "", 404},
		{"view=application&app=web&tab=configuration&draft=foreign-plan", "", "", "", 404},
		{"view=application&app=web&plan=foreign-plan", "", "", "", 404},
		{"view=application&app=web&operation=foreign-operation", "", "", "", 404},
		{"view=application&app=missing", "", "", "", 404}, {"view=application&app=web&tab=missing", "", "", "", 404},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			page := requestPage(t, applicationTestHandler(t), "organization=team&space=dev&"+input.query, input.status)
			if input.status != 200 {
				return
			}
			assert.Equal(page.ApplicationTab, input.tab)
			assert.Equal(page.ApplicationSubtab, input.subtab)
			if input.app != "" {
				assert.Equal(page.Draft.Name, input.app)
			}
			if strings.Contains(input.query, "manifests") {
				assert.Equal(len(page.ApplicationManifests), 1)
				assert.Equal(page.ApplicationManifests[0]["metadata"].(map[string]any)["name"], "web-deployment")
			}
		})
	}
}

// TestWorkspaceDataScope prevents unrelated configuration from crossing the JSON boundary.
func TestWorkspaceDataScope(t *testing.T) {
	web := applicationTestHandler(t)
	for _, query := range []string{
		"view=overview", "view=spaces", "view=applications", "view=application",
		"view=application&app=web", "view=application&app=web&tab=observability",
		"view=application&app=web&tab=configuration", "view=application&app=web&tab=configuration&subtab=yaml",
		"view=application&app=web&tab=configuration&subtab=manifests",
		"view=application&app=web&plan=scope-plan",
	} {
		t.Run(query, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?organization=team&space=dev&"+query, nil))
			page := decodeView[pageData](t, response, "workspace")
			for _, private := range []string{"worker-command-private", "worker-manifest-private", "other-space-private", "plan-values-private", "resource-body-private"} {
				if strings.Contains(response.Body.String(), private) {
					t.Errorf("unselected data %q in workspace response", private)
				}
			}
			if strings.Contains(query, "app=web") {
				assert.Equal(page.Application.Command, "web-selected-command")
				assert.Equal(len(page.ApplicationManifests), 1)
			} else {
				assert.True(!strings.Contains(response.Body.String(), "web-selected-command"))
				assert.Equal(len(page.ApplicationManifests), 0)
			}
			if query == "view=applications" {
				assert.Equal(len(page.Applications.Configuration.Apps), 2)
				assert.Equal(page.Applications.Configuration.Apps[1].Image, "worker-image")
			}
			if strings.Contains(query, "plan=scope-plan") {
				assert.Equal(page.Draft.Name, "web")
				assert.Equal(page.Plan.ConfigurationDiff, "selected change\n")
				assert.Equal(page.Plan.Manifests, []appapi.Manifest{{"data": "reviewed-artifact"}})
			}
		})
	}
}

// TestApplicationEnvironmentDropdown keeps selection in the app editor, lists
// published group names without values, and preserves a choice through YAML mode.
func TestApplicationEnvironmentDropdown(t *testing.T) {
	assert := is.New(t)
	web := applicationTestHandler(t)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=application&organization=team&space=dev", nil))
	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(page.ApplicationEditor.GroupNames, []string{"web-config"})
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
		{Name: "web", Image: "web-image", Port: 8080, Replicas: 1, Command: "web-selected-command"},
		{Name: "worker", Image: "worker-image", Port: 0, Replicas: 1, Command: "worker-command-private", Sqlite: &appapi.SqliteSettings{Enabled: true, Backups: true}},
	}}, Manifests: []appapi.Manifest{
		{"kind": "Deployment", "metadata": map[string]any{"name": "web-deployment", "labels": map[string]any{"app.kubernetes.io/name": "web"}}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "worker-deployment", "labels": map[string]any{"app.kubernetes.io/name": "worker"}}, "data": "worker-manifest-private"},
		{"kind": "Namespace", "metadata": map[string]any{"name": "shared-namespace"}},
	}}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{
				{ID: "space-id", Organization: "team", Name: "dev", Cluster: "target", Namespace: "borq-fixture-dev", Configuration: state.Configuration},
				{ID: "other-id", Organization: "team", Name: "other", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "other", Command: "other-space-private"}}}},
			}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/organizations/team/spaces/dev/route-policy":
			output = appapi.ApplicationRoutePolicy{CertificateIssuer: "cluster-issuer", Domains: []string{"example.com"}}
		case "/api/v1/organizations/team/spaces/dev/apps":
			output = state
		case "/api/v1/organizations/team/spaces/dev/environment-groups":
			output = []appapi.EnvironmentGroup{{Name: "web-config", Version: 4, Values: []appapi.Variable{{Name: "PRIVATE", Value: "do-not-render-environment-value"}}}}
		case "/api/v1/operations/foreign-operation":
			output = appapi.Operation{ID: "foreign-operation", SpaceID: "other-space"}
		case "/api/v1/plans/foreign-plan":
			output = appapi.Plan{ID: "foreign-plan", Space: appapi.Space{ID: "other-space"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "web", Image: "foreign-image"}}}}
		case "/api/v1/plans/new-plan":
			output = appapi.Plan{ID: "new-plan", Space: appapi.Space{ID: "space-id"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "planned", Image: "new-image"}}}}
		case "/api/v1/plans/scope-plan":
			output = appapi.Plan{
				ID: "scope-plan", Space: appapi.Space{ID: "space-id", Configuration: state.Configuration},
				Configuration: state.Configuration, Before: state.Configuration,
				ConfigurationDiff: "selected change\n", Manifests: []appapi.Manifest{{"data": "reviewed-artifact"}},
				Values:        map[string]any{"private": "plan-values-private"},
				ResourceDiffs: []appapi.ResourceDiff{{Kind: "Deployment", Name: "web", Action: "update", Desired: appapi.Manifest{"data": "resource-body-private"}}},
			}
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
		if serveActionPageFixture(writer, request) {
			return
		}
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/plans")
		assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
		_, _ = writer.Write([]byte(`{"id":"app-plan"}`))
	}))
	assert.NoErr(err)
	baseline := `{"name":"web","image":"old-image","port":8080,"replicas":1,"checks":{"readiness":{"enabled":true,"port":8081,"periodSeconds":13,"failureThreshold":4},"startup":{"enabled":true,"port":8080}},"ingresses":[{"name":"site","host":"web.example.com"}],"predeploy":{"command":"run-migrations","timeoutSeconds":300}}`
	form := url.Values{
		"organization": {"team"}, "space": {"dev"}, "format": {"fields"}, "application": {"web"}, "document": {baseline}, "name": {"  web  "}, "image": {" new-image\t "}, "port": {" 9090 "}, "replicas": {" 0 "}, "command": {`./app serve --label "hello world"`},
		"environmentVersion": {" 4 "}, "readiness": {"on"}, "readinessPath": {" /ready "}, "readinessPort": {" 8082 "}, "readinessPeriodSeconds": {"17"}, "readinessTimeoutSeconds": {"5"}, "readinessFailureThreshold": {"6"},
		"startupPath": {"/start"}, "startupPort": {"8083"}, "startupPeriodSeconds": {"19"}, "startupTimeoutSeconds": {"7"}, "startupFailureThreshold": {"8"},
		"predeployCommand": {`./app migrate --name "release one"`}, "predeployTimeoutSeconds": {"601"}, "predeployBackoffLimit": {"0"},
		"ingressFields": {"on"}, "savedIngress": {"site"}, "ingressName": {" site ", " api "}, "ingressHost": {" new.example.com ", " api.example.com "}, "ingressPath": {" /web ", " /v1 "}, "yamlMode": {"on"},
		"serviceNamespace": {"borq-fixture-dev"}, "environmentChoice": {"web-config", "worker-config"},
	}
	yamlForm := convertApplicationEditor(t, web, form)
	assert.Equal(yamlForm.Get("format"), "yaml")
	assert.True(!strings.Contains(yamlForm.Get("configuration"), "apps:"))
	assert.True(strings.Contains(yamlForm.Get("configuration"), "replicas: 0"))
	yamlForm.Set("configuration", strings.ReplaceAll(yamlForm.Get("configuration"), "name: web", "name: ' web '"))
	yamlForm.Set("configuration", strings.ReplaceAll(yamlForm.Get("configuration"), "image: new-image", "image: ' new-image '"))
	assert.True(strings.Contains(yamlForm.Get("configuration"), "' web '"))
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
	response := postForm(web, fields)
	location, err := url.Parse(decodeView[pageData](t, response, "workspace").Location)
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
	assert.Equal(partial["ingressSubdomain"], []string{"new", "api"})
	assert.Equal(partial["ingressDomain"], []string{"example.com", "example.com"})
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
	// Submit both an empty command and an omitted successful control against an
	// existing nonempty hidden document. Neither may resurrect the saved task.
	for _, omitted := range []bool{false, true} {
		fields := url.Values{"action": {"plan-app"}, "organization": {"team"}, "space": {"dev"}, "application": {"web"}, "name": {"web"}, "document": {baseline}, "image": {"new-image"}, "port": {"8080"}, "replicas": {"1"}, "predeployCommand": {""}}
		if omitted {
			fields.Del("predeployCommand")
		}
		decodeView[pageData](t, postForm(web, fields), "workspace")
		assert.True((*submitted.Apps)[0].Predeploy.Command != nil)
		assert.Equal(*(*submitted.Apps)[0].Predeploy.Command, "")
	}

}

// TestApplicationRouteDrafts exercises route addition and removal through the
// actual editor endpoint, including explicit empty lists and protected saved rows.
func TestApplicationRouteDrafts(t *testing.T) {
	assert := is.New(t)
	web := applicationTestHandler(t)
	form := url.Values{"organization": {"team"}, "space": {"dev"}, "format": {"yaml"}, "configuration": {"name: new-app\nimage: nginx\nport: 8080\n"}}
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
	fields.Set("ingressSubdomain", "app")
	fields.Set("editorAction", "add-ingress")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(len(fields["ingressName"]), 2)
	fields["ingressName"][1], fields["ingressSubdomain"][1], fields["ingressPath"][1] = "api", "api", "/v1"
	fields.Set("removeIngress", "0")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(fields["ingressName"], []string{"api"})
	assert.Equal(fields["ingressSubdomain"], []string{"api"})
	assert.Equal(fields["ingressPath"], []string{"/v1"})
	fields.Set("removeIngress", "0")
	fields = convertApplicationEditor(t, web, fields)
	assert.Equal(len(fields["ingressName"]), 0)
	fields.Set("yamlMode", "on")
	cleared := convertApplicationEditor(t, web, fields)
	assert.True(strings.Contains(cleared.Get("configuration"), "ingresses: []"))
	// Editor submissions return the draft without loading another workspace.
	fields.Del("yamlMode")
	fields.Set("editorAction", "add-ingress")
	fields.Set("organization", "team")
	fields.Set("space", "dev")
	request := httptest.NewRequest(http.MethodPost, "/api/ui/application-editor", strings.NewReader(fields.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(decodeView[applicationEditorData](t, response, "application-editor").Draft.Name, "new-app")
	assert.Equal(applicationEditorFields(t, response.Body.String()).Get("name"), "new-app")
	fields.Set("ingressName", "public")
	fields.Set("ingressSubdomain", "app")
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
	request := httptest.NewRequest(http.MethodPost, "/api/ui/application-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
			request := httptest.NewRequest(http.MethodPost, "/api/ui/application-editor", strings.NewReader(url.Values{"format": {"yaml"}, "application": {"web"}, "configuration": {input.source}}.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
	var envelope struct {
		Component string          `json:"component"`
		Data      json.RawMessage `json:"data"`
	}
	assert.NoErr(json.Unmarshal([]byte(source), &envelope))
	var editor applicationEditorData
	if envelope.Component == "workspace" {
		var page pageData
		assert.NoErr(json.Unmarshal(envelope.Data, &page))
		editor = page.ApplicationEditor
	} else {
		assert.Equal(envelope.Component, "application-editor")
		assert.NoErr(json.Unmarshal(envelope.Data, &editor))
	}
	draft := editor.Draft
	values := url.Values{"organization": {editor.Organization}, "space": {editor.Space}, "format": {editor.Format}, "application": {editor.ExistingName}, "document": {editor.Document}, "configuration": {editor.Yaml}, "name": {draft.Name}, "image": {draft.Image}, "port": {strconv.Itoa(draft.Port)}, "replicas": {strconv.Itoa(draft.Replicas)}, "command": {draft.Command}, "environment": {draft.Env.Name}, "environmentVersion": {draft.Env.Version}, "predeployCommand": {draft.Predeploy.Command}, "predeployTimeoutSeconds": {strconv.Itoa(draft.Predeploy.TimeoutSeconds)}, "predeployBackoffLimit": {strconv.Itoa(draft.Predeploy.BackoffLimit)}, "serviceNamespace": {editor.Namespace}, "ingressFields": {"on"}}
	values.Set("secretDraft", editor.SecretDraft)
	for _, variable := range editor.SecretValues {
		values.Add("variableName", variable.Name)
		values.Add("variableValue", variable.Value)
	}
	values["environmentChoice"] = editor.GroupNames
	values["savedIngress"] = editor.SavedIngresses
	values.Set("sqliteRetained", strconv.FormatBool(editor.SqliteRetained))
	if editor.Format == "fields" {
		values.Set("sqliteFields", "true")
		if !editor.SqliteRetained && draft.Sqlite != nil && draft.Sqlite.Enabled {
			values.Set("sqliteEnabled", "on")
		}
		if editor.SqliteRetained || (draft.Sqlite != nil && draft.Sqlite.Enabled) {
			values.Set("sqliteBackupFields", "true")
			if draft.Sqlite.Backups {
				values.Set("sqliteBackups", "on")
			}
		}
	}
	for _, check := range editor.Checks {
		if check.Check.Enabled {
			values.Set(check.Name, "on")
		}
		values.Set(check.Name+"Path", check.Check.Path)
		if check.PortSet {
			values.Set(check.Name+"Port", strconv.Itoa(check.Check.Port))
		}
		values.Set(check.Name+"PeriodSeconds", strconv.Itoa(check.Check.PeriodSeconds))
		values.Set(check.Name+"TimeoutSeconds", strconv.Itoa(check.Check.TimeoutSeconds))
		values.Set(check.Name+"FailureThreshold", strconv.Itoa(check.Check.FailureThreshold))
	}
	for _, ingress := range editor.Ingresses {
		values.Add("ingressName", ingress.Route.Name)
		values.Add("ingressSubdomain", ingress.Subdomain)
		values.Add("ingressDomain", ingress.Domain)
		values.Add("ingressPath", ingress.Route.Path)
	}
	return values
}

// TestApplicationPlanReviewSections keeps review exclusive and returns Cancel to
// its originating section, including the selected editor or observability view.
func TestApplicationPlanReviewSections(t *testing.T) {
	for _, input := range []struct{ tab, subtab string }{{"overview", ""}, {"configuration", "yaml"}, {"observability", "metrics"}} {
		t.Run(input.tab, func(t *testing.T) {
			assert := is.New(t)
			query := url.Values{"view": {"application"}, "organization": {"team"}, "space": {"dev"}, "app": {"web"}, "tab": {input.tab}, "subtab": {input.subtab}, "plan": {"new-plan"}}
			page := requestPage(t, applicationTestHandler(t), query.Encode(), http.StatusOK)
			assert.Equal(page.Plan.ID, "new-plan")
			location, err := url.Parse(page.ReviewReturnUrl)
			assert.NoErr(err)
			assert.Equal(location.Query().Get("tab"), input.tab)
			assert.Equal(location.Query().Get("subtab"), input.subtab)
			assert.Equal(location.Query().Get("app"), "web")
			assert.Equal(location.Query().Get("plan"), "")
		})
	}
}
