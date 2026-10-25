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

// TestComponentCatalogView renders desired metadata and YAML separately from the saved override editor in the chosen target context.
func TestComponentCatalogView(t *testing.T) {
	assert := is.New(t)
	raw := map[string]any{"savedOverride": "only-this-is-editable"}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "first"}, {Name: "selected"}}
		case "/api/v1/organizations/first/clusters":
			output = []appapi.Cluster{{Name: "default"}}
		case "/api/v1/organizations/selected/clusters":
			output = []appapi.Cluster{{Name: "remote"}}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{}
		case "/api/v1/management/component-catalog":
			assert.Equal(request.URL.Query().Get("scope"), "target")
			assert.Equal(request.URL.Query().Get("organization"), "selected")
			assert.Equal(request.URL.Query().Get("cluster"), "remote")
			output = appapi.ComponentCatalog{Scope: "target", Namespace: "custom-namespace", Profile: "production", Components: []appapi.ComponentConfiguration{
				{Name: "cert-manager", Chart: "cert-manager", Version: "1.0.0", Values: map[string]any{"enabled": true}},
				{Name: "alloy", Description: "Collects telemetry.", Repository: "https://charts.example", Chart: "collector", Version: "2.3.4", Values: map[string]any{"borqDefault": "rendered-for-selected-remote", "savedOverride": "only-this-is-editable"}, Override: appapi.ComponentOverride{Values: &raw}},
			}}
		case "/api/v1/management/operations/apply":
			output = appapi.Operation{ID: "apply", Status: "running"}
		default:
			t.Fatalf("unexpected API request %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(output)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=settings&componentScope=target&componentOrganization=selected&componentCluster=remote&component=alloy&managementOperation=apply", nil))
	assert.Equal(response.Code, http.StatusOK)
	body := response.Body.String()
	document, err := html.Parse(strings.NewReader(body))
	assert.NoErr(err)
	var effective, editor, nextComponent string
	var customizers, selectedRows int
	for node := range document.Descendants() {
		attributes := map[string]string{}
		for _, attribute := range node.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		if node.Data == "a" && strings.HasSuffix(attributes["href"], "#component-catalog") {
			location, err := url.Parse(attributes["hx-get"])
			assert.NoErr(err)
			assert.Equal(location.Query().Get("managementOperation"), "apply")
			assert.Equal(attributes["hx-target"], "#workspace")
			assert.Equal(attributes["hx-select"], "")
			if location.Query().Get("component") == "cert-manager" {
				nextComponent = attributes["hx-get"]
			}
		}
		if node.Data == "option" && strings.Contains(attributes["value"], "componentScope=") {
			location, err := url.Parse(attributes["value"])
			assert.NoErr(err)
			assert.Equal(location.Query().Get("managementOperation"), "apply")
		}
		if attributes["id"] == "component-effective-values" {
			effective = navigationText(node)
		}
		if node.Data == "textarea" && attributes["name"] == "values" {
			editor = navigationText(node)
		}
		if node.Data == "details" && attributes["class"] == "component-customize" {
			customizers++
			_, open := attributes["open"]
			assert.True(!open)
		}
		if node.Data == "tr" && attributes["data-selected"] == "true" {
			selectedRows++
			assert.True(strings.Contains(navigationText(node), "alloy"))
		}
		if node.Data == "section" && attributes["hx-trigger"] == "every 3s" {
			location, err := url.Parse(attributes["hx-get"])
			assert.NoErr(err)
			assert.Equal(location.Query().Get("componentOrganization"), "selected")
			assert.Equal(location.Query().Get("componentCluster"), "remote")
			assert.Equal(location.Query().Get("component"), "alloy")
		}
	}
	assert.Equal(customizers, 1)
	assert.Equal(selectedRows, 1)
	assert.True(strings.Contains(effective, "borqDefault: rendered-for-selected-remote"))
	assert.True(strings.Contains(effective, "savedOverride: only-this-is-editable"))
	assert.Equal(editor, "savedOverride: only-this-is-editable")
	for _, value := range []string{"https://charts.example", "collector", "2.3.4", "custom-namespace", "production", "Your values overrides", "Blank chart fields keep current settings."} {
		if !strings.Contains(body, value) {
			t.Fatalf("rendered component is missing %q", value)
		}
	}
	assert.True(nextComponent != "")
	request := httptest.NewRequest(http.MethodGet, nextComponent, nil)
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	location, err := url.Parse(response.Header().Get("HX-Push-Url"))
	assert.NoErr(err)
	assert.Equal(location.Query().Get("component"), "cert-manager")
	assert.Equal(location.Query().Get("managementOperation"), "apply")
	document, err = html.Parse(response.Body)
	assert.NoErr(err)
	var refreshLocations int
	for node := range document.Descendants() {
		attributes := map[string]string{}
		for _, attribute := range node.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		if attributes["id"] == "workspace" || attributes["hx-trigger"] == "every 3s" {
			refreshLocations++
			location, err := url.Parse(attributes["hx-get"])
			assert.NoErr(err)
			assert.Equal(location.Query().Get("component"), "cert-manager")
			assert.Equal(location.Query().Get("managementOperation"), "apply")
			assert.Equal(location.Query().Get("componentOrganization"), "selected")
			assert.Equal(location.Query().Get("componentCluster"), "remote")
		}
	}
	assert.Equal(refreshLocations, 2)
}

// TestComponentCatalogMutation preserves preview selection and sends only explicit edits, including the clear-values object.
func TestComponentCatalogMutation(t *testing.T) {
	for _, values := range []string{"", "{}", "custom: changed"} {
		t.Run(values, func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPut)
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				var body appapi.ManagementConfiguration
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				assert.Equal(len(*body.Components), 1)
				override := (*body.Components)["alloy"]
				assert.True(override.Repo == nil && override.Chart == nil && override.Version == nil)
				if values == "" {
					assert.True(override.Values == nil)
				} else if values == "{}" {
					assert.True(override.Values != nil)
					assert.Equal(len(*override.Values), 0)
				} else {
					assert.Equal(*override.Values, map[string]any{"custom": "changed"})
				}
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "accepted"})
			})}
			form := url.Values{"view": {"settings"}, "action": {"put-management-component"}, "componentScope": {"target"}, "componentOrganization": {"selected"}, "componentCluster": {"remote"}, "component": {"alloy"}, "values": {values}}
			request := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			assert.NoErr(request.ParseForm())
			result, err := web.execute(request)
			assert.NoErr(err)
			for _, key := range []string{"componentScope", "componentOrganization", "componentCluster", "component"} {
				assert.Equal(result.selection.Get(key), form.Get(key))
			}
			assert.Equal(result.selection.Get("organization"), "")
			assert.Equal(result.selection.Get("managementOperation"), "accepted")
		})
	}
}

// TestComponentCatalogNoTargets avoids inventing target runtime values before a deployment cluster is registered.
func TestComponentCatalogNoTargets(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{}
		default:
			t.Fatalf("unexpected request %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(output)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=settings&componentScope=target", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "Register a deployment cluster under Clusters"))
	assert.True(!strings.Contains(response.Body.String(), `id="component-effective-values"`))
}
