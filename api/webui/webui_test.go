package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"golang.org/x/net/html"
)

// TestManagementShellAndRefresh proves the embedded static document and all its
// bundled assets load independently of the dynamic workspace API.
func TestManagementShellAndRefresh(t *testing.T) {
	assert := is.New(t)
	calls := 0
	handler, err := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode([]appapi.Organization{})
	}))
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	var initial []byte
	for _, query := range []string{"", "?organization=private&space=secret&view=application"} {
		response, err := server.Client().Get(server.URL + "/" + query)
		assert.NoErr(err)
		assert.Equal(response.StatusCode, http.StatusOK)
		assert.True(strings.HasPrefix(response.Header.Get("Content-Type"), "text/html"))
		body, err := io.ReadAll(response.Body)
		assert.NoErr(err)
		assert.NoErr(response.Body.Close())
		if initial == nil {
			initial = body
		} else {
			assert.Equal(string(body), string(initial))
		}
	}
	assert.Equal(calls, 0)
	document, err := html.Parse(strings.NewReader(string(initial)))
	assert.NoErr(err)
	assets := map[string]bool{}
	for node := range document.Descendants() {
		for _, attribute := range node.Attr {
			if (attribute.Key == "src" || attribute.Key == "href") && (strings.HasPrefix(attribute.Val, "/_astro/") || strings.HasPrefix(attribute.Val, "/assets/")) {
				assets[attribute.Val] = true
			}
		}
	}
	assert.True(len(assets) > 0)
	for path := range assets {
		response, err := server.Client().Get(server.URL + path)
		assert.NoErr(err)
		body, err := io.ReadAll(response.Body)
		assert.NoErr(err)
		assert.NoErr(response.Body.Close())
		assert.Equal(response.StatusCode, http.StatusOK)
		assert.True(len(body) > 0)
		assert.True(!strings.HasPrefix(response.Header.Get("Content-Type"), "text/html"))
		if strings.HasPrefix(path, "/_astro/") {
			assert.Equal(response.Header.Get("Cache-Control"), "public, max-age=31536000, immutable")
		}
	}
	page := requestPage(t, handler, "", http.StatusOK)
	assert.Equal(page.Title, "Overview")
	assert.Equal(page.View, "overview")
	assert.Equal(page.Location, "/?view=overview")
	assert.Equal(calls, 1)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/ui/page", nil))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/ui/page", nil))
	assert.Equal(first.Header().Get("X-Borq-Ui-Version"), second.Header().Get("X-Borq-Ui-Version"))
}

// TestShellRouteBoundaries keeps assets explicit and the static shell read-only.
func TestShellRouteBoundaries(t *testing.T) {
	handler := newTestHandler(t)
	for _, input := range []struct {
		method, path string
		status       int
	}{
		{http.MethodHead, "/", http.StatusOK}, {http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", http.StatusNotFound}, {http.MethodGet, "/ui/actions", http.StatusNotFound},
		{http.MethodGet, "/api/ui/cluster-creations?organization=team", http.StatusNotFound},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound}, {http.MethodGet, "/_astro/missing.js", http.StatusNotFound},
		{http.MethodGet, "/assets/", http.StatusNotFound}, {http.MethodGet, "/_astro/", http.StatusNotFound},
		{http.MethodGet, "/api/v1/organizations", http.StatusNotFound}, {http.MethodPost, "/assets/style.css", http.StatusMethodNotAllowed},
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

// TestNavigationScopes keeps global administration outside stale organization
// and space selections and reports canonical browser locations in JSON.
func TestNavigationScopes(t *testing.T) {
	web := applicationTestHandler(t)
	for _, input := range []struct{ query, view, organization, space, location string }{
		{"view=spaces&organization=team&space=missing", "spaces", "team", "", "/?organization=team&view=spaces"},
		{"view=clusters&organization=team&space=missing", "infrastructure", "", "", "/?componentScope=management&node=management&tab=clusters&view=infrastructure"},
		{"view=applications&organization=team&space=dev", "applications", "team", "dev", "/?organization=team&space=dev&view=applications"},
	} {
		t.Run(input.view, func(t *testing.T) {
			assert := is.New(t)
			page := requestPage(t, web, input.query, http.StatusOK)
			assert.Equal(page.View, input.view)
			assert.Equal(page.Organization, input.organization)
			assert.Equal(page.Space.Name, input.space)
			assert.Equal(page.Location, input.location)
		})
	}
}

// TestManagementNavigationMovesToClusters preserves renamed bookmarks and rejects
// removed unscoped mutations without invoking the protected management API.
func TestManagementNavigationMovesToClusters(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	page := requestPage(t, web, "view=management&organization=missing&space=missing&cluster=target", http.StatusOK)
	assert.Equal(page.View, "infrastructure")
	assert.Equal(page.Location, "/?componentScope=management&node=management&tab=clusters&view=infrastructure")
	for _, input := range []struct{ method, path string }{{http.MethodGet, "/api/ui/management/components"}, {http.MethodPost, "/api/ui/management/actions"}} {
		response := httptest.NewRecorder()
		web.ServeHTTP(response, httptest.NewRequest(input.method, input.path, nil))
		if input.method == http.MethodPost {
			assert.Equal(response.Code, http.StatusMethodNotAllowed)
		} else {
			assert.Equal(response.Code, http.StatusNotFound)
		}
	}
	response := postForm(web, url.Values{"action": {"upgrade-management"}})
	assert.Equal(response.Code, http.StatusBadRequest)
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/components", nil))
	assert.Equal(response.Code, http.StatusBadRequest)
}

// decodeView validates the JSON transport before decoding a component's public data.
func decodeView[T any](t *testing.T, response *httptest.ResponseRecorder, component string) T {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s returned %d: %s", component, response.Code, response.Body.String())
	}
	assert := is.New(t)
	assert.True(strings.HasPrefix(response.Header().Get("Content-Type"), "application/json"))
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.True(response.Header().Get("X-Borq-Ui-Version") != "")
	var envelope struct {
		Component string `json:"component"`
		Data      T      `json:"data"`
	}
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &envelope))
	assert.Equal(envelope.Component, component)
	return envelope.Data
}

func requestPage(t *testing.T, web http.Handler, query string, status int) pageData {
	t.Helper()
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?"+query, nil))
	if response.Code != status {
		t.Fatalf("page %s returned %d, want %d: %s", query, response.Code, status, response.Body.String())
	}
	if status != http.StatusOK {
		return pageData{}
	}
	return decodeView[pageData](t, response, "workspace")
}

func newTestHandler(t *testing.T, organizations ...string) http.Handler {
	t.Helper()
	assert := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "ui.sqlite")})
	assert.NoErr(err)
	for _, name := range organizations {
		_, err := manager.CreateOrganization(t.Context(), name)
		assert.NoErr(err)
	}
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	handler, err := NewHandler(api)
	assert.NoErr(err)
	return handler
}
