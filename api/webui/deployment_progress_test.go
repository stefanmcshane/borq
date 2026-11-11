package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

func TestApplicationDeploymentProgressScope(t *testing.T) {
	for _, entry := range []string{"initial page", "refresh", "cancel", "history", "space operation", "app ready", "app failed", "other space"} {
		t.Run(entry, func(t *testing.T) {
			assert := is.New(t)
			space := appapi.Space{ID: "space-id", Organization: "team", Name: "dev", Cluster: "target"}
			configuration := appapi.Configuration{Apps: []appapi.App{{Name: "web", Image: "nginx"}, {Name: "web-worker", Image: "worker"}}}
			plan := appapi.Plan{ID: "plan", Space: space, Configuration: configuration}
			operation := appapi.Operation{ID: "progress", PlanID: plan.ID, SpaceID: space.ID, Status: appapi.Waiting, Phase: "applications", Message: "Deployment web-worker: unrelated image pull error"}
			for _, resource := range []struct {
				version, kind, namespace, name, app, state, message string
			}{
				{"apps/v1", "Deployment", "namespace", "web-worker", "web-worker", "failed", "unrelated image pull error"},
				{"apps/v1", "Deployment", "namespace", "web", "web", "running", "selected app is starting"},
				{"networking.k8s.io/v1", "Ingress", "namespace", "public-route", "web", "succeeded", "selected route is ready"},
				{"batch/v1", "Job", "namespace", "web-0123456789ab-pre", "web", "succeeded", "selected predeploy completed"},
				{"v1", "Secret", "namespace", "shared", "", "succeeded", "unrelated shared secret"},
			} {
				plan.Manifests = append(plan.Manifests, appapi.Manifest{"apiVersion": resource.version, "kind": resource.kind, "metadata": map[string]any{"namespace": resource.namespace, "name": resource.name, "labels": map[string]any{"app.kubernetes.io/name": resource.app}}})
				operation.Resources = append(operation.Resources, appapi.ResourceStatus{APIVersion: resource.version, Kind: resource.kind, Namespace: resource.namespace, Name: resource.name, State: resource.state, Message: resource.message})
			}
			// Equal names in another namespace or API must not match the saved app resource.
			foreign := operation.Resources[1]
			foreign.Namespace, foreign.Message = "other", "unrelated namespace"
			operation.Resources = append(operation.Resources, foreign)
			foreign.Namespace, foreign.APIVersion, foreign.Message = "namespace", "other/v1", "unrelated API"
			operation.Resources = append(operation.Resources, foreign)
			if entry == "app ready" {
				operation.Status = appapi.Failed
				operation.Resources[1].State = "succeeded"
			}
			if entry == "app failed" {
				operation.Status, operation.Resources[1].State = appapi.Failed, "failed"
			}
			if entry == "other space" {
				plan.Space.ID = "other-space"
			}
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch request.URL.Path {
				case "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}}
				case "/api/v1/organizations/team/spaces":
					output = []appapi.Space{space}
				case "/api/v1/organizations/team/clusters":
					output = []appapi.Cluster{}
				case "/api/v1/organizations/team/spaces/dev/apps":
					output = appapi.ApplicationState{Configuration: configuration}
				case "/api/v1/operations/progress":
					output = operation
				case "/api/v1/operations/progress/cancel":
					assert.Equal(request.Method, http.MethodPost)
					operation.Status = appapi.Canceling
					output = operation
				case "/api/v1/plans/plan":
					output = plan
				case "/api/v1/organizations/team/spaces/dev/apps/web/deployments":
					output = []appapi.Deployment{{Operation: operation}}
				default:
					t.Errorf("unexpected progress request: %s", request.URL.Path)
				}
				assert.NoErr(json.NewEncoder(writer).Encode(output))
			}))
			assert.NoErr(err)
			path, method := "/api/ui/operations/progress?organization=team&space=dev&app=web", http.MethodGet
			switch entry {
			case "initial page":
				path = "/api/ui/page?view=application&organization=team&space=dev&app=web&operation=progress"
			case "cancel":
				path, method = "/api/ui/operations/progress/cancel?organization=team&space=dev&app=web", http.MethodPost
			case "history":
				path = "/api/ui/application?organization=team&space=dev&app=web&panel=deployments"
			case "space operation":
				path = "/api/ui/operations/progress"
			}
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if entry == "other space" {
				assert.Equal(response.Code, http.StatusNotFound)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			var selected appapi.Operation
			switch entry {
			case "initial page":
				selected = decodeView[pageData](t, response, "workspace").Operation
			case "history":
				selected = decodeView[applicationPanelData](t, response, "application-deployments").Deployments[0].Operation
			default:
				selected = decodeView[appapi.Operation](t, response, "ui-operation")
			}
			if entry == "space operation" {
				assert.Equal(selected, operation)
				return
			}
			assert.Equal(selected.Resources, operation.Resources[1:4])
			assert.Equal(selected.Status, operation.Status)
			assert.True(!strings.Contains(selected.Message, "unrelated"))
			if entry == "app ready" {
				assert.True(strings.Contains(selected.Message, "All resources for web are ready"))
				assert.True(strings.Contains(selected.Message, "failed"))
				return
			}
			assert.True(strings.Contains(selected.Message, "selected app is starting"))
		})
	}
}
