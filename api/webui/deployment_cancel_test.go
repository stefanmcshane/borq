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
)

// TestDeploymentCancellationControls owns visible cancel actions, the POST transport,
// and polling stopping for canceled and superseded deployments.
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
			terminal := status == appapi.Canceled || status == appapi.Superseded || status == appapi.Succeeded || status == appapi.Failed
			for _, path := range []string{"/ui/application?organization=acme&space=dev&app=web&panel=deployments", "/ui/operations/deployment-one"} {
				response := httptest.NewRecorder()
				web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				assert.Equal(response.Code, http.StatusOK)
				assert.Equal(strings.Contains(response.Body.String(), ">Cancel deployment</button>"), canCancel)
				if path == "/ui/operations/deployment-one" {
					assert.Equal(strings.Contains(response.Body.String(), "every 2s"), !terminal)
				}
			}
			if !canCancel {
				return
			}
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ui/operations/deployment-one/cancel", nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(cancels, 1)
			assert.True(strings.Contains(response.Body.String(), "canceling"))
			assert.True(!strings.Contains(response.Body.String(), ">Cancel deployment</button>"))
			current = appapi.Canceled
			response = httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/operations/deployment-one", nil))
			assert.Equal(response.Header().Get("HX-Trigger"), "deployment-complete")
			assert.True(!strings.Contains(response.Body.String(), "every 2s"))
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
					t.Errorf("unexpected cancellation request: %s", request.URL.Path)
				}
			}))
			assert.NoErr(err)
			response := postForm(web, url.Values{"action": {"cancel-deployment"}, "organization": {"acme"}, "space": {"dev"}, "application": {"web"}, "deployment": {"deployment-one"}}, false)
			if !sameSpace {
				assert.Equal(response.Code, http.StatusConflict)
				assert.Equal(cancels, 0)
				return
			}
			assert.Equal(response.Code, http.StatusSeeOther)
			assert.Equal(cancels, 1)
			location, err := url.Parse(response.Header().Get("Location"))
			assert.NoErr(err)
			assert.Equal(location.Query().Get("view"), "application")
			assert.Equal(location.Query().Get("app"), "web")
			assert.Equal(location.Query().Get("tab"), "deployments")
		})
	}
}
