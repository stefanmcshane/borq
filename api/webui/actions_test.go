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
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestClusterLinkRequiresSetup keeps browser registration mandatory even when
// old or modified forms omit the checkbox or submit a context override.
func TestClusterLinkRequiresSetup(t *testing.T) {
	for _, option := range []string{"", "off"} {
		t.Run("reconcile="+option, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if serveActionPageFixture(writer, request) {
					return
				}
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
			response := postForm(web, form)
			page := decodeView[pageData](t, response, "workspace")
			assert.Equal(page.Location, "/?cluster=target-abc12&componentScope=target&node=application&organization=team&tab=metrics&view=infrastructure")
		})
	}
}

// TestOrganizationAndSpaceForms exercises real API-backed form writes and scoped
// navigation, including canonical workspace navigation after writes.
func TestOrganizationAndSpaceForms(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t, "team", "other")
	created := decodeView[pageData](t, postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}), "workspace")
	assert.True(strings.Contains(created.Notice, "Organization created."))
	assert.Equal(created.Location, "/?organization=team&setup=cluster&view=overview")
	assert.True(created.PromptCluster)
	missing := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "name": {"staging"}})
	assert.Equal(missing.Code, http.StatusBadRequest)
	cluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gitVersion":"v1.35.3"}`))
	}))
	t.Cleanup(cluster.Close)
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: local\n  cluster:\n    server: " + cluster.URL + "\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: test\ncurrent-context: local\nusers:\n- name: test\n  user: {}\n"
	linked := decodeView[pageData](t, postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"team"}, "view": {"spaces"}, "name": {"local"}, "kubeconfig": {kubeconfig}}), "workspace")
	location, err := url.Parse(linked.Location)
	assert.NoErr(err)
	teamCluster := location.Query().Get("cluster")
	assert.True(strings.HasPrefix(teamCluster, "local-"))
	assert.Equal(linked.View, "spaces")
	space := decodeView[pageData](t, postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "view": {"spaces"}, "name": {"staging"}, "cluster": {teamCluster}}), "workspace")
	assert.Equal(space.Location, "/?organization=team&view=spaces")
	assert.Equal(space.Notice, "Space saved. Its cluster binding is fixed.")
	found := false
	for _, saved := range space.Spaces {
		if saved.Name == "staging" {
			found = true
			assert.Equal(saved.Cluster, teamCluster)
		}
	}
	assert.True(found)
	other := decodeView[pageData](t, postForm(web, url.Values{"action": {"create-organization"}, "name": {"other"}}), "workspace")
	assert.Equal(other.Organization, "other")
	assert.True(other.PromptCluster)
	otherSpaces := requestPage(t, web, "view=spaces&organization=other", http.StatusOK)
	assert.Equal(len(otherSpaces.Spaces), 0)
	assert.Equal(len(otherSpaces.Clusters), 0)
	completed := decodeView[pageData](t, postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"other"}, "view": {"overview"}, "setup": {"cluster"}, "name": {"other-cluster"}, "kubeconfig": {kubeconfig}}), "workspace")
	assert.Equal(completed.Location, "/?organization=other&view=overview")
	assert.True(!completed.PromptCluster)
	assert.Equal(len(completed.Spaces), 1)
	shared := decodeView[pageData](t, postForm(web, url.Values{"action": {"register-cluster"}, "organization": {"other"}, "space": {"stale-space"}, "view": {"clusters"}, "name": {"local"}, "kubeconfig": {kubeconfig}}), "workspace")
	assert.Equal(shared.Organization, "other")
	assert.Equal(shared.View, "infrastructure")
	assert.True(strings.HasPrefix(shared.Cluster, "local-"))
	assert.Equal(shared.Space.ID, "")
	global := requestPage(t, web, "view=clusters", http.StatusOK)
	assert.Equal(global.Location, "/?componentScope=management&node=management&tab=clusters&view=infrastructure")
	assert.Equal(len(global.ClusterSummaries), 3)
	owners := map[string]int{}
	for _, summary := range global.ClusterSummaries {
		owners[summary.Organization]++
	}
	assert.Equal(owners, map[string]int{"team": 1, "other": 2})
	selected := requestPage(t, web, "view=cluster&organization=other&cluster="+shared.Cluster+"&tab=spaces", http.StatusOK)
	assert.Equal(selected.SelectedCluster.Organization, "other")
	assert.Equal(len(selected.SelectedCluster.Spaces), 1)
}

// TestFormErrorsRemainVisibleAndDoNotEchoSecrets reaches YAML, schema, path and
// numeric validation at their real boundaries while preserving the existing page.
func TestFormErrorsRemainVisibleAndDoNotEchoSecrets(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t, "team")
	assert.Equal(postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}).Code, http.StatusOK)
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
			response := postForm(web, input.form)
			assert.Equal(response.Code, http.StatusBadRequest)
			var problem appapi.Problem
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
			assert.True(problem.Message != "")
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
}

// TestMissingSelectionsAndComponentDiagnostics keeps invalid navigation visible
// and distinguishes uninitialized target components from a broken page.
func TestMissingSelectionsAndComponentDiagnostics(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t, "team")
	assert.Equal(postForm(web, url.Values{"action": {"create-organization"}, "name": {"team"}}).Code, http.StatusOK)
	for _, path := range []string{"/api/ui/page?view=missing", "/api/ui/page?organization=team&space=missing", "/api/ui/page?view=applications&organization=team&app=missing", "/api/ui/operations/missing", "/api/ui/kubeconfig?organization=team&cluster=missing"} {
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
	components.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/components?organization=team&cluster=target", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "No component reconciliation has been recorded."))
}

// TestCrossOriginFormsAreRejected proves a valid mutation reaches the origin
// guard and is rejected before an organization can be created.
func TestCrossOriginFormsAreRejected(t *testing.T) {
	assert := is.New(t)
	web := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "http://borq.test/api/ui/actions", strings.NewReader(url.Values{"action": {"create-organization"}, "name": {"cross-site"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://untrusted.test")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusForbidden)
	page := httptest.NewRecorder()
	web.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/api/ui/page", nil))
	assert.Equal(len(decodeView[pageData](t, page, "workspace").Organizations), 0)
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
	response := postForm(web, url.Values{"action": {"create-space"}, "organization": {"team"}, "name": {"staging"}})
	assert.Equal(response.Code, http.StatusServiceUnavailable)
	assert.Equal(response.Header().Get("Retry-After"), "15")
	assert.True(strings.Contains(response.Body.String(), "Management components are starting."))
}

func postForm(handler http.Handler, form url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// serveActionPageFixture supplies readback snapshots only for mutation fixtures
// that capture typed writes; real-manager tests own durable mutation readback.
func serveActionPageFixture(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	path := request.URL.Path
	var output any
	switch {
	case path == "/api/v1/organizations":
		output = []appapi.Organization{{Name: "team"}, {Name: "acme"}}
	case strings.HasSuffix(path, "/spaces"):
		output = []appapi.Space{{ID: "space-id", Name: "dev", Cluster: "target-abc12"}}
	case strings.HasSuffix(path, "/clusters"):
		output = []appapi.Cluster{{Name: "target-abc12"}}
	case strings.HasSuffix(path, "/route-policy"):
		output = appapi.ApplicationRoutePolicy{CertificateIssuer: "cluster-issuer", Domains: []string{"example.com"}}
	case strings.HasSuffix(path, "/apps"):
		output = appapi.ApplicationState{Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "web", Image: "new-image", Port: 8080, Replicas: 1}}}}
	case strings.HasSuffix(path, "/secrets"):
		output = []appapi.Secret{{Name: "web-config", Type: appapi.SecretTypeSecretGroup, Version: 4, Data: map[string]string{}}, {Name: "copy", Type: appapi.SecretTypeSecretGroup, Version: 1, Data: map[string]string{}}}
	case strings.Contains(path, "/secrets/"):
		output = appapi.Secret{Name: path[strings.LastIndex(path, "/")+1:], Type: appapi.SecretTypeSecretGroup, Version: 4, Data: map[string]string{}}
	case strings.HasSuffix(path, "/environment-groups"):
		output = []appapi.EnvironmentGroup{{Name: "web-config", Version: 4}, {Name: "copy", Version: 1}}
	case strings.Contains(path, "/environment-groups/"):
		output = appapi.EnvironmentGroup{Name: path[strings.LastIndex(path, "/")+1:], Version: 4}
	case strings.HasPrefix(path, "/api/v1/plans/"):
		output = appapi.Plan{ID: path[strings.LastIndex(path, "/")+1:], Space: appapi.Space{ID: "space-id"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "web", Image: "new-image"}}}}
	case strings.HasPrefix(path, "/api/v1/operations/"):
		output = appapi.Operation{ID: path[strings.LastIndex(path, "/")+1:], SpaceID: "space-id", Status: appapi.Canceling}
	default:
		return false
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(output)
	return true
}

// TestOrganizationOnboardingForm preserves provider selection and private credentials at the browser-to-API boundary.
func TestOrganizationOnboardingForm(t *testing.T) {
	for _, mode := range []string{"external", "new-pocketid", "none", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodGet && request.URL.Path == "/api/v1/organizations" {
					_ = json.NewEncoder(writer).Encode([]appapi.Organization{{Name: "new", ID: "new-id"}})
					return
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/oidc") {
					_ = json.NewEncoder(writer).Encode(appapi.OrganizationOidc{})
					return
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/pocketid") {
					_ = json.NewEncoder(writer).Encode(appapi.OrganizationPocketID{})
					return
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/pocketid/configuration") {
					_ = json.NewEncoder(writer).Encode(appapi.OrganizationPocketIDConfiguration{Revision: "onboarding-test"})
					return
				}
				if serveActionPageFixture(writer, request) {
					return
				}
				assert.Equal(request.Method, http.MethodPut)
				assert.Equal(request.URL.Path, "/api/v1/organizations/new")
				var input appapi.OrganizationOnboardingInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
				if mode == "none" {
					assert.True(input.AdminEmail == nil)
				} else {
					assert.Equal(*input.AdminEmail, "owner@example.test")
				}
				switch mode {
				case "external":
					assert.Equal(input.Oidc.IssuerURL, "https://id.example.test")
					assert.Equal(*input.Oidc.ClientSecret, "  private-client-secret  ")
				case "new-pocketid":
					assert.Equal(input.PocketID.AdminEmail, "identity-manager@example.test")
					assert.Equal(input.PocketID.BaseURL, "https://borq.example.test")
				case "reuse":
					assert.Equal(*input.PocketIDOrganization, "initial")
				default:
					assert.True(input.PocketIDOrganization == nil && input.PocketID == nil && input.Oidc == nil)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"name":"new","id":"new-id","managedAddons":true,"authenticationState":"queued"}`))
			}))
			assert.NoErr(err)
			administratorWeb := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				web.ServeHTTP(writer, request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true})))
			})
			email := "owner@example.test"
			if mode == "none" {
				email = ""
			}
			response := postForm(administratorWeb, url.Values{"action": {"create-organization"}, "name": {"new"}, "provider": {mode}, "adminEmail": {email}, "pocketIdAdminEmail": {"identity-manager@example.test"}, "issuerUrl": {"https://id.example.test"}, "borqPublicUrl": {"https://borq.example.test"}, "clientId": {"client"}, "clientSecret": {"  private-client-secret  "}, "pocketIdOrganization": {"initial"}})
			assert.Equal(response.Code, http.StatusOK)
			page := decodeView[pageData](t, response, "workspace")
			assert.True(strings.Contains(page.Notice, "setup is queued"))
			assert.Equal(page.Location, "/?organization=new&view=organization-authentication")
			assert.True(!strings.Contains(response.Body.String(), "private-client-secret"))
		})
	}
}
