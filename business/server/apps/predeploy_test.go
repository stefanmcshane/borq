package apps

import (
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TestPredeploySelection renders execution only for selected apps while keeping
// the other apps' task configuration available for subsequent updates.
func TestPredeploySelection(t *testing.T) {
	for _, input := range []struct {
		name     string
		selected []string
		jobs     []string
	}{
		{name: "whole space", jobs: []string{"backend", "frontend"}},
		{name: "one app", selected: []string{"backend"}, jobs: []string{"backend"}},
		{name: "no tasks", selected: []string{}, jobs: []string{}},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"/app migrate"}},{"name":"frontend","image":"frontend:v1","predeploy":{"command":"/app verify","timeoutSeconds":601,"backoffLimit":2}}]}`))
			assert.NoErr(err)
			rendered, err := renderWithEnv(configuration, "shop", "prod", "plan-1", "borq", nil, ResourceScope{}, input.selected, ManagementS3{})
			assert.NoErr(err)
			jobs, _ := splitPlanManifests(rendered.Manifests)
			assert.Equal(len(jobs), len(input.jobs))
			for _, manifest := range jobs {
				job := decodeResource[batchv1.Job](t, manifest)
				assert.True(slices.Contains(input.jobs, job.Labels["app.kubernetes.io/name"]))
			}
			inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
			assert.NoErr(err)
			assert.Equal(inspected, configuration)
		})
	}
}

// TestPredeployMerge preserves omitted task settings and permits command clearing
// and a zero retry budget without mutating stored state.
func TestPredeployMerge(t *testing.T) {
	assert := is.New(t)
	current, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"/app migrate","timeoutSeconds":600,"backoffLimit":2}}]}`))
	assert.NoErr(err)
	assert.Equal(current.Apps[0].Predeploy, Predeploy{Command: "/app migrate", TimeoutSeconds: 600, BackoffLimit: 2})
	unchanged, err := Merge(current, Patch{})
	assert.NoErr(err)
	assert.Equal(unchanged, current)
	updated, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v2","predeploy":{"command":"/app verify","backoffLimit":0}}]}`))
	assert.NoErr(err)
	assert.Equal(updated.Apps[0].Predeploy, Predeploy{Command: "/app verify", TimeoutSeconds: 600})
	assert.Equal(current.Apps[0].Predeploy.Command, "/app migrate")
	cleared, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","predeploy":{"command":""}}]}`))
	assert.NoErr(err)
	assert.Equal(cleared.Apps[0].Predeploy, Predeploy{TimeoutSeconds: 600, BackoffLimit: 2})
	empty, err := Merge(current, patchFromJson(t, `{"apps":[{"name":"backend","predeploy":{}}]}`))
	assert.NoErr(err)
	assert.Equal(empty, current)
	defaults, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"/app migrate"}}]}`))
	assert.NoErr(err)
	assert.Equal(defaults.Apps[0].Predeploy, Predeploy{Command: "/app migrate", TimeoutSeconds: 300})
}

// TestPredeployValidation rejects invalid execution budgets and malformed command
// quoting before creating any deployment plan.
func TestPredeployValidation(t *testing.T) {
	for _, task := range []string{
		`{"command":"migrate","timeoutSeconds":0}`, `{"command":"migrate","timeoutSeconds":2147483648}`,
		`{"command":"migrate","backoffLimit":-1}`, `{"command":"migrate","backoffLimit":2147483648}`,
		`{"command":"migrate 'unterminated"}`,
	} {
		t.Run(task, func(t *testing.T) {
			assert := is.New(t)
			_, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":`+task+`}]}`))
			assert.True(err != nil)
		})
	}
}

// TestPredeployRenderingRoundTrip verifies Jobs inherit the current parent image
// and env, preserve task overrides on readback, and receive plan-specific names.
func TestPredeployRenderingRoundTrip(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v2","env":{"name":"backend-env","version":"7"},"command":"/app serve","predeploy":{"command":"/app migrate","timeoutSeconds":600,"backoffLimit":2}}]}`))
	assert.NoErr(err)
	first, err := RenderForPlan(configuration, "shop", "prod", "plan-1", "borq")
	assert.NoErr(err)
	assert.Equal(first.Values["planID"], "plan-1")
	jobManifest := predeployManifest(t, first)
	job := decodeResource[batchv1.Job](t, jobManifest)
	assert.True(len(job.Name) <= 63)
	assert.True(strings.HasSuffix(job.Name, "-pre"))
	assert.Equal(job.Labels["borq.run/phase"], "predeploy")
	assert.Equal(job.Labels["app.kubernetes.io/name"], "backend")
	assert.Equal(job.Spec.Template.Labels["borq.run/app-name"], "backend")
	assert.Equal(job.Spec.Template.Labels["app.kubernetes.io/name"], "")
	assert.Equal(job.Spec.Template.Labels["borq.run/predeploy-name"], "pre")
	assert.Equal(job.Spec.Template.Spec.RestartPolicy, corev1.RestartPolicyNever)
	assert.Equal(*job.Spec.ActiveDeadlineSeconds, int64(600))
	assert.Equal(*job.Spec.BackoffLimit, int32(2))
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(container.Image, "backend:v2")
	assert.Equal(len(container.Env), 0)
	assert.Equal(container.EnvFrom, []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: EnvSecretName(first.EnvSnapshots[0])}}}})
	assert.Equal(container.Command, []string{"/app", "migrate"})
	assert.Equal(len(container.Args), 0)
	assert.True(container.ReadinessProbe == nil && container.StartupProbe == nil)
	inspected, err := InspectWithEnv(first.Manifests, first.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	second, err := RenderForPlan(configuration, "shop", "prod", "plan-2", "borq")
	assert.NoErr(err)
	assert.True(job.Name != decodeResource[batchv1.Job](t, predeployManifest(t, second)).Name)
	again, err := RenderForPlan(configuration, "shop", "prod", "plan-1", "borq")
	assert.NoErr(err)
	assert.Equal(first, again)
	_, err = InspectWithEnv(append(first.Manifests, predeployManifest(t, second)), first.EnvSnapshots)
	assert.True(err != nil && strings.Contains(err.Error(), "duplicate predeploy"))
	_, err = RenderForPlan(configuration, "shop", "prod", "", "borq")
	assert.True(err != nil)
}

// TestPredeployLongNames keeps maximum-length app names safe for Kubernetes and
// preserves distinct names even when app prefixes share every visible character.
func TestPredeployLongNames(t *testing.T) {
	assert := is.New(t)
	prefix := strings.Repeat("a", 49)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"`+prefix+`a","image":"backend:v1","predeploy":{"command":"migrate"}},{"name":"`+prefix+`b","image":"backend:v1","predeploy":{"command":"migrate"}}]}`))
	assert.NoErr(err)
	rendered, err := RenderForPlan(configuration, "shop", "prod", "plan-1", "borq")
	assert.NoErr(err)
	jobNames := map[string]bool{}
	for _, manifest := range rendered.Manifests {
		if manifest["kind"] != "Job" {
			continue
		}
		job := decodeResource[batchv1.Job](t, manifest)
		assert.Equal(len(validation.IsDNS1123Label(job.Name)), 0)
		assert.True(strings.HasSuffix(job.Name, "-pre"))
		assert.True(!jobNames[job.Name])
		jobNames[job.Name] = true
	}
	assert.Equal(len(jobNames), 2)
	inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
}

// TestRenderWithoutPredeployKeepsPlanIndependentValues prevents plan identity
// from changing application-only rendering or producing a misleading diff.
func TestRenderWithoutPredeployKeepsPlanIndependentValues(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1"}]}`))
	assert.NoErr(err)
	first, err := RenderForPlan(configuration, "shop", "prod", "plan-1", "borq")
	assert.NoErr(err)
	second, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	assert.Equal(first, second)
	_, hasPlanID := first.Values["planID"]
	assert.True(!hasPlanID)
}

// TestDisabledPredeployDoesNotRender leaves task settings stored without
// creating a Job when command is absent or explicitly cleared.
func TestDisabledPredeployDoesNotRender(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"migrate","timeoutSeconds":600}}]}`))
	assert.NoErr(err)
	cleared, err := Merge(configuration, patchFromJson(t, `{"apps":[{"name":"backend","predeploy":{"command":""}}]}`))
	assert.NoErr(err)
	rendered, err := Render(cleared, "shop", "production")
	assert.NoErr(err)
	assert.Equal(len(rendered.Manifests), 1)
	assert.Equal(rendered.Manifests[0]["kind"], "Deployment")
	inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected, cleared)
}

// TestInspectPredeployDrift reads actual Job commands and budgets, but rejects
// changes the inherited-image/environment task model cannot represent.
func TestInspectPredeployDrift(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"foreign phase", func(job map[string]any) { setManifestField(t, job, "other", "metadata", "labels", "borq.run/phase") }},
		{"application Service selector", func(job map[string]any) {
			setManifestField(t, job, "backend", "spec", "template", "metadata", "labels", "app.kubernetes.io/name")
		}},
		{"missing deadline", func(job map[string]any) { delete(job["spec"].(map[string]any), "activeDeadlineSeconds") }},
		{"restart policy", func(job map[string]any) {
			setManifestField(t, job, "Always", "spec", "template", "spec", "restartPolicy")
		}},
		{"different image", func(job map[string]any) { mutatePredeployContainer(t, job, "image", "other:v1") }},
		{"different env", func(job map[string]any) {
			mutatePredeployContainer(t, job, "env", []any{map[string]any{"name": "OTHER", "value": "value"}})
		}},
		{"unsupported probe", func(job map[string]any) {
			mutatePredeployContainer(t, job, "readinessProbe", map[string]any{"httpGet": map[string]any{"path": "/", "port": int64(80)}})
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := is.New(t)
			configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"migrate"}}]}`))
			assert.NoErr(err)
			rendered, err := Render(configuration, "shop", "prod")
			assert.NoErr(err)
			scenario.change(predeployManifest(t, rendered))
			_, err = InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
			assert.True(err != nil)
		})
	}
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","predeploy":{"command":"migrate"}}]}`))
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "prod")
	assert.NoErr(err)
	job := predeployManifest(t, rendered)
	setManifestField(t, job, int64(19), "spec", "activeDeadlineSeconds")
	mutatePredeployContainer(t, job, "command", []any{"/migrate"})
	// Actual Job settings win over retained settings from an earlier partial plan.
	setManifestField(t, findManifest(t, rendered, "Deployment", "backend"), `{"command":"old-task","timeoutSeconds":300}`, "metadata", "annotations", "borq.run/retained-predeploy-v1")
	inspected, err := InspectWithEnv(rendered.Manifests, rendered.EnvSnapshots)
	assert.NoErr(err)
	assert.Equal(inspected.Apps[0].Predeploy.Command, "/migrate")
	assert.Equal(inspected.Apps[0].Predeploy.TimeoutSeconds, 19)
}

func predeployManifest(t *testing.T, rendered Rendered) map[string]any {
	t.Helper()
	for _, manifest := range rendered.Manifests {
		if manifest["kind"] == "Job" {
			return manifest
		}
	}
	t.Fatal("missing predeploy Job")
	return nil
}

func mutatePredeployContainer(t *testing.T, job map[string]any, field string, value any) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(job, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	containers[0].(map[string]any)[field] = value
	setManifestField(t, job, containers, "spec", "template", "spec", "containers")
}
