package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"golang.org/x/net/html"
)

// TestManagementShellAndRefresh exercises the browser contract: a complete initial
// document, reachable bundled assets, and a fragment that refreshes the workspace.
func TestManagementShellAndRefresh(t *testing.T) {
	assert := is.New(t)
	handler := newTestHandler(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	response, err := server.Client().Get(server.URL + "/")
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.StatusCode, http.StatusOK)
	assert.Equal(response.Header.Get("Content-Type"), "text/html; charset=utf-8")
	assert.Equal(response.Header.Get("Cache-Control"), "no-store")
	assert.Equal(response.Header.Get("Vary"), "HX-Request")
	document, err := html.Parse(response.Body)
	assert.NoErr(err)
	assets := map[string]bool{}
	refreshPath, refreshTarget := "", ""
	var inspect func(*html.Node)
	inspect = func(node *html.Node) {
		refresh := false
		for _, attribute := range node.Attr {
			if attribute.Key == "id" && attribute.Val == "refresh-workspace" {
				refresh = true
			}
		}
		for _, attribute := range node.Attr {
			if (attribute.Key == "src" || attribute.Key == "href") && strings.HasPrefix(attribute.Val, "/assets/") {
				assets[attribute.Val] = true
			}
			if refresh && attribute.Key == "hx-get" {
				refreshPath = attribute.Val
			}
			if node.Data == "body" && attribute.Key == "hx-target" {
				refreshTarget = attribute.Val
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			inspect(child)
		}
	}
	inspect(document)
	assert.True(assets["/assets/htmx.min.js"])
	assert.Equal(refreshTarget, "#app")
	assert.True(refreshPath != "")
	for path := range assets {
		asset, err := server.Client().Get(server.URL + path)
		assert.NoErr(err)
		content, err := io.ReadAll(asset.Body)
		assert.NoErr(err)
		assert.NoErr(asset.Body.Close())
		assert.Equal(asset.StatusCode, http.StatusOK)
		assert.True(len(content) > 0)
		assert.True(!strings.HasPrefix(asset.Header.Get("Content-Type"), "text/html"))
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+refreshPath, nil)
	assert.NoErr(err)
	request.Header.Set("HX-Request", "true")
	fragment, err := server.Client().Do(request)
	assert.NoErr(err)
	defer fragment.Body.Close()
	body, err := io.ReadAll(fragment.Body)
	assert.NoErr(err)
	assert.Equal(fragment.StatusCode, http.StatusOK)
	assert.Equal(fragment.Header.Get("Vary"), "HX-Request")
	assert.True(strings.Contains(string(body), "<h1>Overview</h1>"))
	assert.True(!strings.Contains(string(body), "<html"))
	assert.True(!strings.Contains(string(body), "<script"))
}

// TestShellRouteBoundaries keeps the blank shell read-only, supports browser HEAD
// requests, and prevents unknown paths or absent assets from becoming successful pages.
func TestShellRouteBoundaries(t *testing.T) {
	handler := newTestHandler(t)
	for _, input := range []struct {
		method, path string
		status       int
	}{
		{http.MethodHead, "/", http.StatusOK},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodGet, "/ui/cluster-creations?organization=team", http.StatusNotFound},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound},
		{http.MethodGet, "/v1/organizations", http.StatusNotFound},
		{http.MethodGet, "/api/v1/organizations", http.StatusNotFound},
		{http.MethodPost, "/assets/style.css", http.StatusMethodNotAllowed},
	} {
		t.Run(input.method+" "+input.path, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(input.method, input.path, nil))
			assert.Equal(response.Code, input.status)
			if input.method == http.MethodHead {
				assert.Equal(response.Body.Len(), 0)
			}
		})
	}
}

// TestManagementSectionKeepsItsOwnScope proves management is available without
// an organization and that workspace selectors cannot redirect management actions.
func TestManagementSectionKeepsItsOwnScope(t *testing.T) {
	assert := is.New(t)
	var requests []string
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ready","message":"Management is ready","result":{"components":[]}}`))
	}))
	assert.NoErr(err)
	page := httptest.NewRecorder()
	web.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?view=management&organization=missing&space=missing&cluster=target", nil))
	assert.Equal(page.Code, http.StatusOK)
	assert.True(strings.Contains(page.Body.String(), "<h1>Management</h1>"))
	assert.True(strings.Contains(page.Body.String(), `/ui/management/components`))
	assert.True(strings.Contains(page.Body.String(), `action="/ui/management/actions"`))
	assert.True(!strings.Contains(page.Body.String(), `id="space-selector"`))
	assert.Equal(len(requests), 0)
	components := httptest.NewRecorder()
	web.ServeHTTP(components, httptest.NewRequest(http.MethodGet, "/ui/management/components?cluster=target", nil))
	assert.Equal(components.Code, http.StatusOK)
	assert.True(strings.Contains(components.Body.String(), "Management is ready"))
	assert.True(strings.Contains(components.Body.String(), `hx-get="/ui/management/components"`))
	request := httptest.NewRequest(http.MethodPost, "/ui/management/actions", strings.NewReader("action=upgrade-management&view=clusters&organization=missing&space=missing&cluster=target"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	upgraded := httptest.NewRecorder()
	web.ServeHTTP(upgraded, request)
	assert.Equal(upgraded.Code, http.StatusOK)
	assert.Equal(upgraded.Header().Get("HX-Push-Url"), "/?view=management")
	assert.True(strings.Contains(upgraded.Body.String(), "Management components upgraded."))
	assert.Equal(requests, []string{"GET /api/v1/management/components", "POST /api/v1/management/components/upgrade"})
	legacyAction := postForm(web, map[string][]string{"action": {"upgrade-management"}}, true)
	assert.Equal(legacyAction.Code, http.StatusBadRequest)
	request = httptest.NewRequest(http.MethodPost, "/ui/management/actions", strings.NewReader("action=register-cluster&organization=team&name=target"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	invalidAction := httptest.NewRecorder()
	web.ServeHTTP(invalidAction, request)
	assert.Equal(invalidAction.Code, http.StatusBadRequest)
	unscoped := httptest.NewRecorder()
	web.ServeHTTP(unscoped, httptest.NewRequest(http.MethodGet, "/ui/components", nil))
	assert.Equal(unscoped.Code, http.StatusBadRequest)
	assert.Equal(len(requests), 2)
}

// TestClusterUiIsIndependentOfManagementTopology protects the same deployment
// controls and linking flow for converged and separate management installations.
func TestClusterUiIsIndependentOfManagementTopology(t *testing.T) {
	assert := is.New(t)
	var pages []string
	for _, inCluster := range []bool{false, true} {
		web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/organizations":
				_, _ = writer.Write([]byte(`[{"name":"team"}]`))
			case "/api/v1/organizations/team/spaces":
				_, _ = writer.Write([]byte(`[]`))
			case "/api/v1/organizations/team/clusters":
				assert.NoErr(json.NewEncoder(writer).Encode([]map[string]any{{"name": "production", "organization": "team", "inCluster": inCluster}}))
			case "/api/v1/organizations/team/clusters/production/components":
				_, _ = writer.Write([]byte(`{"status":"ready","message":"Deployment components are ready","result":{"components":[]}}`))
			default:
				t.Errorf("unexpected API request: %s", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
		assert.NoErr(err)
		response := httptest.NewRecorder()
		web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=clusters&organization=team", nil))
		assert.Equal(response.Code, http.StatusOK)
		pages = append(pages, response.Body.String())
		for _, absent := range []string{"Hosting cluster", `name="inCluster"`, "Management components", "upgrade-management", "/ui/management/components", "Create a cluster", `value="create-cluster"`} {
			assert.True(!strings.Contains(response.Body.String(), absent))
		}
		assert.True(strings.Contains(response.Body.String(), "Reconcile production"))
		assert.True(strings.Contains(response.Body.String(), "Download kubeconfig"))
		overview := httptest.NewRecorder()
		web.ServeHTTP(overview, httptest.NewRequest(http.MethodGet, "/?view=overview&organization=team", nil))
		assert.Equal(overview.Code, http.StatusOK)
		assert.True(!strings.Contains(overview.Body.String(), "/ui/management/components"))
		assert.True(!strings.Contains(overview.Body.String(), "upgrade-management"))
		components := httptest.NewRecorder()
		web.ServeHTTP(components, httptest.NewRequest(http.MethodGet, "/ui/components?organization=team&cluster=production", nil))
		assert.Equal(components.Code, http.StatusOK)
		assert.True(strings.Contains(components.Body.String(), "production components"))
		assert.True(strings.Contains(components.Body.String(), `hx-get="/ui/components?cluster=production&amp;organization=team"`))
	}
	assert.Equal(pages[0], pages[1])
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "ui.sqlite")})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	handler, err := NewHandler(api)
	assert.NoErr(err)
	return handler
}
