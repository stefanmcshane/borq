package apps

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TestRenderVersionedEnvironment verifies published configuration references
// survive readback without vault access or secret values in app artifacts.
func TestRenderVersionedEnvironment(t *testing.T) {
	assert := is.New(t)
	configuration, rendered := renderEnvFixture(t)
	assert.Equal(rendered.RendererVersion, "0.14.0")
	encoded, err := json.Marshal(rendered)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "sensitive-literal"))
	assert.True(!strings.Contains(string(encoded), `"envSnapshots"`))
	snapshot := rendered.EnvSnapshots[0]
	secretName := EnvSecretName(snapshot)
	resource := envExternalSecret(t, rendered)
	external := unstructured.Unstructured{Object: resource}
	assert.Equal(external.GetAPIVersion(), "external-secrets.io/v1")
	assert.Equal(external.GetAnnotations()["borq.run/environment-group-name"], "backend-env")
	for _, expected := range []struct {
		path  []string
		value string
	}{
		{[]string{"spec", "refreshPolicy"}, "OnChange"},
		{[]string{"spec", "secretStoreRef", "kind"}, "ClusterSecretStore"},
		{[]string{"spec", "target", "name"}, secretName},
		{[]string{"spec", "target", "creationPolicy"}, "Owner"},
		{[]string{"spec", "target", "deletionPolicy"}, "Merge"},
		{[]string{"spec", "target", "template", "metadata", "annotations", "borq.run/env-version"}, "7"},
		{[]string{"spec", "target", "template", "metadata", "annotations", "borq.run/env-path"}, "borq/environment-groups/prod/backend-env"},
		{[]string{"spec", "target", "template", "metadata", "annotations", "borq.run/env-keys"}, `["EMPTY","TOKEN"]`},
	} {
		value, found, err := unstructured.NestedString(resource, expected.path...)
		assert.NoErr(err)
		assert.True(found)
		assert.Equal(value, expected.value)
	}
	immutable, _, err := unstructured.NestedBool(resource, "spec", "target", "immutable")
	assert.NoErr(err)
	assert.True(immutable)
	dataFrom, _, err := unstructured.NestedSlice(resource, "spec", "dataFrom")
	assert.NoErr(err)
	assert.Equal(dataFrom, []any{map[string]any{"extract": map[string]any{"key": snapshot.Path, "property": "values", "version": "7"}}})
	deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "backend"))
	job := decodeResource[batchv1.Job](t, predeployManifest(t, rendered))
	container := deployment.Spec.Template.Spec.Containers[0]
	assert.Equal(len(container.Env), 0)
	assert.Equal(container.EnvFrom, []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}}})
	assert.Equal(container.EnvFrom, job.Spec.Template.Spec.Containers[0].EnvFrom)
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	references, err := EnvReferences(rendered.Manifests)
	assert.NoErr(err)
	snapshot.Values = nil
	assert.Equal(references, []EnvSnapshot{snapshot})
	// The defaults added by ESO's CRD do not change the public app reference.
	setManifestField(t, resource, "1h0m0s", "spec", "refreshInterval")
	setManifestField(t, resource, "v2", "spec", "target", "template", "engineVersion")
	setManifestField(t, resource, "Replace", "spec", "target", "template", "mergePolicy")
	inspected, err = Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	second, err := RenderWithEnv(configuration, "shop", "prod", "another-plan", "borq", rendered.EnvSnapshots, ResourceScope{})
	assert.NoErr(err)
	assert.Equal(EnvSecretName(second.EnvSnapshots[0]), secretName)
}

// TestEmptyAndUnresolvedEnvironment preserves public references without
// inventing a vault document, including the reserved empty version zero.
func TestEmptyAndUnresolvedEnvironment(t *testing.T) {
	for _, env := range []string{`{}`, `{"name":"backend-env"}`, `{"name":"backend-env","version":"0"}`} {
		t.Run(env, func(t *testing.T) {
			assert := is.New(t)
			configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":`+env+`}]}`))
			assert.NoErr(err)
			rendered, err := Render(configuration, "shop", "prod")
			assert.NoErr(err)
			assert.Equal(len(rendered.Manifests), 1)
			inspected, err := Inspect(rendered.Manifests)
			assert.NoErr(err)
			assert.Equal(inspected, configuration)
		})
	}
}

// TestPinnedEmptyEnvironment allows a published config with zero keys and
// still records its concrete version. Merge deletion policy makes ESO create
// the empty Secret instead of treating an empty map as a deleted provider key.
func TestPinnedEmptyEnvironment(t *testing.T) {
	assert := is.New(t)
	configuration, rendered := renderEnvFixture(t)
	rendered.EnvSnapshots[0].Values = []Variable{}
	rendered, err := RenderWithEnv(configuration, "shop", "prod", "plan-1", "borq", rendered.EnvSnapshots, ResourceScope{})
	assert.NoErr(err)
	resource := envExternalSecret(t, rendered)
	policy, _, err := unstructured.NestedString(resource, "spec", "target", "deletionPolicy")
	assert.NoErr(err)
	assert.Equal(policy, "Merge")
	keys, _, err := unstructured.NestedString(resource, "spec", "target", "template", "metadata", "annotations", "borq.run/env-keys")
	assert.NoErr(err)
	assert.Equal(keys, "[]")
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
}

// TestRenderEnvironmentSnapshotValidation rejects references to another config,
// another target, or malformed variable names before rendering resources.
func TestRenderEnvironmentSnapshotValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]EnvSnapshot) []EnvSnapshot
	}{
		{"missing", func(s []EnvSnapshot) []EnvSnapshot { return []EnvSnapshot{} }},
		{"duplicate", func(s []EnvSnapshot) []EnvSnapshot { return append(s, s[0]) }},
		{"other app", func(s []EnvSnapshot) []EnvSnapshot { s[0].AppName = "other"; return s }},
		{"other config", func(s []EnvSnapshot) []EnvSnapshot { s[0].ConfigName = "other"; return s }},
		{"other target", func(s []EnvSnapshot) []EnvSnapshot {
			s[0].Path = "borq/environment-groups/staging/backend-env"
			return s
		}},
		{"invalid store", func(s []EnvSnapshot) []EnvSnapshot { s[0].Store = "Invalid Store"; return s }},
		{"missing identity", func(s []EnvSnapshot) []EnvSnapshot { s[0].SnapshotID = ""; return s }},
		{"wrong version", func(s []EnvSnapshot) []EnvSnapshot { s[0].Version = 8; return s }},
		{"invalid variable", func(s []EnvSnapshot) []EnvSnapshot { s[0].Values = []Variable{{Name: "invalid=name"}}; return s }},
		{"duplicate variable", func(s []EnvSnapshot) []EnvSnapshot { s[0].Values = append(s[0].Values, s[0].Values[0]); return s }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			configuration, rendered := renderEnvFixture(t)
			_, err := RenderWithEnv(configuration, "shop", "prod", "plan-1", "borq", test.change(rendered.EnvSnapshots), ResourceScope{})
			assert.True(err != nil)
		})
	}
}

// TestInspectRejectsEnvironmentDrift prevents unsupported env transformations
// or mismatched metadata from being disguised as a valid named config.
func TestInspectRejectsEnvironmentDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(Rendered)
	}{
		{"wrong resolver", func(r Rendered) { r.EnvSnapshots[0].Version++ }},
		{"wrong Secret", func(r Rendered) { changeEnvReference(t, r, "name", "other") }},
		{"optional Secret", func(r Rendered) { changeEnvReference(t, r, "optional", true) }},
		{"inline values", func(r Rendered) {
			changeContainer(t, r, func(c map[string]any) { c["env"] = []any{map[string]any{"name": "MANUAL", "value": "changed"}} })
		}},
		{"prefix", func(r Rendered) {
			changeContainer(t, r, func(c map[string]any) { c["envFrom"].([]any)[0].(map[string]any)["prefix"] = "OTHER_" })
		}},
		{"wrong config annotation", func(r Rendered) {
			setManifestField(t, findManifest(t, r, "Deployment", "backend"), `{"name":"other","version":"7"}`, "metadata", "annotations", "borq.run/environment-group")
		}},
		{"wrong refresh", func(r Rendered) { setManifestField(t, envExternalSecret(t, r), "Periodic", "spec", "refreshPolicy") }},
		{"mutable Secret", func(r Rendered) { setManifestField(t, envExternalSecret(t, r), false, "spec", "target", "immutable") }},
		{"wrong property", func(r Rendered) { changeEnvExtract(t, r, "property", "other") }},
		{"latest version", func(r Rendered) { changeEnvExtract(t, r, "version", "") }},
		{"other path", func(r Rendered) { changeEnvExtract(t, r, "key", "borq/environment-groups/staging/backend-env") }},
		{"predeploy changed reference", func(r Rendered) { mutatePredeployContainer(t, predeployManifest(t, r), "envFrom", []any{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			_, rendered := renderEnvFixture(t)
			test.change(rendered)
			_, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
			assert.True(err != nil)
		})
	}
}

// TestEnvironmentSecretNames keeps names valid at the app-name boundary and
// separates config identities and versions without exposing secret values.
func TestEnvironmentSecretNames(t *testing.T) {
	assert := is.New(t)
	snapshot := EnvSnapshot{AppName: strings.Repeat("a", 50), SnapshotID: "prod/backend/1", Version: math.MaxInt}
	first := EnvSecretName(snapshot)
	assert.True(len(first) <= 253)
	assert.Equal(len(validation.IsDNS1123Subdomain(first)), 0)
	snapshot.SnapshotID = "prod/other/1"
	assert.True(first != EnvSecretName(snapshot))
	snapshot.SnapshotID = "prod/backend/1"
	snapshot.Version--
	assert.True(first != EnvSecretName(snapshot))
}

// TestInspectLegacyEnvironmentGroup reads the previous annotations and vault
// paths so a rename cannot break live diffs or the exact artifacts in saved plans.
func TestInspectLegacyEnvironmentGroup(t *testing.T) {
	is := is.New(t)
	configuration, rendered := renderEnvFixture(t)
	for _, manifest := range rendered.Manifests {
		if manifest["kind"] == "Deployment" {
			resource := unstructured.Unstructured{Object: manifest}
			annotations := resource.GetAnnotations()
			annotations["borq.run/env-config"] = annotations["borq.run/environment-group"]
			delete(annotations, "borq.run/environment-group")
			resource.SetAnnotations(annotations)
		}
	}
	resource := envExternalSecret(t, rendered)
	legacyPath := "borq/env-configs/prod/backend-env"
	for _, metadataPath := range [][]string{{"metadata", "annotations"}, {"spec", "target", "template", "metadata", "annotations"}} {
		annotations, _, err := unstructured.NestedStringMap(resource, metadataPath...)
		is.NoErr(err)
		annotations["borq.run/env-config-name"] = annotations["borq.run/environment-group-name"]
		delete(annotations, "borq.run/environment-group-name")
		if annotations["borq.run/env-path"] != "" {
			annotations["borq.run/env-path"] = legacyPath
		}
		is.NoErr(unstructured.SetNestedStringMap(resource, annotations, metadataPath...))
	}
	changeEnvExtract(t, rendered, "key", legacyPath)
	inspected, err := Inspect(rendered.Manifests)
	is.NoErr(err)
	is.Equal(inspected, configuration)
	references, err := EnvReferences(rendered.Manifests)
	is.NoErr(err)
	is.Equal(references[0].Path, legacyPath)
	is.Equal(references[0].Version, 7)
}

func renderEnvFixture(t *testing.T) (Configuration, Rendered) {
	t.Helper()
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend-env","version":"7"},"predeploy":{"command":"migrate"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderWithEnv(configuration, "shop", "prod", "plan-1", "borq", []EnvSnapshot{{AppName: "backend", ConfigName: "backend-env", Store: "borq-openbao", Path: "borq/environment-groups/prod/backend-env", Version: 7, SnapshotID: "prod/backend-env/7", Values: []Variable{{Name: "TOKEN", Value: "sensitive-literal"}, {Name: "EMPTY", Value: ""}}}}, ResourceScope{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration, rendered
}

func envExternalSecret(t *testing.T, rendered Rendered) map[string]any {
	t.Helper()
	return findManifest(t, rendered, "ExternalSecret", EnvSecretName(rendered.EnvSnapshots[0]))
}
func changeEnvReference(t *testing.T, rendered Rendered, key string, value any) {
	t.Helper()
	changeContainer(t, rendered, func(c map[string]any) {
		c["envFrom"].([]any)[0].(map[string]any)["secretRef"].(map[string]any)[key] = value
	})
}
func changeEnvExtract(t *testing.T, rendered Rendered, key string, value any) {
	t.Helper()
	resource := envExternalSecret(t, rendered)
	entries, _, err := unstructured.NestedSlice(resource, "spec", "dataFrom")
	if err != nil {
		t.Fatal(err)
	}
	entries[0].(map[string]any)["extract"].(map[string]any)[key] = value
	setManifestField(t, resource, entries, "spec", "dataFrom")
}
