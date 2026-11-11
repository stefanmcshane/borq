package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestApplicationRoutePolicyFields owns the browser form's label-to-host conversion
// and ensures posted domain choices are checked against fresh server metadata.
func TestApplicationRoutePolicyFields(t *testing.T) {
	assert := is.New(t)
	var submitted appapi.Patch
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/route-policy") {
			assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/route-policy")
			assert.Equal(request.URL.Query().Get("application"), "webnginx")
			_ = json.NewEncoder(writer).Encode(appapi.ApplicationRoutePolicy{CertificateIssuer: "space-issuer", Domains: []string{"borq.io"}})
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/plans/route-plan" {
			_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "route-plan", Space: appapi.Space{ID: "space-id"}, Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "webnginx", Image: "traefik/whoami", Port: 80}}}})
			return
		}
		if serveActionPageFixture(writer, request) {
			return
		}
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/plans")
		assert.NoErr(json.NewDecoder(request.Body).Decode(&submitted))
		_, _ = writer.Write([]byte(`{"id":"route-plan"}`))
	}))
	assert.NoErr(err)
	form := url.Values{"organization": {"team"}, "space": {"dev"}, "name": {"webnginx"}, "image": {"traefik/whoami"}, "port": {"80"}, "replicas": {"1"}, "format": {"fields"}, "ingressFields": {"on"}, "ingressName": {"tdog"}, "ingressSubdomain": {"web"}, "ingressDomain": {"borq.io"}, "ingressPath": {"/"}}
	editor := decodeView[applicationEditorData](t, postApplicationEditor(web, form), "application-editor")
	assert.Equal(editor.Draft.Ingresses[0].Host, "web.borq.io")
	assert.Equal(editor.Ingresses[0].Subdomain, "web")
	assert.Equal(editor.Ingresses[0].Domain, "borq.io")
	assert.Equal(editor.RoutePolicy.CertificateIssuer, "space-issuer")
	form.Set("action", "plan-app")
	decodeView[pageData](t, postForm(web, form), "workspace")
	assert.Equal((*(*submitted.Apps)[0].Ingresses)[0].Host, "web.borq.io")
	assert.Equal(*(*submitted.Apps)[0].Port, 80)
	for _, input := range []struct{ field, value, message string }{
		{"ingressDomain", "attacker.example", "Choose a domain allowed"},
		{"ingressSubdomain", "web.borq.io", "one subdomain label"},
		{"ingressSubdomain", "nested.web", "one subdomain label"},
		{"ingressSubdomain", "-web", "one subdomain label"},
	} {
		t.Run(input.value, func(t *testing.T) {
			form.Set(input.field, input.value)
			response := postForm(web, form)
			is.New(t).Equal(response.Code, http.StatusBadRequest)
			is.New(t).True(strings.Contains(response.Body.String(), input.message))
			form.Set("ingressDomain", "borq.io")
			form.Set("ingressSubdomain", "web")
		})
	}
}

// TestApplicationLegacyRoutePolicy preserves unsupported accepted hosts and gives
// a correction message instead of silently changing them during an editor refresh.
func TestApplicationLegacyRoutePolicy(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/route-policy")
		_ = json.NewEncoder(writer).Encode(appapi.ApplicationRoutePolicy{CertificateIssuer: "space-issuer", Domains: []string{"borq.io"}})
	}))
	assert.NoErr(err)
	form := url.Values{"organization": {"team"}, "space": {"dev"}, "format": {"yaml"}, "configuration": {"name: web\nimage: traefik/whoami\nport: 80\ningresses:\n- name: public\n  host: old.example.com\n"}}
	response := postApplicationEditor(web, form)
	assert.Equal(response.Code, http.StatusOK)
	editor := decodeView[applicationEditorData](t, response, "application-editor")
	assert.Equal(editor.Draft.Ingresses[0].Host, "old.example.com")
	assert.Equal(editor.Ingresses[0].Subdomain, "old.example.com")
	assert.True(strings.Contains(editor.Ingresses[0].HostError, "outside this space's eligible domains"))
	fields := applicationEditorFields(t, response.Body.String())
	response = postApplicationEditor(web, fields)
	assert.Equal(response.Code, http.StatusBadRequest)
	assert.True(strings.Contains(response.Body.String(), "one subdomain label"))
}

// TestApplicationRoutePolicyUnavailable keeps internal app drafts editable while
// failing an actual public route submission when its policy cannot be verified.
func TestApplicationRoutePolicyUnavailable(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/route-policy")
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(`{"message":"Cluster is temporarily unreachable."}`))
	}))
	assert.NoErr(err)
	form := url.Values{"organization": {"team"}, "space": {"dev"}, "format": {"fields"}, "name": {"webnginx"}, "image": {"traefik/whoami"}, "port": {"80"}, "replicas": {"1"}, "ingressFields": {"on"}}
	response := postApplicationEditor(web, form)
	assert.Equal(response.Code, http.StatusOK)
	editor := decodeView[applicationEditorData](t, response, "application-editor")
	assert.Equal(editor.Draft.Name, "webnginx")
	assert.True(strings.Contains(editor.RoutePolicyError, "Cluster is temporarily unreachable"))
	assert.Equal(len(editor.RoutePolicy.Domains), 0)
	form.Set("ingressName", "tdog")
	form.Set("ingressSubdomain", "web")
	form.Set("ingressDomain", "borq.io")
	form.Set("ingressPath", "/")
	response = postApplicationEditor(web, form)
	assert.Equal(response.Code, http.StatusBadGateway)
	assert.True(strings.Contains(response.Body.String(), "Cluster is temporarily unreachable"))
}
