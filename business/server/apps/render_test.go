package apps

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestRenderExample checks that the runnable frontend/backend example produces
// routable private Services, a public Traefik route, and complete healthchecks.
func TestRenderExample(t *testing.T) {
	assert := is.New(t)
	document, err := os.ReadFile("apps.yaml")
	assert.NoErr(err)
	patch := Patch{}
	assert.NoErr(yaml.UnmarshalStrict(document, &patch))
	configuration, err := Merge(Configuration{}, patch)
	assert.NoErr(err)
	rendered, err := Render(configuration, "borq-shop", "env-shop-prod")
	assert.NoErr(err)
	assert.Equal(rendered.RendererVersion, "0.12.0")
	assert.Equal(rendered.Values["namespace"], "borq-shop")
	assert.Equal(rendered.Values["targetID"], "env-shop-prod")
	assert.Equal(len(rendered.Manifests), 5)
	for _, manifest := range rendered.Manifests {
		resource := unstructured.Unstructured{Object: manifest}
		assert.Equal(resource.GetNamespace(), "borq-shop")
		assert.Equal(resource.GetLabels()["borq.run/target-id"], "env-shop-prod")
		assert.Equal(resource.GetLabels()["app.kubernetes.io/managed-by"], "borq")
	}
	frontend := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "frontend"))
	assert.Equal(*frontend.Spec.Replicas, int32(1))
	assert.Equal(frontend.Spec.Selector.MatchLabels["borq.run/target-id"], "env-shop-prod")
	assert.Equal(frontend.Spec.Template.Labels["app.kubernetes.io/name"], "frontend")
	assert.Equal(frontend.Spec.Template.Labels["borq.run/logs"], "true")
	assert.Equal(frontend.Spec.Template.Labels["borq.run/metrics"], "true")
	container := frontend.Spec.Template.Spec.Containers[0]
	assert.Equal(container.Image, "nginx:1.27.5")
	assert.Equal(container.Ports[0].ContainerPort, int32(80))
	assert.Equal(container.ReadinessProbe.HTTPGet.Path, "/")
	assert.Equal(container.ReadinessProbe.HTTPGet.Port.IntVal, int32(80))
	assert.Equal(container.StartupProbe.HTTPGet.Path, "/")
	assert.Equal(container.StartupProbe.FailureThreshold, int32(3))
	service := decodeResource[corev1.Service](t, findManifest(t, rendered, "Service", "backend"))
	assert.Equal(service.Spec.Type, corev1.ServiceTypeClusterIP)
	assert.Equal(service.Spec.Ports[0].Port, int32(80))
	assert.Equal(service.Spec.Ports[0].TargetPort.IntVal, int32(80))
	assert.Equal(service.Spec.Selector["app.kubernetes.io/name"], "backend")
	ingress := decodeResource[networkingv1.Ingress](t, findManifest(t, rendered, "Ingress", "frontend-public"))
	assert.Equal(*ingress.Spec.IngressClassName, "traefik")
	assert.Equal(ingress.Spec.Rules[0].Host, "app.borq.test")
	assert.Equal(ingress.Spec.Rules[0].HTTP.Paths[0].Path, "/")
	assert.Equal(ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name, "frontend")
	assert.Equal(ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number, int32(80))
	assert.Equal(ingress.Spec.TLS, []networkingv1.IngressTLS{{Hosts: []string{"app.borq.test"}, SecretName: "frontend-public-tls"}})
	configuration.Apps[1].Ingresses[0].Host = "*.example.com"
	for _, issuer := range []string{"production-issuer", ""} {
		custom, err := RenderForPlan(configuration, "borq-shop", "env-shop-prod", "plan-1", issuer)
		assert.NoErr(err)
		if issuer == "" {
			issuer = "borq"
		}
		assert.Equal(custom.Values["certificateIssuer"], issuer)
		route := decodeResource[networkingv1.Ingress](t, findManifest(t, custom, "Ingress", "frontend-public"))
		for key, value := range map[string]string{
			"cert-manager.io/cluster-issuer": issuer, "external-dns.kubernetes.io/hostname": "*.example.com",
			"traefik.ingress.kubernetes.io/router.entrypoints": "websecure", "traefik.ingress.kubernetes.io/router.tls": "true",
		} {
			assert.Equal(route.Annotations[key], value)
		}
		assert.Equal(route.Spec.TLS, []networkingv1.IngressTLS{{Hosts: []string{"*.example.com"}, SecretName: "frontend-public-tls"}})
		inspected, err := Inspect(custom.Manifests)
		assert.NoErr(err)
		assert.Equal(inspected, configuration)
	}
	_, found, err := unstructured.NestedInt64(findManifest(t, rendered, "Deployment", "frontend"), "spec", "replicas")
	assert.NoErr(err)
	assert.True(found)
}

// TestRenderOwnershipMetadata keeps ownership on every resource and child pod,
// preserves the app round trip, and avoids changing immutable Service selectors.
func TestRenderOwnershipMetadata(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"env":{"name":"backend-env","version":"7"},"predeploy":{"command":"migrate"},"ingresses":[{"name":"public","host":"app.example.test"}]}]}`))
	assert.NoErr(err)
	_, fixture := renderEnvFixture(t)
	scope := ResourceScope{Organization: "acme", Space: "production", Cluster: "local"}
	rendered, err := RenderWithEnv(configuration, "shop", "prod", "plan-1", "borq", fixture.EnvSnapshots, scope)
	assert.NoErr(err)
	expected := map[string]string{"borq.run/organization-id": "acme", "borq.run/organization": "acme", "borq.run/space-id": "prod", "borq.run/space": "production", "borq.run/cluster": "local", "borq.run/cluster-id": stableId("acme/local"), "borq.run/app-name": "backend", "borq.run/app-id": stableId("prod/backend")}
	for _, manifest := range rendered.Manifests {
		resource := unstructured.Unstructured{Object: manifest}
		for key, value := range expected {
			assert.Equal(resource.GetLabels()[key], value)
			assert.Equal(resource.GetAnnotations()[key], value)
		}
		var path []string
		switch resource.GetKind() {
		case "Deployment", "Job":
			path = []string{"spec", "template", "metadata"}
		case "ExternalSecret":
			path = []string{"spec", "target", "template", "metadata"}
		}
		if len(path) > 0 {
			for _, field := range []string{"labels", "annotations"} {
				metadata, found, err := unstructured.NestedStringMap(manifest, append(path, field)...)
				assert.NoErr(err)
				assert.True(found)
				for key, value := range expected {
					assert.Equal(metadata[key], value)
				}
			}
		}
	}
	namespace, err := plannedNamespace(Plan{Space: Space{Namespace: "shop", ID: "prod"}, Values: rendered.Values})
	assert.NoErr(err)
	assert.Equal(namespace.Labels["borq.run/organization-id"], "acme")
	assert.Equal(namespace.Annotations["borq.run/space-id"], "prod")
	assert.Equal(namespace.Labels["borq.run/app-id"], "")
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	next, err := RenderWithEnv(configuration, "shop", "prod", "plan-2", "borq", fixture.EnvSnapshots, scope)
	assert.NoErr(err)
	assert.Equal(findManifest(t, rendered, "Deployment", "backend"), findManifest(t, next, "Deployment", "backend"))
	service := decodeResource[corev1.Service](t, findManifest(t, rendered, "Service", "backend"))
	assert.Equal(len(service.Spec.Selector), 2)
	_, err = RenderWithEnv(configuration, "shop", "prod", "plan-1", "borq", fixture.EnvSnapshots, ResourceScope{Organization: "acme"})
	assert.True(err != nil)
}

// TestRenderOverrides verifies independent probes, explicit replica zero, TLS
// routes, and container command/environment values survive chart rendering.
func TestRenderOverrides(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"example/backend:v2","port":8080,"replicas":0,"env":{"name":"backend-env","version":"7"},"command":"/app --serve","ingresses":[{"name":"public","host":"api.example.com","path":"/api"}],"checks":{"readiness":{"enabled":false},"startup":{"path":"/start","port":8081,"periodSeconds":2,"timeoutSeconds":3,"failureThreshold":40}}}]}`))
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "backend"))
	assert.Equal(*deployment.Spec.Replicas, int32(0))
	container := deployment.Spec.Template.Spec.Containers[0]
	assert.True(container.ReadinessProbe == nil)
	assert.Equal(container.StartupProbe.HTTPGet.Path, "/start")
	assert.Equal(container.StartupProbe.HTTPGet.Port.IntVal, int32(8081))
	assert.Equal(container.StartupProbe.PeriodSeconds, int32(2))
	assert.Equal(container.StartupProbe.TimeoutSeconds, int32(3))
	assert.Equal(container.StartupProbe.FailureThreshold, int32(40))
	assert.Equal(container.Command, []string{"/app", "--serve"})
	assert.Equal(len(container.Args), 0)
	assert.Equal(len(container.Env), 0)
	assert.Equal(container.EnvFrom, []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: EnvSecretName(rendered.EnvSnapshots[0])}}}})
	ingress := decodeResource[networkingv1.Ingress](t, findManifest(t, rendered, "Ingress", "backend-public"))
	assert.Equal(ingress.Spec.TLS, []networkingv1.IngressTLS{{Hosts: []string{"api.example.com"}, SecretName: "backend-public-tls"}})
	assert.Equal(ingress.Spec.Rules[0].HTTP.Paths[0].Path, "/api")
}

// TestRenderWorker verifies apps without listeners emit neither Services nor
// HTTP probes, while empty container arrays remain explicit for reconciliation.
func TestRenderWorker(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"worker","image":"example/worker:v1"}]}`))
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	assert.Equal(len(rendered.Manifests), 1)
	deployment := decodeResource[appsv1.Deployment](t, rendered.Manifests[0])
	container := deployment.Spec.Template.Spec.Containers[0]
	assert.True(container.ReadinessProbe == nil)
	assert.True(container.StartupProbe == nil)
	assert.Equal(len(container.Ports), 0)
	containers, found, err := unstructured.NestedSlice(rendered.Manifests[0], "spec", "template", "spec", "containers")
	assert.NoErr(err)
	assert.True(found)
	managed := containers[0].(map[string]any)
	for _, key := range []string{"env", "command", "args", "ports"} {
		assert.Equal(managed[key], []any{})
	}
}

// TestRenderCommandDollars preserves literal dollars in direct arguments and
// explicit shell scripts without kubelet substitution, preserving quoted words,
// empty arguments, escaped whitespace and shell symbols as direct-exec argv.
func TestRenderCommandDollars(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		command  string
		rendered []string
	}{
		{
			name:     "quoted and escaped arguments",
			command:  `/app "two words" '' escaped\ space '$HOME' '$(id)' '|'`,
			rendered: []string{"/app", "two words", "", "escaped space", "$$HOME", "$$(id)", "|"},
		},
		{
			name:     "literal arguments",
			command:  `/app '$(MESSAGE)' '$$' '$FOO'`,
			rendered: []string{"/app", "$$(MESSAGE)", "$$$$", "$$FOO"},
		},
		{
			name:     "explicit shell expressions",
			command:  `/bin/sh -c 'echo "$MESSAGE"; printf "%s" "$$"; exec ./app "$@"'`,
			rendered: []string{"/bin/sh", "-c", `echo "$$MESSAGE"; printf "%s" "$$$$"; exec ./app "$$@"`},
		},
		{
			name:     "mixed dollars and parentheses",
			command:  `/app '$$(MESSAGE)' '$' 'literal$(MESSAGE)text'`,
			rendered: []string{"/app", "$$$$(MESSAGE)", "$$", "literal$$(MESSAGE)text"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			image := "worker:v1"
			patch := Patch{Apps: &[]AppPatch{{Name: "worker", Image: &image, Command: &scenario.command, Predeploy: &PredeployPatch{Command: &scenario.command}}}}
			configuration, err := Merge(Configuration{}, patch)
			assert.NoErr(err)
			rendered, err := Render(configuration, "shop", "production")
			assert.NoErr(err)
			deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "worker"))
			assert.Equal(deployment.Spec.Template.Spec.Containers[0].Command, scenario.rendered)
			assert.Equal(len(deployment.Spec.Template.Spec.Containers[0].Args), 0)
			job := decodeResource[batchv1.Job](t, predeployManifest(t, rendered))
			assert.Equal(job.Spec.Template.Spec.Containers[0].Command, scenario.rendered)
			inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
			assert.NoErr(err)
			assert.Equal(inspected, configuration)
		})
	}
}

// TestRenderDeterministic ensures unordered app/list input produces the same
// artifact, so immutable saved plans do not churn because of map iteration.
func TestRenderDeterministic(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"z","image":"z:v1","port":80,"env":{"name":"backend-env","version":"7"}},{"name":"a","image":"a:v1","port":90}]}`))
	assert.NoErr(err)
	first, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	configuration.Apps[0], configuration.Apps[1] = configuration.Apps[1], configuration.Apps[0]
	second, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	assert.True(reflect.DeepEqual(first, second))
	empty, err := Render(Configuration{}, "shop", "prod")
	assert.NoErr(err)
	assert.Equal(empty.Manifests, []map[string]any{})
}

// TestRenderIntegerValues prevents large integer values from becoming floating
// point scientific notation while passing through the Helm values document.
func TestRenderIntegerValues(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"replicas":1000000,"checks":{"startup":{"failureThreshold":1000000}}}]}`))
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "backend"))
	assert.Equal(*deployment.Spec.Replicas, int32(1000000))
	assert.Equal(deployment.Spec.Template.Spec.Containers[0].StartupProbe.FailureThreshold, int32(1000000))
}

// TestRenderRejectsInvalidTarget ensures invalid namespaces or ownership labels
// cannot reach Kubernetes through generated resources.
func TestRenderRejectsInvalidTarget(t *testing.T) {
	for _, scenario := range []struct{ namespace, target string }{{"", "prod"}, {"Invalid", "prod"}, {"shop", ""}, {"shop", "invalid/target"}} {
		t.Run(scenario.namespace+scenario.target, func(t *testing.T) {
			assert := is.New(t)
			_, err := Render(Configuration{}, scenario.namespace, scenario.target)
			assert.True(err != nil)
		})
	}
	assert := is.New(t)
	_, err := Render(Configuration{Apps: []App{{Name: "bad"}}}, "shop", "prod")
	assert.True(err != nil)
}

func findManifest(t *testing.T, rendered Rendered, kind, name string) map[string]any {
	t.Helper()
	for _, manifest := range rendered.Manifests {
		resource := unstructured.Unstructured{Object: manifest}
		if resource.GetKind() == kind && resource.GetName() == name {
			return manifest
		}
	}
	t.Fatalf("missing %s/%s", kind, name)
	return nil
}

func decodeResource[Resource any](t *testing.T, manifest map[string]any) Resource {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var resource Resource
	if err := json.Unmarshal(encoded, &resource); err != nil {
		t.Fatal(err)
	}
	return resource
}

// TestRuntimeRevisionLabels connects logs to rendered runtime settings while
// ensuring scaling or updating another app does not roll unchanged replicas.
func TestRuntimeRevisionLabels(t *testing.T) {
	assert := is.New(t)
	initial, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"nginx:1"},{"name":"worker","image":"nginx:1","port":0}]}`))
	assert.NoErr(err)
	first, err := RenderForPlan(initial, "production", "target", "first", "borq")
	assert.NoErr(err)
	before := decodeResource[appsv1.Deployment](t, findManifest(t, first, "Deployment", "web"))
	worker := decodeResource[appsv1.Deployment](t, findManifest(t, first, "Deployment", "worker"))
	assert.Equal(len(before.Spec.Template.Labels["borq.run/revision"]), 12)
	scaled, err := Merge(initial, patchFromJson(t, `{"apps":[{"name":"web","replicas":3}]}`))
	assert.NoErr(err)
	second, err := RenderForPlan(scaled, "production", "target", "second", "borq")
	assert.NoErr(err)
	after := decodeResource[appsv1.Deployment](t, findManifest(t, second, "Deployment", "web"))
	assert.Equal(before.Spec.Template, after.Spec.Template)
	updated, err := Merge(scaled, patchFromJson(t, `{"apps":[{"name":"web","image":"nginx:2","predeploy":{"command":"/app migrate"}}]}`))
	assert.NoErr(err)
	third, err := RenderForPlan(updated, "production", "target", "third", "borq")
	assert.NoErr(err)
	release := decodeResource[appsv1.Deployment](t, findManifest(t, third, "Deployment", "web"))
	assert.True(before.Spec.Template.Labels["borq.run/revision"] != release.Spec.Template.Labels["borq.run/revision"])
	assert.Equal(worker.Spec.Template, decodeResource[appsv1.Deployment](t, findManifest(t, third, "Deployment", "worker")).Spec.Template)
	for _, manifest := range third.Manifests {
		if manifest["kind"] != "Job" {
			continue
		}
		job := decodeResource[batchv1.Job](t, manifest)
		assert.Equal(job.Spec.Template.Labels["borq.run/revision"], release.Spec.Template.Labels["borq.run/revision"])
	}
}
