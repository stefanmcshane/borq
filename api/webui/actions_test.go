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

// TestClusterLinkRequiresSetup keeps browser registration mandatory even when
// old or modified forms omit the checkbox or submit a context override.
func TestClusterLinkRequiresSetup(t *testing.T) {
	for _, option := range []string{"", "off"} {
		t.Run("reconcile="+option, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPost)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters")
				var link appapi.LinkClusterInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&link))
				assert.Equal(link.Name, "target")
				input := link.Connection
				assert.True(input.Reconcile != nil && *input.Reconcile)
				assert.True(input.Context == nil)
				assert.True(input.Kubeconfig != nil)
				assert.Equal(*input.Kubeconfig, "config-for-selected-cluster")
				assert.NoErr(json.NewEncoder(writer).Encode(appapi.Cluster{Name: "target-abc12"}))
			}))
			assert.NoErr(err)
			form := url.Values{"action": {"register-cluster"}, "organization": {"team"}, "view": {"clusters"}, "name": {"target"}, "kubeconfig": {"config-for-selected-cluster"}, "context": {"ignored-context"}}
			if option != "" {
				form.Set("reconcile", option)
			}
			response := postForm(web, form, false)
			assert.Equal(response.Code, http.StatusSeeOther)
			assert.Equal(response.Header().Get("Location"), "/?cluster=target-abc12&organization=team&tab=metrics&view=cluster")
		})
	}
}

// TestOrganizationAndSpaceForms exercises real API-backed form writes and scoped
// navigation, including non-JavaScript redirects and htmx fragment responses.
func TestOrganizationAndSpaceForms(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	created := postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}, true)
	assert.Equal(created.Code, http.StatusOK)
	assert.True(strings.Contains(created.Body.String(), "Organization created."))
	assert.Equal(created.Header().Get("HX-Push-Url"), "/?organization=team&setup=cluster&view=overview")
	assert.True(strings.Contains(created.Body.String(), `id="cluster-link-dialog"`))
	assert.True(strings.Contains(created.Body.String(), `data-auto-open`))
	assert.True(strings.Contains(created.Body.String(), `name="organization" value="team"`))
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
	linkedUrl, err := url.Parse(linked.Header().Get("HX-Push-Url"))
	assert.NoErr(err)
	teamCluster := linkedUrl.Query().Get("cluster")
	assert.True(strings.HasPrefix(teamCluster, "local-"))
	assert.Equal(linkedUrl.Query().Get("view"), "spaces")
	assert.True(!strings.Contains(linked.Body.String(), `name="namespace"`))
	assert.True(!strings.Contains(linked.Body.String(), `name="issuer"`))
	assert.True(!strings.Contains(linked.Body.String(), "Leave unbound"))
	assert.True(!strings.Contains(linked.Body.String(), "<th>Namespace</th>"))
	assert.True(strings.Contains(linked.Body.String(), `name="cluster" required`))
	space := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "view": {"spaces"}, "name": {"staging"}, "cluster": {teamCluster}}, true)
	assert.Equal(space.Code, http.StatusOK)
	assert.Equal(space.Header().Get("HX-Push-Url"), "/?organization=team&view=spaces")
	assert.True(strings.Contains(space.Body.String(), "Space saved."))
	other := postForm(web, url.Values{"action": {"create-organization"}, "name": {"other"}}, false)
	assert.Equal(other.Code, http.StatusSeeOther)
	assert.Equal(other.Header().Get("Location"), "/?organization=other&setup=cluster&view=overview")
	onboarding := httptest.NewRecorder()
	web.ServeHTTP(onboarding, httptest.NewRequest(http.MethodGet, other.Header().Get("Location"), nil))
	assert.Equal(onboarding.Code, http.StatusOK)
	assert.True(strings.Contains(onboarding.Body.String(), "other is ready."))
	assert.True(strings.Contains(onboarding.Body.String(), `name="organization" value="other"`))

	for _, input := range []struct{ path, contains, absent string }{
		{"/?view=spaces&organization=team", "staging", ""},
		{"/?view=spaces&organization=other", "Link a cluster before creating a space", ">staging</a>"},
		{"/?view=applications&organization=team&space=staging", "No applications in this space yet.", "Bind this space"},
		{"/?view=clusters&organization=other", "Select a cluster to inspect", ""},
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
	completed := postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"other"}, "view": {"overview"}, "setup": {"cluster"}, "name": {"other-cluster"}, "kubeconfig": {kubeconfig}}, true)
	assert.Equal(completed.Code, http.StatusOK)
	assert.Equal(completed.Header().Get("HX-Push-Url"), "/?organization=other&view=overview")
	assert.True(strings.Contains(completed.Body.String(), "default"))
	assert.True(strings.Contains(completed.Body.String(), `href="/?organization=other&amp;view=spaces"`))
	assert.True(!strings.Contains(completed.Body.String(), `id="cluster-link-dialog"`))

	// One cluster can be linked to two organizations, while other now has two
	// links. Admin lists all links and keeps each credential download scoped.
	shared := postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"other"}, "space": {"stale-space"}, "view": {"clusters"}, "name": {"local"}, "kubeconfig": {kubeconfig}}, false)
	assert.Equal(shared.Code, http.StatusSeeOther)
	sharedUrl, err := url.Parse(shared.Header().Get("Location"))
	assert.NoErr(err)
	otherCluster := sharedUrl.Query().Get("cluster")
	assert.True(strings.HasPrefix(otherCluster, "local-"))
	assert.Equal(sharedUrl.Query().Get("view"), "cluster")
	selected := httptest.NewRecorder()
	web.ServeHTTP(selected, httptest.NewRequest(http.MethodGet, "/?view=cluster&cluster="+otherCluster+"&organization=other&tab=spaces", nil))
	assert.Equal(selected.Code, http.StatusOK)
	assert.True(strings.Contains(selected.Body.String(), "Deployment spaces attached through other."))
	assert.True(strings.Contains(selected.Body.String(), "default-"+otherCluster))
	global := httptest.NewRecorder()
	globalRequest := httptest.NewRequest(http.MethodGet, "/?view=clusters", nil)
	globalRequest.Header.Set("HX-Request", "true")
	web.ServeHTTP(global, globalRequest)
	assert.Equal(global.Code, http.StatusOK)
	assert.Equal(global.Header().Get("HX-Push-Url"), "/?view=clusters")
	for _, link := range []string{
		`href="/?cluster=` + teamCluster + `&amp;organization=team&amp;view=cluster"`,
		`href="/?cluster=` + otherCluster + `&amp;organization=other&amp;view=cluster"`,
		`href="/?cluster=other-cluster-`,
		`<select class="select" name="organization" required>`,
		`<option value="team"`,
		`<option value="other"`,
	} {
		assert.True(strings.Contains(global.Body.String(), link))
	}
	assert.True(strings.Contains(selected.Body.String(), `href="/ui/kubeconfig?organization=other&amp;cluster=`+otherCluster+`"`))
	assert.True(!strings.Contains(selected.Body.String(), `href="/ui/kubeconfig?organization=team&amp;cluster=`+otherCluster+`"`))
	ambiguous := httptest.NewRecorder()
	web.ServeHTTP(ambiguous, httptest.NewRequest(http.MethodGet, "/?view=clusters&cluster="+teamCluster, nil))
	assert.Equal(ambiguous.Code, http.StatusOK)
	assert.True(strings.Contains(ambiguous.Body.String(), teamCluster))
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
// and distinguishes uninitialized target components from a broken page.
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
	components, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters/target/components")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"No component reconciliation has been recorded."}`))
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	components.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/components?organization=team&cluster=target", nil))
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
