package apps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestInspectLiveDefaults checks that controller bookkeeping, Kubernetes
// defaults, arbitrary resource order, and an injected sidecar preserve the
// reconstructed model, including nondefault settings of disabled checks.
func TestInspectLiveDefaults(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"env":{"name":"backend-env","version":"7"},"ingresses":[{"name":"public","host":"app.example.com"}]},{"name":"worker","image":"worker:v1","checks":{"readiness":{"enabled":false,"path":"/unused","port":9090,"periodSeconds":7,"timeoutSeconds":5,"failureThreshold":4}}}]}`))
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "production")
	assert.NoErr(err)
	for left, right := 0, len(rendered.Manifests)-1; left < right; left, right = left+1, right-1 {
		rendered.Manifests[left], rendered.Manifests[right] = rendered.Manifests[right], rendered.Manifests[left]
	}
	deployment := findManifest(t, rendered, "Deployment", "backend")
	setManifestField(t, deployment, "live-uid", "metadata", "uid")
	setManifestField(t, deployment, "1234", "metadata", "resourceVersion")
	setManifestField(t, deployment, int64(5), "metadata", "generation")
	setManifestField(t, deployment, "4", "metadata", "annotations", "deployment.kubernetes.io/revision")
	setManifestField(t, deployment, map[string]any{"readyReplicas": int64(1)}, "status")
	setManifestField(t, deployment, true, "spec", "template", "spec", "enableServiceLinks")
	containers, _, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	assert.NoErr(err)
	main := containers[0].(map[string]any)
	main["terminationMessagePath"] = "/dev/termination-log"
	main["readinessProbe"].(map[string]any)["successThreshold"] = int64(1)
	main["startupProbe"].(map[string]any)["httpGet"].(map[string]any)["scheme"] = "HTTP"

	containers = append([]any{map[string]any{"name": "injected-proxy", "image": "proxy:v1", "ports": []any{map[string]any{"containerPort": int64(15000)}}}}, containers...)
	setManifestField(t, deployment, containers, "spec", "template", "spec", "containers")
	service := findManifest(t, rendered, "Service", "backend")
	setManifestField(t, service, "10.43.10.20", "spec", "clusterIP")
	setManifestField(t, service, []any{"10.43.10.20"}, "spec", "clusterIPs")
	setManifestField(t, service, "None", "spec", "sessionAffinity")
	setManifestField(t, service, "Cluster", "spec", "internalTrafficPolicy")
	// An HTTP client decodes JSON numbers as float64, unlike Kubernetes' maps.
	encoded, err := json.Marshal(rendered.Manifests)
	assert.NoErr(err)
	var httpManifests []map[string]any
	assert.NoErr(json.Unmarshal(encoded, &httpManifests))
	inspected, err := InspectWithEnv(httpManifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	empty, err := Inspect([]map[string]any{})
	assert.NoErr(err)
	assert.Equal(empty, Configuration{Apps: []App{}})
}

// TestInspectManualChanges verifies readback observes modeled fields in live
// resources rather than returning a saved configuration hidden in metadata.
func TestInspectManualChanges(t *testing.T) {
	assert := is.New(t)
	_, rendered := inspectFixture(t)
	deployment := findManifest(t, rendered, "Deployment", "backend")
	setManifestField(t, deployment, int64(0), "spec", "replicas")
	containers, _, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	assert.NoErr(err)
	container := containers[0].(map[string]any)
	container["image"] = "backend:manual"

	container["command"] = []any{"/changed", "serve"}
	container["args"] = []any{"--second", "--first"}
	container["readinessProbe"].(map[string]any)["httpGet"].(map[string]any)["path"] = "/manual"
	delete(container, "startupProbe")
	setManifestField(t, deployment, containers, "spec", "template", "spec", "containers")
	ingress := findManifest(t, rendered, "Ingress", "backend-public")
	rules, _, err := unstructured.NestedSlice(ingress, "spec", "rules")
	assert.NoErr(err)
	rules[0].(map[string]any)["host"] = "manual.example.com"
	setManifestField(t, ingress, rules, "spec", "rules")
	setManifestField(t, ingress, "manual.example.com", "metadata", "annotations", "external-dns.kubernetes.io/hostname")
	setManifestField(t, ingress, []any{map[string]any{"hosts": []any{"manual.example.com"}, "secretName": "backend-public-tls"}}, "spec", "tls")
	inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	application := inspected.Apps[0]
	assert.Equal(application.Replicas, 0)
	assert.Equal(application.Image, "backend:manual")
	assert.Equal(application.Env, EnvReference{Name: "backend-env", Version: "7"})
	assert.Equal(application.Command, "/changed serve --second --first")
	assert.Equal(application.Checks.Readiness.Path, "/manual")
	assert.True(!application.Checks.Startup.Enabled)
	assert.Equal(application.Ingresses[0].Host, "manual.example.com")
}

// TestInspectActualProbeWins confirms a manually added probe overrides the
// annotation retaining settings from a formerly disabled probe.
func TestInspectActualProbeWins(t *testing.T) {
	assert := is.New(t)
	_, rendered := inspectFixture(t)
	deployment := findManifest(t, rendered, "Deployment", "backend")
	setManifestField(t, deployment, `{"enabled":false,"path":"/unused","port":80,"periodSeconds":8,"timeoutSeconds":4,"failureThreshold":7}`, "metadata", "annotations", "borq.run/disabled-readiness-v1")
	containers, _, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	assert.NoErr(err)
	probe := containers[0].(map[string]any)["readinessProbe"].(map[string]any)
	delete(probe, "periodSeconds")
	delete(probe, "timeoutSeconds")
	delete(probe, "failureThreshold")
	probe["httpGet"].(map[string]any)["port"] = "http"
	probe["httpGet"].(map[string]any)["path"] = ""
	setManifestField(t, deployment, containers, "spec", "template", "spec", "containers")
	inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected.Apps[0].Checks.Readiness, Check{Enabled: true, Path: "/", Port: 8080, PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3})
}

// TestInspectRejectsUnsupported prevents malformed or unrepresentable resources
// from silently losing deployment behavior during a manifest-to-model conversion.
func TestInspectRejectsUnsupported(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(Rendered) []map[string]any
		want   string
	}{
		{"foreign owner", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "other", "metadata", "labels", "app.kubernetes.io/managed-by")
			return r.Manifests
		}, "ownership"},
		{"unsupported version", func(r Rendered) []map[string]any { r.Manifests[0]["apiVersion"] = "apps/v1beta1"; return r.Manifests }, "unsupported"},
		{"unsupported kind", func(r Rendered) []map[string]any { r.Manifests[0]["kind"] = "StatefulSet"; return r.Manifests }, "unsupported"},
		{"invalid name", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "", "metadata", "name")
			return r.Manifests
		}, "name"},
		{"invalid namespace", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "", "metadata", "namespace")
			return r.Manifests
		}, "namespace"},
		{"mixed namespaces", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[1], "other", "metadata", "namespace")
			return r.Manifests
		}, "one namespace"},
		{"mixed targets", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[1], "other", "metadata", "labels", "borq.run/target-id")
			return r.Manifests
		}, "one namespace"},
		{"duplicate", func(r Rendered) []map[string]any { return append(r.Manifests, r.Manifests[0]) }, "duplicate"},
		{"missing deployment", func(r Rendered) []map[string]any { return filterManifest(r, "Deployment") }, "no matching"},
		{"missing service", func(r Rendered) []map[string]any { return filterManifest(r, "Service") }, "no matching Service"},
		{"malformed field", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "many", "spec", "replicas")
			return r.Manifests
		}, "decode"},
		{"foreign selector", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "other", "spec", "selector", "matchLabels", "app.kubernetes.io/name")
			return r.Manifests
		}, "selector"},
		{"foreign pod template", func(r Rendered) []map[string]any {
			setManifestField(t, r.Manifests[0], "other", "spec", "template", "metadata", "labels", "app.kubernetes.io/name")
			return r.Manifests
		}, "pod template"},
		{"missing app container", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) { c["name"] = "sidecar" })
			return r.Manifests
		}, "missing application container"},
		{"duplicate app container", func(r Rendered) []map[string]any {
			d := findManifest(t, r, "Deployment", "backend")
			c, _, _ := unstructured.NestedSlice(d, "spec", "template", "spec", "containers")
			setManifestField(t, d, append(c, c[0]), "spec", "template", "spec", "containers")
			return r.Manifests
		}, "duplicate application container"},
		{"unescaped environment substitution", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["command"] = []any{"/app", "$(MESSAGE)"}
			})
			return r.Manifests
		}, "command"},
		{"args without executable", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["command"] = []any{}
				c["args"] = []any{"serve"}
			})
			return r.Manifests
		}, "command"},
		{"extra ports", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) { p := c["ports"].([]any); c["ports"] = append(p, p[0]) })
			return r.Manifests
		}, "at most one"},
		{"udp listener", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) { c["ports"].([]any)[0].(map[string]any)["protocol"] = "UDP" })
			return r.Manifests
		}, "TCP"},
		{"invalid listener", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) { c["ports"].([]any)[0].(map[string]any)["containerPort"] = int64(0) })
			return r.Manifests
		}, "between"},
		{"value from", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["env"] = []any{map[string]any{"name": "SECRET", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}}}
			})
			return r.Manifests
		}, "inline environment"},
		{"env from", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["envFrom"] = []any{map[string]any{"configMapRef": map[string]any{"name": "config"}}}
			})
			return r.Manifests
		}, "environment"},
		{"exec probe", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["readinessProbe"] = map[string]any{"exec": map[string]any{"command": []any{"true"}}}
			})
			return r.Manifests
		}, "only HTTP"},
		{"https probe", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["readinessProbe"].(map[string]any)["httpGet"].(map[string]any)["scheme"] = "HTTPS"
			})
			return r.Manifests
		}, "HTTPS"},
		{"unknown probe port", func(r Rendered) []map[string]any {
			changeContainer(t, r, func(c map[string]any) {
				c["readinessProbe"].(map[string]any)["httpGet"].(map[string]any)["port"] = "other"
			})
			return r.Manifests
		}, "named http"},
		{"conflicting predeploy annotations", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Deployment", "backend"), `{"command":"migrate","timeoutSeconds":300}`, "metadata", "annotations", "borq.run/retained-predeploy-v1")
			return r.Manifests
		}, "both retained and disabled"},
		{"bad retained predeploy", func(r Rendered) []map[string]any {
			deployment := findManifest(t, r, "Deployment", "backend")
			unstructured.RemoveNestedField(deployment, "metadata", "annotations", "borq.run/disabled-predeploy-v1")
			setManifestField(t, deployment, `{"unknown":true}`, "metadata", "annotations", "borq.run/retained-predeploy-v1")
			return r.Manifests
		}, "invalid predeploy annotation"},
		{"null retained predeploy", func(r Rendered) []map[string]any {
			deployment := findManifest(t, r, "Deployment", "backend")
			unstructured.RemoveNestedField(deployment, "metadata", "annotations", "borq.run/disabled-predeploy-v1")
			setManifestField(t, deployment, `null`, "metadata", "annotations", "borq.run/retained-predeploy-v1")
			return r.Manifests
		}, "predeploy annotation must be a JSON object"},
		{"trailing retained predeploy", func(r Rendered) []map[string]any {
			deployment := findManifest(t, r, "Deployment", "backend")
			unstructured.RemoveNestedField(deployment, "metadata", "annotations", "borq.run/disabled-predeploy-v1")
			setManifestField(t, deployment, `{} {}`, "metadata", "annotations", "borq.run/retained-predeploy-v1")
			return r.Manifests
		}, "predeploy annotation must contain one JSON object"},
		{"bad annotation", func(r Rendered) []map[string]any { disableWithAnnotation(t, r, "not json"); return r.Manifests }, "annotation"},
		{"null annotation", func(r Rendered) []map[string]any { disableWithAnnotation(t, r, "null"); return r.Manifests }, "annotation"},
		{"unsupported annotation version", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Deployment", "backend"), `{}`, "metadata", "annotations", "borq.run/disabled-readiness-v2")
			return r.Manifests
		}, "unsupported disabled-check"},
		{"unknown annotation field", func(r Rendered) []map[string]any { disableWithAnnotation(t, r, `{"unknown":1}`); return r.Manifests }, "annotation"},
		{"trailing annotation", func(r Rendered) []map[string]any { disableWithAnnotation(t, r, `{} {}`); return r.Manifests }, "one JSON"},
		{"enabled missing probe", func(r Rendered) []map[string]any { disableWithAnnotation(t, r, `{"enabled":true}`); return r.Manifests }, "cannot enable"},
		{"invalid disabled settings", func(r Rendered) []map[string]any {
			disableWithAnnotation(t, r, `{"path":"relative"}`)
			return r.Manifests
		}, "absolute"},
		{"external service", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Service", "backend"), "LoadBalancer", "spec", "type")
			return r.Manifests
		}, "private ClusterIP"},
		{"extra service ports", func(r Rendered) []map[string]any {
			s := findManifest(t, r, "Service", "backend")
			p, _, _ := unstructured.NestedSlice(s, "spec", "ports")
			setManifestField(t, s, append(p, p[0]), "spec", "ports")
			return r.Manifests
		}, "exactly one"},
		{"service port mismatch", func(r Rendered) []map[string]any { changeServicePort(t, r, "port", int64(80)); return r.Manifests }, "TCP listener"},
		{"service target mismatch", func(r Rendered) []map[string]any {
			changeServicePort(t, r, "targetPort", int64(80))
			return r.Manifests
		}, "target port"},
		{"wrong ingress class", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), "nginx", "spec", "ingressClassName")
			return r.Manifests
		}, "Traefik"},
		{"extra ingress rules", func(r Rendered) []map[string]any {
			i := findManifest(t, r, "Ingress", "backend-public")
			p, _, _ := unstructured.NestedSlice(i, "spec", "rules")
			setManifestField(t, i, append(p, p[0]), "spec", "rules")
			return r.Manifests
		}, "one explicit"},
		{"no ingress paths", func(r Rendered) []map[string]any {
			changeIngressRule(t, r, func(rule map[string]any) { rule["http"] = map[string]any{"paths": []any{}} })
			return r.Manifests
		}, "one HTTP path"},
		{"wrong ingress backend", func(r Rendered) []map[string]any {
			changeIngressRule(t, r, func(rule map[string]any) {
				rule["http"].(map[string]any)["paths"].([]any)[0].(map[string]any)["backend"].(map[string]any)["service"].(map[string]any)["name"] = "foreign"
			})
			return r.Manifests
		}, "matching application"},
		{"wrong TLS host", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), []any{map[string]any{"hosts": []any{"other.example.com"}, "secretName": "tls"}}, "spec", "tls")
			return r.Manifests
		}, "route host"},
		{"missing TLS", func(r Rendered) []map[string]any {
			unstructured.RemoveNestedField(findManifest(t, r, "Ingress", "backend-public"), "spec", "tls")
			return r.Manifests
		}, "managed certificate"},
		{"unmanaged TLS secret", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), []any{map[string]any{"hosts": []any{"app.example.com"}, "secretName": "custom-secret"}}, "spec", "tls")
			return r.Manifests
		}, "managed certificate"},
		{"missing certificate issuer", func(r Rendered) []map[string]any {
			unstructured.RemoveNestedField(findManifest(t, r, "Ingress", "backend-public"), "metadata", "annotations", "cert-manager.io/cluster-issuer")
			return r.Manifests
		}, "automatic DNS"},
		{"invalid certificate issuer", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), "Invalid_Issuer", "metadata", "annotations", "cert-manager.io/cluster-issuer")
			return r.Manifests
		}, "automatic DNS"},
		{"different DNS hostname", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), "other.example.com", "metadata", "annotations", "external-dns.kubernetes.io/hostname")
			return r.Manifests
		}, "automatic DNS"},
		{"HTTP entrypoint", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), "web", "metadata", "annotations", "traefik.ingress.kubernetes.io/router.entrypoints")
			return r.Manifests
		}, "HTTPS entrypoint"},
		{"disabled TLS", func(r Rendered) []map[string]any {
			setManifestField(t, findManifest(t, r, "Ingress", "backend-public"), "false", "metadata", "annotations", "traefik.ingress.kubernetes.io/router.tls")
			return r.Manifests
		}, "HTTPS entrypoint"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			_, rendered := inspectFixture(t)
			_, err := InspectWithEnv(scenario.change(rendered), rendered.EnvSnapshots)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), scenario.want))
		})
	}
}

func inspectFixture(t *testing.T) (Configuration, Rendered) {
	t.Helper()
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","port":8080,"env":{"name":"backend-env","version":"7"},"ingresses":[{"name":"public","host":"app.example.com"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(configuration, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	return configuration, rendered
}

func setManifestField(t *testing.T, manifest map[string]any, value any, fields ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(manifest, value, fields...); err != nil {
		t.Fatal(err)
	}
}

func changeContainer(t *testing.T, rendered Rendered, change func(map[string]any)) {
	t.Helper()
	deployment := findManifest(t, rendered, "Deployment", "backend")
	containers, _, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	change(containers[0].(map[string]any))
	setManifestField(t, deployment, containers, "spec", "template", "spec", "containers")
}

func changeServicePort(t *testing.T, rendered Rendered, name string, value any) {
	t.Helper()
	service := findManifest(t, rendered, "Service", "backend")
	ports, _, err := unstructured.NestedSlice(service, "spec", "ports")
	if err != nil {
		t.Fatal(err)
	}
	ports[0].(map[string]any)[name] = value
	setManifestField(t, service, ports, "spec", "ports")
}

func changeIngressRule(t *testing.T, rendered Rendered, change func(map[string]any)) {
	t.Helper()
	ingress := findManifest(t, rendered, "Ingress", "backend-public")
	rules, _, err := unstructured.NestedSlice(ingress, "spec", "rules")
	if err != nil {
		t.Fatal(err)
	}
	change(rules[0].(map[string]any))
	setManifestField(t, ingress, rules, "spec", "rules")
}

func disableWithAnnotation(t *testing.T, rendered Rendered, annotation string) {
	t.Helper()
	changeContainer(t, rendered, func(container map[string]any) { delete(container, "readinessProbe") })
	setManifestField(t, findManifest(t, rendered, "Deployment", "backend"), annotation, "metadata", "annotations", "borq.run/disabled-readiness-v1")
}

func filterManifest(rendered Rendered, kind string) []map[string]any {
	remaining := []map[string]any{}
	for _, manifest := range rendered.Manifests {
		if manifest["kind"] != kind {
			remaining = append(remaining, manifest)
		}
	}
	return remaining
}
