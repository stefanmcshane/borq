package appapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestApplicationConversions expands JSON updates and verifies lossless rendering
// and resource inspection through the published contract and generated client.
func TestApplicationConversions(t *testing.T) {
	is := is.New(t)
	manager, err := apps.NewManager(context.Background(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	is.NoErr(err)
	defer manager.Close()
	transport, err := NewHandler(manager)
	is.NoErr(err)
	server := httptest.NewServer(transport)
	defer server.Close()
	client, err := NewClientWithResponses(server.URL)
	is.NoErr(err)
	ctx := context.Background()
	backendName := strings.Repeat("b", 50)
	source := `{"apps":[
  {"name":"frontend","image":"nginx:1.27.5","port":8080,
   "env":{"name":"frontend-config","version":"2"},
   "command":"/bin/sh -c \"exec nginx -g 'daemon off;'\"",
   "ingresses":[{"name":"public","host":"app.borq.test"}],
   "checks":{"readiness":{"path":"/ready","timeoutSeconds":3},
   "startup":{"path":"/startup","periodSeconds":2,"failureThreshold":60}}},
  {"name":"` + backendName + `","image":"nginx:1.27.5","replicas":0,
   "predeploy":{"command":"/bin/sh -c 'echo migrate'","timeoutSeconds":120},
   "checks":{"readiness":{"enabled":false,"path":"/disabled-ready","periodSeconds":7},
   "startup":{"enabled":false,"path":"/disabled-start","failureThreshold":19}}}
]}`
	expanded, err := client.ExpandAppsWithBodyWithResponse(ctx, "application/json", strings.NewReader(source))
	is.NoErr(err)
	is.Equal(expanded.StatusCode(), http.StatusOK)
	if expanded.JSON200 == nil {
		t.Fatalf("expand response missing configuration: %s", expanded.Body)
	}
	configuration := *expanded.JSON200
	is.Equal(len(configuration.Apps), 2)
	is.Equal(configuration.Apps[0].Name, backendName)
	is.Equal(configuration.Apps[0].Replicas, 0)
	is.Equal(configuration.Apps[0].Env.Name, "")
	is.Equal(configuration.Apps[0].Env.Version, "latest")
	is.Equal(configuration.Apps[0].Predeploy.BackoffLimit, 0)
	is.Equal(configuration.Apps[0].Predeploy.TimeoutSeconds, 120)
	is.True(configuration.Apps[0].Predeploy.Command != "")
	is.Equal(configuration.Apps[1].Replicas, 1)
	is.Equal(configuration.Apps[1].Checks.Readiness.TimeoutSeconds, 3)
	is.True(configuration.Apps[1].Checks.Readiness.Enabled)
	is.True(configuration.Apps[1].Checks.Startup.Enabled)
	is.Equal(configuration.Apps[1].Checks.Startup.FailureThreshold, 60)
	is.Equal(configuration.Apps[1].Predeploy.Command, "")
	is.Equal(configuration.Apps[1].Predeploy.TimeoutSeconds, 300)
	is.Equal(configuration.Apps[1].Ingresses[0].Path, "/")
	is.Equal(configuration.Apps[1].Ingresses[0].Host, "app.borq.test")

	encoded, err := json.Marshal(configuration)
	is.NoErr(err)
	is.True(!strings.Contains(string(encoded), "tlsSecret"))
	reexpanded, err := client.ExpandAppsWithBodyWithResponse(ctx, "application/json", bytes.NewReader(encoded))
	is.NoErr(err)
	is.Equal(reexpanded.StatusCode(), http.StatusOK)
	is.Equal(reexpanded.HTTPResponse.Header.Get("Content-Type"), "application/json")
	is.Equal(*reexpanded.JSON200, configuration)

	rendered, err := client.RenderAppsWithResponse(ctx, RenderInput{
		Configuration: configuration, Namespace: "shop-dev", TargetID: "target-dev",
		Scope: &ResourceScope{Organization: "acme", Space: "dev", Cluster: "local"}, // The generated client uses a pointer for optional rendering context.
	})
	is.NoErr(err)
	is.Equal(rendered.StatusCode(), http.StatusOK)
	if rendered.JSON200 == nil {
		t.Fatalf("render response missing manifests: %s", rendered.Body)
	}
	is.Equal(len(rendered.JSON200.Manifests), 8)
	is.True(rendered.JSON200.RendererVersion != "")
	is.Equal(rendered.JSON200.Values["namespace"], "shop-dev")
	is.Equal(rendered.JSON200.Values["certificateIssuer"], "borq")
	for _, manifest := range rendered.JSON200.Manifests {
		metadata := manifest["metadata"].(map[string]any)
		for _, field := range []string{"labels", "annotations"} {
			identity := metadata[field].(map[string]any)
			is.Equal(identity["borq.run/organization-id"], "acme")
			is.Equal(identity["borq.run/space-id"], "target-dev")
			is.Equal(identity["borq.run/space"], "dev")
			is.Equal(identity["borq.run/cluster"], "local")
		}
	}
	inspected, err := client.InspectAppsWithResponse(ctx, InspectInput{Manifests: rendered.JSON200.Manifests})
	is.NoErr(err)
	is.Equal(inspected.StatusCode(), http.StatusOK)
	if inspected.JSON200 == nil {
		t.Fatalf("inspect response missing configuration: %s", inspected.Body)
	}
	is.Equal(*inspected.JSON200, configuration)
	planID := "saved-plan"
	issuer := "letsencrypt-dns"
	withPlan, err := client.RenderAppsWithResponse(ctx, RenderInput{
		Configuration: configuration, Namespace: "shop-dev", TargetID: "target-dev", PlanID: &planID, CertificateIssuer: &issuer,
	})
	is.NoErr(err)
	is.Equal(withPlan.StatusCode(), http.StatusOK)
	is.Equal(withPlan.JSON200.Values["planID"], planID)
	is.Equal(withPlan.JSON200.Values["certificateIssuer"], issuer)
	for _, manifest := range withPlan.JSON200.Manifests {
		if manifest["kind"] == "Ingress" {
			annotations := manifest["metadata"].(map[string]any)["annotations"].(map[string]any)
			is.Equal(annotations["cert-manager.io/cluster-issuer"], issuer)
			is.Equal(annotations["external-dns.kubernetes.io/hostname"], "app.borq.test")
		}
		if manifest["kind"] != "Job" {
			continue
		}
		name := manifest["metadata"].(map[string]any)["name"]
		for _, previewManifest := range rendered.JSON200.Manifests {
			if previewManifest["kind"] == "Job" {
				is.True(name != previewManifest["metadata"].(map[string]any)["name"])
			}
		}
	}
}

// TestJsonContractRejectsAmbiguousInput ensures JSON is validated before either
// expansion or planning and keeps YAML parsing outside the API. App fixtures
// include a valid image so unrelated merge failures cannot mask lost validation.
func TestJsonContractRejectsAmbiguousInput(t *testing.T) {
	transport, err := NewHandler(apps.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		name, mediaType, body string
		status                int
	}{
		{"null document", "application/json", `null`, 400},
		{"array document", "application/json", `[]`, 400},
		{"empty body", "application/json", ``, 400},
		{"duplicate json key", "application/json", `{"apps":[],"apps":[]}`, 400},
		{"duplicate nested json key", "application/json", `{"apps":[{"name":"api","image":"app:v1","name":"api"}]}`, 400},
		{"null json list", "application/json", `{"apps":null}`, 400},
		{"null app", "application/json", `{"apps":[null]}`, 400},
		{"missing app name", "application/json", `{"apps":[{"image":"app:v1"}]}`, 400},
		{"null image", "application/json", `{"apps":[{"name":"api","image":null}]}`, 400},
		{"null env", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":null}]}`, 400},
		{"negative replicas", "application/json", `{"apps":[{"name":"api","image":"app:v1","replicas":-1}]}`, 400},
		{"invalid port", "application/json", `{"apps":[{"name":"api","image":"app:v1","port":65536}]}`, 400},
		{"zero startup timeout", "application/json", `{"apps":[{"name":"api","image":"app:v1","checks":{"startup":{"timeoutSeconds":0}}}]}`, 400},
		{"long app name", "application/json", `{"apps":[{"name":"` + strings.Repeat("a", 51) + `","image":"app:v1"}]}`, 400},
		{"unknown json field", "application/json", `{"apps":[],"cluster":"test"}`, 400},
		{"unknown env field", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":{"name":"backend","unknown":"secret-to-keep-private"}}]}`, 400},
		{"legacy inline env list", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":[{"name":"TOKEN","value":"secret-to-keep-private"}]}]}`, 400},
		{"inline env values", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":{"name":"backend","values":[{"name":"TOKEN","value":"secret-to-keep-private"}]}}]}`, 400},
		{"numeric env version", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":{"name":"backend","version":2}}]}`, 400},
		{"invalid env version", "application/json", `{"apps":[{"name":"api","image":"app:v1","env":{"name":"backend","version":"next"}}]}`, 400},
		{"null predeploy object", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":null}]}`, 400},
		{"predeploy list", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":[{"command":"migrate"}]}]}`, 400},
		{"named predeploy", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":{"name":"migrate","command":"migrate"}}]}`, 400},
		{"unsupported task image", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":{"command":"migrate","image":"other:v1"}}]}`, 400},
		{"array command", "application/json", `{"apps":[{"name":"api","image":"app:v1","command":["run"]}]}`, 400},
		{"array predeploy command", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":{"command":["migrate"]}}]}`, 400},
		{"separate args", "application/json", `{"apps":[{"name":"api","image":"app:v1","args":["serve"]}]}`, 400},
		{"separate predeploy args", "application/json", `{"apps":[{"name":"api","image":"app:v1","predeploy":{"command":"migrate","args":[]}}]}`, 400},
		{"top-level readiness", "application/json", `{"apps":[{"name":"api","image":"app:v1","readiness":{"enabled":false}}]}`, 400},
		{"top-level startup", "application/json", `{"apps":[{"name":"api","image":"app:v1","startup":{"enabled":false}}]}`, 400},
		{"public TLS secret", "application/json", `{"apps":[{"name":"api","image":"app:v1","port":8080,"ingresses":[{"name":"public","host":"example.com","tlsSecret":"app-tls"}]}]}`, 400},
		{"null checks", "application/json", `{"apps":[{"name":"api","image":"app:v1","checks":null}]}`, 400},
		{"null nested readiness", "application/json", `{"apps":[{"name":"api","image":"app:v1","checks":{"readiness":null}}]}`, 400},
		{"yaml in json body", "application/json", "apps: []\n", 400},
		{"malformed json", "application/json", `{"apps":[`, 400},
		{"multiple json documents", "application/json", `{"apps":[]} {"apps":[]}`, 400},
		{"yaml request", "application/yaml", "apps: []\n", 415},
		{"json body limit", "application/json", strings.Repeat(" ", (1<<20)+1), 400},
		{"unsupported media", "text/plain", "apps: []\n", 415},
		{"malformed media", "application/json; invalid", `{}`, 415},
	} {
		t.Run(input.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/apps/expand", "/api/v1/organizations/team/spaces/dev/plans"} {
				writer := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(input.body))
				request.Header.Set("Content-Type", input.mediaType)
				transport.ServeHTTP(writer, request)
				if writer.Code != input.status {
					t.Fatalf("%s: status %d, want %d: %s", path, writer.Code, input.status, writer.Body.String())
				}
				if writer.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("%s: error response must be JSON", path)
				}
				if strings.Contains(writer.Body.String(), "secret-to-keep-private") {
					t.Fatalf("%s: response disclosed environment value", path)
				}
			}
		})
	}
}

// TestConversionValidation rejects invalid application definitions and resource
// shapes rather than returning a successful conversion that loses information.
func TestConversionValidation(t *testing.T) {
	is := is.New(t)
	transport, err := NewHandler(apps.Manager{})
	is.NoErr(err)
	for _, input := range []struct{ path, mediaType, body string }{
		{"expand", "application/json", `{"apps":[{"name":"api","port":8080}]}`},
		{"expand", "application/json", `{"apps":[{"name":"api","image":"bad image"}]}`},
		{"expand", "application/json", `{"apps":[{"name":"api","image":"api:v1","predeploy":{"command":"migrate","timeoutSeconds":0}}]}`},
		{"expand", "application/json", `{"apps":[{"name":"api","image":"api:v1","command":"serve 'unterminated"}]}`},
		{"expand", "application/json", `{"apps":[{"name":"api","image":"api:v1","predeploy":{"command":"migrate 'unterminated"}}]}`},
		{"render", "application/json", `{}`},
		{"render", "application/json", `{"configuration":{"apps":[]},"namespace":"INVALID","targetID":"target"}`},
		{"render", "application/json", `{"configuration":{"apps":[]},"namespace":"dev","targetID":"invalid label"}`},
		{"render", "application/json", `{"configuration":{"apps":[]},"namespace":"dev","targetID":"target","certificateIssuer":"invalid/issuer"}`},
		{"render", "application/json", `{"configuration":{"apps":[]},"namespace":"dev","targetID":"target","scope":{"organization":"acme"}}`},
		{"render", "application/json", `{"configuration":{"apps":[]},"namespace":"dev","targetID":"target","scope":{"organization":"acme","space":"Invalid","cluster":"local"}}`},
		{"inspect", "application/json", `{}`},
		{"inspect", "application/json", `{"manifests":[{}]}`},
		{"inspect", "application/json", `{"manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unsupported"}}]}`},
	} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+input.path, strings.NewReader(input.body))
		request.Header.Set("Content-Type", input.mediaType)
		transport.ServeHTTP(writer, request)
		is.Equal(writer.Code, http.StatusBadRequest)
	}
	for _, path := range []string{"render", "inspect"} {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+path, strings.NewReader("apps: []"))
		request.Header.Set("Content-Type", "application/yaml")
		transport.ServeHTTP(writer, request)
		is.Equal(writer.Code, http.StatusUnsupportedMediaType)
	}
}
