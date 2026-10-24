package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestOrganizationAndSpaceForms exercises real API-backed form writes and scoped
// navigation, including non-JavaScript redirects and htmx fragment responses.
func TestOrganizationAndSpaceForms(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	created := postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}, true)
	assert.Equal(created.Code, http.StatusOK)
	assert.True(strings.Contains(created.Body.String(), "Organization created."))
	assert.True(strings.Contains(created.Header().Get("HX-Push-Url"), "organization=team"))
	missingCluster := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "name": {"staging"}}, true)
	assert.Equal(missingCluster.Code, http.StatusBadRequest)
	cluster := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"gitVersion":"v1.35.3"}`))
	}))
	t.Cleanup(cluster.Close)
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: local\n  cluster:\n    server: " + cluster.URL + "\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: test\ncurrent-context: local\nusers:\n- name: test\n  user: {}\n"
	linked := postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"team"}, "view": {"spaces"}, "name": {"local"}, "kubeconfig": {kubeconfig}}, true)
	assert.Equal(linked.Code, http.StatusOK)
	assert.True(strings.Contains(linked.Header().Get("HX-Push-Url"), "space=local"))
	assert.True(!strings.Contains(linked.Body.String(), `name="namespace"`))
	assert.True(!strings.Contains(linked.Body.String(), `name="issuer"`))
	assert.True(!strings.Contains(linked.Body.String(), "Leave unbound"))
	assert.True(!strings.Contains(linked.Body.String(), "<th>Namespace</th>"))
	assert.True(strings.Contains(linked.Body.String(), `name="cluster" required`))
	space := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "view": {"spaces"}, "name": {"staging"}, "cluster": {"local"}}, true)
	assert.Equal(space.Code, http.StatusOK)
	assert.True(strings.Contains(space.Header().Get("HX-Push-Url"), "space=staging"))
	assert.True(strings.Contains(space.Body.String(), "Space saved."))
	other := postForm(web, url.Values{"action": {"create-organization"}, "name": {"other"}}, false)
	assert.Equal(other.Code, http.StatusSeeOther)
	assert.Equal(other.Header().Get("Location"), "/?organization=other&view=overview")
	for _, input := range []struct{ path, contains, absent string }{
		{"/?view=spaces&organization=team", "staging", ""},
		{"/?view=spaces&organization=other", "Link a cluster before creating a space", ">staging</a>"},
		{"/?view=applications&organization=team&space=staging", "Applications in staging", "Bind this space"},
		{"/?view=clusters&organization=other", "Link a cluster", "Download kubeconfig"},
		{"/?view=environment&organization=other", "Create a space to use environment groups", "Publish version"},
	} {
		response := httptest.NewRecorder()
		web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, input.path, nil))
		assert.Equal(response.Code, http.StatusOK)
		assert.True(strings.Contains(response.Body.String(), input.contains))
		if input.absent != "" {
			assert.True(!strings.Contains(response.Body.String(), input.absent))
		}
	}
}

// TestFormErrorsRemainVisibleAndDoNotEchoSecrets reaches YAML, schema, path and
// numeric validation at their real boundaries while preserving the existing page.
func TestFormErrorsRemainVisibleAndDoNotEchoSecrets(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	assert.Equal(postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}, true).Code, http.StatusOK)
	for _, input := range []struct {
		name    string
		form    url.Values
		message string
	}{
		{"identifier", url.Values{"action": {"create-organization"}, "name": {"INVALID"}}, "invalid request path"},
		{"duplicate YAML", url.Values{"action": {"plan-app"}, "format": {"yaml"}, "configuration": {"name: web\nname: worker"}}, "unique keys"},
		{"multiple documents", url.Values{"action": {"plan-app"}, "format": {"yaml"}, "configuration": {"name: web\n---\nname: worker"}}, "one YAML document"},
		{"schema", url.Values{"action": {"plan-app"}, "format": {"yaml"}, "configuration": {"name: web\nprivateKey: do-not-echo-secret"}}, "OpenAPI schema"},
		{"bulk YAML wrapper", url.Values{"action": {"plan-app"}, "format": {"yaml"}, "configuration": {"apps: [{name: web}, {name: worker}]"}}, "OpenAPI schema"},
		{"application identity", url.Values{"action": {"plan-app"}, "format": {"yaml"}, "application": {"web"}, "configuration": {"name: worker\nimage: nginx"}}, "Keep this application"},
		{"number", url.Values{"action": {"plan-app"}, "port": {"invalid"}, "replicas": {"1"}}, "Port must be"},
		{"action", url.Values{"action": {"unsupported"}}, "supported workspace action"},
		{"removed cluster creation", url.Values{"action": {"create-cluster"}, "name": {"target"}}, "supported workspace action"},
		{"credentials", url.Values{"action": {"register-cluster"}, "name": {"target"}, "kubeconfig": {"do-not-echo-secret"}}, "request configuration is invalid"},
		{"target infrastructure disabled", url.Values{"action": {"reconcile-cluster"}, "cluster": {"local"}}, "request configuration is invalid"},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			input.form.Set("organization", "team")
			input.form.Set("space", "default")
			response := postForm(web, input.form, true)
			assert.Equal(response.Code, http.StatusBadRequest)
			assert.Equal(response.Header().Get("HX-Retarget"), "#notice")
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
}

// TestMissingSelectionsAndComponentDiagnostics keeps invalid navigation visible
// and distinguishes an uninitialized management installation from a broken page.
func TestMissingSelectionsAndComponentDiagnostics(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	assert.Equal(postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}, true).Code, http.StatusOK)
	for _, path := range []string{"/?view=missing", "/?organization=team&space=missing", "/?view=applications&organization=team&app=missing", "/ui/operations/missing", "/ui/kubeconfig?organization=team&cluster=missing"} {
		response := httptest.NewRecorder()
		web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(response.Code, http.StatusNotFound)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	}
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/management/components", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "No component reconciliation has been recorded."))
}

// TestCrossOriginFormsAreRejected proves a valid mutation reaches the origin
// guard and is rejected before an organization can be created.
func TestCrossOriginFormsAreRejected(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "http://borq.test/ui/actions", strings.NewReader(url.Values{"action": {"create-organization"}, "name": {"cross-site"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://untrusted.test")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusForbidden)
	page := httptest.NewRecorder()
	web.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.True(strings.Contains(page.Body.String(), "Create your first organization"))
}

// TestReadinessErrorsReachTheBrowser protects retry guidance across the HTML
// boundary without bypassing or reimplementing the API's readiness decision.
func TestReadinessErrorsReachTheBrowser(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "15")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"message":"Management components are starting."}`))
	}))
	assert.NoErr(err)
	response := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "name": {"staging"}}, true)
	assert.Equal(response.Code, http.StatusServiceUnavailable)
	assert.Equal(response.Header().Get("Retry-After"), "15")
	assert.True(strings.Contains(response.Body.String(), "Management components are starting."))
}

func postForm(handler http.Handler, form url.Values, htmx bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		request.Header.Set("HX-Request", "true")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
