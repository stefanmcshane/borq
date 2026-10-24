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
	version := response.Header.Get("X-Borq-Ui-Version")
	assert.True(version != "")
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
	assert.True(assets["/assets/app.js?v="+version])
	assert.True(assets["/assets/style.css?v="+version])
	assert.Equal(refreshTarget, "#workspace")
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
	request.Header.Set("X-Borq-Ui-Version", version)
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
	assert.True(!strings.Contains(string(body), `<aside`))
	assert.True(!strings.Contains(string(body), `<header class="topbar"`))
	request.Header.Set("X-Borq-Ui-Version", "previous-assets")
	stale, err := server.Client().Do(request)
	assert.NoErr(err)
	defer stale.Body.Close()
	assert.Equal(stale.StatusCode, http.StatusOK)
	assert.Equal(stale.Header.Get("HX-Redirect"), refreshPath)
}

// TestBreadcrumbNavigation follows scope and application links to keep overview
// scopes distinct and discard editor state when navigating between applications.
func TestBreadcrumbNavigation(t *testing.T) {
	assert := is.New(t)
	web := applicationTestHandler(t)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=application&organization=team&space=dev&app=planned&tab=configuration&subtab=yaml&draft=new-plan", nil))
	assert.Equal(response.Code, http.StatusOK)
	document, err := html.Parse(response.Body)
	assert.NoErr(err)
	var breadcrumb *html.Node
	for node := range document.Descendants() {
		for _, attribute := range node.Attr {
			if attribute.Key == "aria-label" && attribute.Val == "Breadcrumb" {
				breadcrumb = node
			}
		}
	}
	if breadcrumb == nil {
		t.Fatal("breadcrumb navigation is missing")
	}
	links := map[string]string{}
	var menus []string
	var applicationMenu *html.Node
	for node := range breadcrumb.Descendants() {
		for _, attribute := range node.Attr {
			if node.Data == "summary" && attribute.Key == "aria-label" {
				menus = append(menus, attribute.Val)
				if attribute.Val == "Switch application" {
					applicationMenu = node.Parent
				}
			}
			if node.Data != "a" || attribute.Key != "href" {
				continue
			}
			var label strings.Builder
			for child := range node.Descendants() {
				if child.Type == html.TextNode {
					label.WriteString(child.Data)
				}
			}
			links[strings.TrimSpace(label.String())] = attribute.Val
		}
	}
	assert.Equal(menus, []string{"Switch organization", "Switch space", "Switch application"})
	if applicationMenu == nil {
		t.Fatal("application breadcrumb menu is missing")
	}
	var applicationLabels []string
	for node := range applicationMenu.Descendants() {
		if node.Data != "a" {
			continue
		}
		var label strings.Builder
		for child := range node.Descendants() {
			if child.Type == html.TextNode {
				label.WriteString(child.Data)
			}
		}
		applicationLabels = append(applicationLabels, strings.TrimSpace(label.String()))
	}
	assert.Equal(applicationLabels, []string{"web", "worker", "Manage applications"})
	assert.Equal(links["Manage applications"], "/?organization=team&space=dev&view=applications")
	_, hasManageButton := links["Manage"]
	assert.True(!hasManageButton)
	assert.True(!strings.Contains(response.Body.String(), `id="space-selector"`))
	assert.True(!strings.Contains(response.Body.String(), `id="organization-selector"`))
	assert.True(!strings.Contains(response.Body.String(), "← All applications"))
	for _, input := range []struct{ label, path, content string }{
		{"team", "/?organization=team&view=overview", "Applications across all spaces"},
		{"dev", "/?organization=team&space=dev&view=overview", "View applications"},
		{"Manage applications", "/?organization=team&space=dev&view=applications", "Your applications"},
		{"web", "/?app=web&organization=team&space=dev&view=application", "http://web.borq-fixture-dev.svc:8080"},
		{"worker", "/?app=worker&organization=team&space=dev&view=application", "No network service"},
	} {
		t.Run(input.label, func(t *testing.T) {
			assert := is.New(t)
			assert.Equal(links[input.label], input.path)
			request := httptest.NewRequest(http.MethodGet, links[input.label], nil)
			request.Header.Set("HX-Request", "true")
			page := httptest.NewRecorder()
			web.ServeHTTP(page, request)
			assert.Equal(page.Code, http.StatusOK)
			assert.Equal(page.Header().Get("HX-Push-Url"), input.path)
			assert.True(strings.Contains(page.Body.String(), input.content))
		})
	}
}

// TestNavigationScopes keeps organization-wide spaces and global administration
// out of a selected space, including stale bookmarks and htmx navigation updates.
func TestNavigationScopes(t *testing.T) {
	web := applicationTestHandler(t)
	for _, input := range []struct {
		view, scope, location, breadcrumb string
		menus                             []string
	}{
		{"spaces", "&organization=team&space=missing", "/?organization=team&view=spaces", "Org › team › Spaces", []string{"Switch organization"}},
		{"clusters", "&organization=team&space=missing", "/?view=clusters", "Admin › Clusters", nil},
		{"applications", "&organization=team&space=dev", "/?organization=team&space=dev&view=applications", "Org › team › Space › dev › Applications", []string{"Switch organization", "Switch space", "Switch application"}},
	} {
		t.Run(input.view, func(t *testing.T) {
			for _, fragment := range []bool{false, true} {
				assert := is.New(t)
				request := httptest.NewRequest(http.MethodGet, "/?view="+input.view+input.scope, nil)
				if fragment {
					request.Header.Set("HX-Request", "true")
				}
				response := httptest.NewRecorder()
				web.ServeHTTP(response, request)
				assert.Equal(response.Code, http.StatusOK)
				if fragment {
					assert.Equal(response.Header().Get("HX-Push-Url"), input.location)
				}
				document, err := html.Parse(response.Body)
				assert.NoErr(err)
				var breadcrumb, refreshLocation string
				var menus []string
				for node := range document.Descendants() {
					for _, attribute := range node.Attr {
						if attribute.Key != "id" {
							continue
						}
						if attribute.Val == "refresh-workspace" {
							for _, attribute := range node.Attr {
								if attribute.Key == "hx-get" {
									refreshLocation = attribute.Val
								}
							}
						}
						if attribute.Val == "workspace-breadcrumb" {
							breadcrumb = navigationText(node)
							for child := range node.Descendants() {
								if child.Data != "summary" {
									continue
								}
								for _, attribute := range child.Attr {
									if attribute.Key == "aria-label" {
										menus = append(menus, attribute.Val)
									}
								}
							}
						}
						if attribute.Val == "sidebar-navigation" {
							group := ""
							for child := node.FirstChild; child != nil; child = child.NextSibling {
								if child.Data == "p" {
									group = navigationText(child)
								}
								if child.Data == "a" && navigationText(child) == "Clusters" {
									assert.Equal(group, "Admin")
									for _, attribute := range child.Attr {
										if attribute.Key == "href" || attribute.Key == "hx-get" {
											assert.Equal(attribute.Val, "/?view=clusters")
										}
									}
								}
							}
						}
					}
				}
				assert.Equal(breadcrumb, input.breadcrumb)
				assert.Equal(refreshLocation, input.location)
				assert.Equal(menus, input.menus)
			}
		})
	}
}

// navigationText reads the parser's linked DOM tree and omits closed details
// content so assertions describe the navigation visible before opening a menu.
func navigationText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var label strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if node.Data == "details" && child.Data != "summary" {
			continue
		}
		label.WriteString(navigationText(child))
		label.WriteByte(' ')
	}
	return strings.Join(strings.Fields(label.String()), " ")
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

// TestManagementNavigationMovesToClusters keeps old bookmarks useful while
// removing the standalone management UI and its unscoped mutation routes.
func TestManagementNavigationMovesToClusters(t *testing.T) {
	assert := is.New(t)
	var requests []string
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		assert.Equal(request.URL.Path, "/api/v1/organizations")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}))
	assert.NoErr(err)
	for _, fragment := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/?view=management&organization=missing&space=missing&cluster=target", nil)
		if fragment {
			request.Header.Set("HX-Request", "true")
		}
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
		if fragment {
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("HX-Redirect"), "/?view=clusters")
		} else {
			assert.Equal(response.Code, http.StatusSeeOther)
			assert.Equal(response.Header().Get("Location"), "/?view=clusters")
		}
	}
	assert.Equal(len(requests), 0)
	page := httptest.NewRecorder()
	web.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?view=clusters", nil))
	assert.Equal(page.Code, http.StatusOK)
	assert.True(strings.Contains(page.Body.String(), "Admin"))
	assert.True(strings.Contains(page.Body.String(), `href="/?view=clusters"`))
	assert.True(!strings.Contains(page.Body.String(), `href="/?view=management"`))
	for _, input := range []struct{ method, path string }{
		{http.MethodGet, "/ui/management/components"},
		{http.MethodPost, "/ui/management/actions"},
	} {
		request := httptest.NewRequest(input.method, input.path, strings.NewReader("action=upgrade-management"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		assert.Equal(response.Code, http.StatusNotFound)
	}
	legacyAction := postForm(web, map[string][]string{"action": {"upgrade-management"}}, true)
	assert.Equal(legacyAction.Code, http.StatusBadRequest)
	unscoped := httptest.NewRecorder()
	web.ServeHTTP(unscoped, httptest.NewRequest(http.MethodGet, "/ui/components", nil))
	assert.Equal(unscoped.Code, http.StatusBadRequest)
	assert.Equal(requests, []string{"GET /api/v1/organizations"})
}

// TestClusterUiIsIndependentOfManagementTopology protects the same deployment
// summaries and linking flow for converged and separate management installations.
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
				_ = json.NewEncoder(writer).Encode([]appapi.Space{
					{ID: "prod-a", Name: "prod-a", Cluster: "production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "web"}, {Name: "worker"}}}},
					{ID: "preview", Name: "preview", Cluster: "preview", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "test"}}}},
					{ID: "prod-b", Name: "prod-b", Cluster: "production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api"}}}},
				})
			case "/api/v1/organizations/team/clusters":
				assert.NoErr(json.NewEncoder(writer).Encode([]map[string]any{{"name": "production", "organization": "team", "inCluster": inCluster}, {"name": "preview", "organization": "team"}, {"name": "empty", "organization": "team"}}))
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
		for _, absent := range []string{"Hosting cluster", `name="inCluster"`, "Management components", "upgrade-management", "/ui/management/components", "Create a cluster", `value="create-cluster"`, "/ui/components?", "Reconcile production", "Upgrade components"} {
			assert.True(!strings.Contains(response.Body.String(), absent))
		}
		assert.True(!strings.Contains(response.Body.String(), "Download kubeconfig"))
		document, err := html.Parse(strings.NewReader(response.Body.String()))
		assert.NoErr(err)
		var rows [][]string
		for node := range document.Descendants() {
			if node.Data != "tr" {
				continue
			}
			var cells []string
			for cell := node.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Data != "td" && cell.Data != "th" {
					continue
				}
				var label strings.Builder
				for child := range cell.Descendants() {
					if child.Type == html.TextNode {
						label.WriteString(child.Data)
					}
				}
				cells = append(cells, strings.Join(strings.Fields(label.String()), " "))
			}
			rows = append(rows, cells)
		}
		assert.Equal(rows, [][]string{
			{"Cluster", "Organization", "Spaces", "Applications"},
			{"production", "team", "2", "3"},
			{"preview", "team", "1", "1"},
			{"empty", "team", "0", "0"},
		})
		empty := httptest.NewRecorder()
		web.ServeHTTP(empty, httptest.NewRequest(http.MethodGet, "/?view=clusters&organization=team&cluster=empty&tab=spaces", nil))
		assert.Equal(empty.Code, http.StatusOK)
		assert.True(strings.Contains(empty.Body.String(), "No spaces attached through this organization yet."))
		missing := httptest.NewRecorder()
		web.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/?view=clusters&organization=team&cluster=missing", nil))
		assert.Equal(missing.Code, http.StatusNotFound)
		assert.True(strings.Contains(missing.Body.String(), "selected cluster was not found"))
		overview := httptest.NewRecorder()
		web.ServeHTTP(overview, httptest.NewRequest(http.MethodGet, "/?view=overview&organization=team", nil))
		assert.Equal(overview.Code, http.StatusOK)
		assert.True(!strings.Contains(overview.Body.String(), "/ui/management/components"))
		assert.True(!strings.Contains(overview.Body.String(), "upgrade-management"))
		components := httptest.NewRecorder()
		web.ServeHTTP(components, httptest.NewRequest(http.MethodGet, "/ui/components?organization=team&cluster=production", nil))
		assert.Equal(components.Code, http.StatusOK)
		assert.True(strings.Contains(components.Body.String(), "<h2>On target cluster</h2>"))
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
