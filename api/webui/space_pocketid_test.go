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

// TestSpacePocketIdUi owns form conversion and enrollment credentials' response-only lifetime.
func TestSpacePocketIdUi(t *testing.T) {
	assert := is.New(t)
	var submitted appapi.PocketIDInput
	var apply string
	var deletion bool
	service := appapi.PocketID{ID: "identity-id", Name: "identity", State: "ready", Operation: "change"}
	base := "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid"
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{{ID: "dev-id", Name: "dev", Cluster: "local"}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case base:
			output = []appapi.PocketID{service}
		case base + "/identity":
			switch request.Method {
			case http.MethodPut:
				submitted = appapi.PocketIDInput{}
				assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
				apply = request.URL.Query().Get("apply")
			case http.MethodDelete:
				deletion = true
			}
			output = service
		case base + "/identity/enrollment":
			assert.Equal(request.Method, http.MethodPost)
			output = map[string]string{"url": "https://id.test/enroll/private-token"}
		case "/api/v1/pocketid-operations/change":
			output = appapi.Operation{ID: "change", SpaceID: "dev-id", Status: "succeeded"}
		default:
			t.Errorf("unexpected API request: %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	perform := func(form url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		return response
	}
	form := url.Values{"action": {"put-space-pocketid"}, "organization": {"team"}, "space": {"dev"}, "name": {" identity "}, "issuerUrl": {" https://id.test "}, "adminUsername": {" admin "}, "adminEmail": {" admin@test.example "}, "storage": {" 2Gi "}, "storageClass": {" local-path "}, "image": {" example/pocketid:v1 "}, "certificateIssuer": {" letsencrypt "}}
	response := perform(form)
	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(*submitted.IssuerURL, "https://id.test")
	assert.Equal(*submitted.AdminUsername, "admin")
	assert.Equal(*submitted.AdminEmail, "admin@test.example")
	assert.Equal(*submitted.Storage, "2Gi")
	assert.Equal(*submitted.StorageClass, "local-path")
	assert.Equal(*submitted.Image, "example/pocketid:v1")
	assert.Equal(*submitted.CertificateIssuer, "letsencrypt")
	assert.Equal(apply, "")
	assert.Equal(page.View, "space-pocketid")
	assert.Equal(page.SectionView, "postgres")
	assert.Equal(page.SpacePocketId.Selected.ID, "identity-id")
	for _, field := range []string{"storage", "storageClass", "image", "certificateIssuer"} {
		form.Del(field)
	}
	form.Set("apply", "true")
	response = perform(form)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(apply, "true")
	assert.Equal(submitted.Storage, (*string)(nil))
	assert.Equal(submitted.StorageClass, (*string)(nil))
	assert.Equal(submitted.Image, (*string)(nil))
	assert.Equal(submitted.CertificateIssuer, (*string)(nil))

	form.Set("action", "enroll-space-pocketid")
	response = perform(form)
	page = decodeView[pageData](t, response, "workspace")
	assert.Equal(page.SpacePocketIdEnrollment, "https://id.test/enroll/private-token")
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.True(!strings.Contains(page.Location, "private-token"))
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=space-pocketid&organization=team&space=dev&spacePocketId=identity&enrollmentUrl=private-token", nil))
	page = decodeView[pageData](t, response, "workspace")
	assert.Equal(page.SpacePocketIdEnrollment, "")
	assert.True(!strings.Contains(response.Body.String(), "private-token"))

	form.Set("action", "delete-space-pocketid")
	response = perform(form)
	page = decodeView[pageData](t, response, "workspace")
	assert.True(deletion)
	assert.Equal(page.SpacePocketId.Selected.ID, "")
	assert.Equal(page.SpacePocketId.Operation.ID, "change")
	assert.True(strings.Contains(page.Notice, "retained"))
}

// TestSpacePocketIdOperationScope prevents another space's operation from appearing in this view.
func TestSpacePocketIdOperationScope(t *testing.T) {
	for _, operationSpace := range []string{"dev-id", "other-id"} {
		t.Run(operationSpace, func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any = []appapi.PocketID{}
				if strings.HasPrefix(request.URL.Path, "/api/v1/pocketid-operations/") {
					output = appapi.Operation{ID: "change", SpaceID: operationSpace}
				}
				assert.NoErr(json.NewEncoder(writer).Encode(output))
			})}
			page := pageData{Organization: "team", Space: spaceSummary{ID: "dev-id", Name: "dev"}}
			page, err := web.loadSpacePocketIdPage(httptest.NewRequest(http.MethodGet, "/", nil), url.Values{"pocketIdOperation": {"change"}}, page)
			if operationSpace == "other-id" {
				assert.True(err != nil)
				assert.Equal(err.(apiProblem).status, http.StatusNotFound)
				return
			}
			assert.NoErr(err)
			assert.Equal(page.SpacePocketId.Operation.ID, "change")
		})
	}
}
