package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestDeploymentCancellationControls preserves deployment status and cancellation
// through the JSON operation and application-history transports.
func TestDeploymentCancellationControls(t *testing.T) {
	for _, status := range []appapi.OperationStatus{appapi.Queued, appapi.Pending, appapi.Applying, appapi.Waiting, appapi.Canceling, appapi.Canceled, appapi.Superseded, appapi.Succeeded, appapi.Failed} {
		t.Run(string(status), func(t *testing.T) {
			assert := is.New(t)
			current := status
			cancels := 0
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				operation := appapi.Operation{ID: "deployment-one", Status: current, Message: "Deployment " + string(current)}
				switch request.URL.Path {
				case "/api/v1/organizations/acme/spaces/dev/apps/web/deployments":
					assert.NoErr(json.NewEncoder(writer).Encode([]appapi.Deployment{{Operation: operation}}))
				case "/api/v1/operations/deployment-one":
					assert.NoErr(json.NewEncoder(writer).Encode(operation))
				case "/api/v1/operations/deployment-one/cancel":
					assert.Equal(request.Method, http.MethodPost)
					cancels++
					current, operation.Status = appapi.Canceling, appapi.Canceling
					assert.NoErr(json.NewEncoder(writer).Encode(operation))
				default:
					t.Errorf("unexpected application API request: %s", request.URL.Path)
				}
			}))
			assert.NoErr(err)
			canCancel := status == appapi.Queued || status == appapi.Pending || status == appapi.Applying || status == appapi.Waiting
			for _, path := range []string{"/api/ui/application?organization=acme&space=dev&app=web&panel=deployments", "/api/ui/operations/deployment-one"} {
				response := httptest.NewRecorder()
				web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				assert.Equal(response.Code, http.StatusOK)
				if path == "/api/ui/operations/deployment-one" {
					operation := decodeView[appapi.Operation](t, response, "ui-operation")
					assert.Equal(operation.Status, status)
					assert.Equal(operation.ID, "deployment-one")
				} else {
					data := decodeView[applicationPanelData](t, response, "application-deployments")
					assert.Equal(data.Deployments[0].Operation.Status, status)
				}
			}
			if !canCancel {
				return
			}
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ui/operations/deployment-one/cancel", nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(cancels, 1)
			assert.Equal(decodeView[appapi.Operation](t, response, "ui-operation").Status, appapi.Canceling)
			current = appapi.Canceled
			response = httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/operations/deployment-one", nil))
			assert.Equal(decodeView[appapi.Operation](t, response, "ui-operation").Status, appapi.Canceled)
		})
	}
}

// TestCancelDeploymentHistoryScope prevents a stale or tampered history form from
// canceling a deployment in another space and retains application navigation.
func TestCancelDeploymentHistoryScope(t *testing.T) {
	for _, sameSpace := range []bool{true, false} {
		t.Run(map[bool]string{true: "selected space", false: "other space"}[sameSpace], func(t *testing.T) {
			assert := is.New(t)
			cancels := 0
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/operations/deployment-one":
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.Operation{ID: "deployment-one", SpaceID: "dev", Status: appapi.Waiting}))
				case "/api/v1/organizations/acme/spaces":
					assert.NoErr(json.NewEncoder(writer).Encode([]appapi.Space{{ID: "dev", Name: "dev", Cluster: "target-abc12"}}))
				case "/api/v1/organizations/acme/spaces/dev":
					id := "other"
					if sameSpace {
						id = "dev"
					}
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.Space{ID: id}))
				case "/api/v1/operations/deployment-one/cancel":
					assert.Equal(request.Method, http.MethodPost)
					cancels++
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.Operation{ID: "deployment-one", Status: appapi.Canceling}))
				default:
					if serveActionPageFixture(writer, request) {
						return
					}
					t.Errorf("unexpected cancellation request: %s", request.URL.Path)
				}
			}))
			assert.NoErr(err)
			response := postForm(web, url.Values{"action": {"cancel-deployment"}, "organization": {"acme"}, "space": {"dev"}, "application": {"web"}, "deployment": {"deployment-one"}})
			if !sameSpace {
				assert.Equal(response.Code, http.StatusConflict)
				assert.Equal(cancels, 0)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(cancels, 1)
			location, err := url.Parse(decodeView[pageData](t, response, "workspace").Location)
			assert.NoErr(err)
			assert.Equal(location.Query().Get("view"), "application")
			assert.Equal(location.Query().Get("app"), "web")
			assert.Equal(location.Query().Get("tab"), "deployments")
		})
	}
}
