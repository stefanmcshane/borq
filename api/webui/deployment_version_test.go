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

// TestDeploymentVersionLinks offers older plans for reuse regardless of status,
// while excluding the most recent version from the action.
func TestDeploymentVersionLinks(t *testing.T) {
	assert := is.New(t)
	statuses := []appapi.OperationStatus{appapi.Queued, appapi.Pending, appapi.Applying, appapi.Waiting, appapi.Canceling, appapi.Canceled, appapi.Superseded, appapi.Succeeded, appapi.Failed}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/apps/web/deployments")
		deployments := []appapi.Deployment{{Operation: appapi.Operation{ID: "operation-latest", PlanID: "plan-latest", Status: appapi.Succeeded}}}
		for _, status := range statuses {
			deployments = append(deployments, appapi.Deployment{Operation: appapi.Operation{ID: "operation-" + string(status), PlanID: "plan-" + string(status), Status: status}})
		}
		assert.NoErr(json.NewEncoder(writer).Encode(deployments))
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/application?organization=team&space=dev&app=web&panel=deployments", nil))
	assert.Equal(response.Code, http.StatusOK)
	document, err := html.Parse(strings.NewReader(response.Body.String()))
	assert.NoErr(err)
	sources := []string{}
	for node := range document.Descendants() {
		if node.Data != "a" || node.FirstChild == nil || node.FirstChild.Data != "New version from this plan" {
			continue
		}
		for _, attribute := range node.Attr {
			if attribute.Key != "href" {
				continue
			}
			location, err := url.Parse(attribute.Val)
			assert.NoErr(err)
			assert.Equal(location.Query().Get("view"), "application")
			assert.Equal(location.Query().Get("organization"), "team")
			assert.Equal(location.Query().Get("space"), "dev")
			assert.Equal(location.Query().Get("app"), "web")
			assert.Equal(location.Query().Get("tab"), "configuration")
			assert.Equal(location.Query().Get("plan"), "") // Opening the editor must not open an apply-ready plan.
			sources = append(sources, location.Query().Get("draft"))
		}
	}
	assert.Equal(sources, []string{"plan-queued", "plan-pending", "plan-applying", "plan-waiting", "plan-canceling", "plan-canceled", "plan-superseded", "plan-succeeded", "plan-failed"})
	assert.True(!strings.Contains(response.Body.String(), "Restore"))
}

// TestNewVersionFromPlanEdits starts from historical settings, edits in either
// mode, and submits a fresh app-only plan without applying the old plan.
func TestNewVersionFromPlanEdits(t *testing.T) {
	for _, format := range []string{"fields", "yaml"} {
		t.Run(format, func(t *testing.T) {
			assert := is.New(t)
			current := appapi.App{Name: "web", Image: "current-image", Port: 9090, Replicas: 3}
			old := appapi.App{Name: "web", Image: "old-image", Port: 8080, Replicas: 0, Command: "serve-old", Env: appapi.EnvReference{Name: "web-config", Version: "2"},
				Predeploy: appapi.Predeploy{Command: "migrate-old", TimeoutSeconds: 300},
				Checks:    appapi.Checks{Readiness: appapi.Check{Port: 8081, Path: "/ready", PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3}, Startup: appapi.Check{Port: 8082, Path: "/start", PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3}}}
			plansCreated, applies := 0, 0
			var submitted appapi.Patch
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch request.URL.Path {
				case "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}}
				case "/api/v1/organizations/team/spaces":
					output = []appapi.Space{{ID: "space-id", Organization: "team", Name: "dev", Cluster: "target"}}
				case "/api/v1/organizations/team/clusters":
					output = []appapi.Cluster{}
				case "/api/v1/organizations/team/spaces/dev/apps":
					output = appapi.ApplicationState{Configuration: appapi.Configuration{Apps: []appapi.App{current}}}
				case "/api/v1/organizations/team/spaces/dev/environment-groups":
					output = []appapi.EnvironmentGroup{{Name: "web-config", Version: 4}}
				case "/api/v1/plans/old-plan":
					output = appapi.Plan{ID: "old-plan", Space: appapi.Space{ID: "space-id"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "worker", Image: "other-app-image"}, old}}}
				case "/api/v1/organizations/team/spaces/dev/plans":
					assert.Equal(request.Method, http.MethodPost)
					plansCreated++
					assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
					output = appapi.Plan{ID: "fresh-plan"}
				default:
					if strings.HasSuffix(request.URL.Path, "/apply") {
						applies++
					}
					t.Errorf("unexpected version request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				assert.NoErr(json.NewEncoder(writer).Encode(output))
			}))
			assert.NoErr(err)
			query := url.Values{"view": {"application"}, "organization": {"team"}, "space": {"dev"}, "app": {"web"}, "tab": {"configuration"}, "subtab": {format}, "draft": {"old-plan"}}
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?"+query.Encode(), nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.True(!strings.Contains(response.Body.String(), "Review deployment plan"))
			assert.Equal(plansCreated, 0)
			form := applicationEditorFields(t, response.Body.String())
			if format == "yaml" {
				assert.True(strings.Contains(form.Get("configuration"), "image: old-image"))
				assert.True(strings.Contains(form.Get("configuration"), "replicas: 0"))
				assert.True(!strings.Contains(form.Get("configuration"), "other-app-image"))
				form.Set("configuration", strings.Replace(form.Get("configuration"), "old-image", "edited-image", 1))
			} else {
				assert.Equal(form.Get("image"), "old-image")
				assert.Equal(form.Get("replicas"), "0")
				assert.Equal(form.Get("command"), "serve-old")
				form.Set("image", "edited-image")
				form.Set("editorFormat", "yaml")
				form = convertApplicationEditor(t, web, form)
				assert.True(strings.Contains(form.Get("configuration"), "image: edited-image"))
				form.Set("editorFormat", "fields")
				form = convertApplicationEditor(t, web, form)
			}
			form.Set("action", "plan-app")
			form.Set("organization", "team")
			form.Set("space", "dev")
			response = postForm(web, form, false)
			assert.Equal(response.Code, http.StatusSeeOther)
			location, err := url.Parse(response.Header().Get("Location"))
			assert.NoErr(err)
			assert.Equal(location.Query().Get("plan"), "fresh-plan")
			assert.Equal(plansCreated, 1)
			assert.Equal(applies, 0)
			assert.True(submitted.Apps != nil)
			assert.Equal(len(*submitted.Apps), 1)
			patch := (*submitted.Apps)[0]
			assert.Equal(patch.Name, "web")
			assert.Equal(*patch.Image, "edited-image")
			assert.Equal(*patch.Port, 8080)
			assert.Equal(*patch.Replicas, 0)
			assert.Equal(*patch.Env.Name, "web-config")
			assert.Equal(*patch.Env.Version, "2")
			assert.Equal(*patch.Predeploy.Command, "migrate-old")
			assert.Equal(*patch.Checks.Readiness.Port, 8081)
		})
	}
}
