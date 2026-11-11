package kubeapps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestJobPlanningDoesNotRun verifies dry-run Job creation cannot start migration pods.
func TestJobPlanningDoesNotRun(t *testing.T) {
	assert := is.New(t)
	manifest := predeployManifest()
	client, fakeClient := testClient(namespaceManifest())
	diffs, err := client.Observe(context.Background(), []map[string]any{manifest}, "target-one")
	assert.NoErr(err)
	assert.Equal(diffs[0].Action, "create")
	_, err = fakeClient.Tracker().Get(jobResource(), "shop", "frontend-migrate-plan-one")
	assert.True(apierrors.IsNotFound(err))
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "create" {
			assert.Equal(action.(clienttesting.CreateActionImpl).GetCreateOptions().DryRun, []string{metav1.DryRunAll})
		}
		assert.True(action.GetVerb() != "patch")
	}
}

// TestStartJobIsIdempotent verifies completed migrations are retained after recovery
// and their immutable templates are never patched through generic Apply.
func TestStartJobIsIdempotent(t *testing.T) {
	assert := is.New(t)
	manifest := predeployManifest()
	client, fakeClient := testClient(namespaceManifest())
	assert.NoErr(client.StartJob(context.Background(), manifest, "target-one"))
	live, err := client.dynamic.Resource(jobResource()).Namespace("shop").Get(context.Background(), "frontend-migrate-plan-one", metav1.GetOptions{})
	assert.NoErr(err)
	live.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	// Kubernetes injects selector labels and pod defaults which are outside the saved specification.
	live.Object["spec"].(map[string]any)["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": "generated"}}
	valueAt(live.Object, "spec", "template", "metadata", "labels").(map[string]any)["batch.kubernetes.io/controller-uid"] = "generated"
	assert.NoErr(fakeClient.Tracker().Update(jobResource(), live, "shop"))
	fakeClient.ClearActions()
	assert.NoErr(client.StartJob(context.Background(), manifest, "target-one"))
	assert.NoErr(client.Apply(context.Background(), []map[string]any{manifest}, "target-one"))
	status, err := client.JobStatus(context.Background(), manifest, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "succeeded")
	for _, action := range fakeClient.Actions() {
		assert.Equal(action.GetVerb(), "get")
	}
}

// TestStartJobRejectsChangedOrForeignJobs prevents retries from adopting a new
// owner or treating a manually changed migration as the planned migration.
func TestStartJobRejectsChangedOrForeignJobs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
		owner  bool
	}{
		{name: "foreign owner", owner: true, change: func(live map[string]any) {
			live["metadata"].(map[string]any)["labels"].(map[string]any)[targetLabel] = "other-target"
		}},
		{name: "changed image", change: func(live map[string]any) {
			valueAt(live, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["image"] = "nginx:changed"
		}},
		{name: "changed timeout", change: func(live map[string]any) { live["spec"].(map[string]any)["activeDeadlineSeconds"] = int64(10) }},
		{name: "extra args", change: func(live map[string]any) {
			container := valueAt(live, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
			container["args"] = append(container["args"].([]any), "extra-argument")
		}},
		{name: "changed argument", change: func(live map[string]any) {
			valueAt(live, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"] = []any{"exit 17"}
		}},
		{name: "missing arguments", change: func(live map[string]any) {
			delete(valueAt(live, "spec", "template", "spec", "containers").([]any)[0].(map[string]any), "args")
		}},
		{name: "extra env", change: func(live map[string]any) {
			container := valueAt(live, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
			container["env"] = []any{map[string]any{"name": "MANUAL", "value": "changed"}}
		}},
		{name: "wrong phase", change: func(live map[string]any) {
			live["metadata"].(map[string]any)["labels"].(map[string]any)["borq.run/phase"] = "other-phase"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := predeployManifest()
			live := copyMap(desired)
			test.change(live)
			client, fakeClient := testClient(namespaceManifest(), live)
			err := client.StartJob(context.Background(), desired, "target-one")
			assert.True(err != nil)
			assert.Equal(errors.Is(err, ErrOwnershipConflict), test.owner)
			_, err = client.Observe(context.Background(), []map[string]any{desired}, "target-one")
			assert.True(err != nil)
			status, err := client.JobStatus(context.Background(), desired, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "failed")
			for _, action := range fakeClient.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
}

// TestStartJobHandlesCreationRace verifies concurrent recovery only reuses an
// already created Job after checking its owner and saved specification.
func TestStartJobHandlesCreationRace(t *testing.T) {
	assert := is.New(t)
	desired := predeployManifest()
	client, fakeClient := testClient(namespaceManifest())
	fakeClient.PrependReactor("create", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		assert.NoErr(fakeClient.Tracker().Create(jobResource(), &unstructured.Unstructured{Object: copyMap(desired)}, "shop"))
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, textAt(desired, "metadata", "name"))
	})
	assert.NoErr(client.StartJob(context.Background(), desired, "target-one"))
}

// TestStopJobSuspendsOnlyUnfinishedOwnedJobs verifies failed rollout cleanup
// stops active migration pods without changing successful migration history.
func TestStopJobSuspendsOnlyUnfinishedOwnedJobs(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
	}{
		{name: "active"},
		{name: "complete", state: "Complete"},
		{name: "failed", state: "Failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := predeployManifest()
			live := copyMap(desired)
			if test.state != "" {
				live["status"] = map[string]any{"conditions": []any{map[string]any{"type": test.state, "status": "True"}}}
			}
			client, fakeClient := testClient(live)
			assert.NoErr(client.StopJob(context.Background(), desired, "target-one"))
			current, err := client.dynamic.Resource(jobResource()).Namespace("shop").Get(context.Background(), "frontend-migrate-plan-one", metav1.GetOptions{})
			assert.NoErr(err)
			suspended, _ := valueAt(current.Object, "spec", "suspend").(bool)
			assert.Equal(suspended, test.state == "")
			fakeClient.ClearActions()
			assert.NoErr(client.StopJob(context.Background(), desired, "target-one"))
			for _, action := range fakeClient.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
	assert := is.New(t)
	client, _ := testClient()
	assert.NoErr(client.StopJob(context.Background(), predeployManifest(), "target-one"))
	foreign := predeployManifest()
	foreign["metadata"].(map[string]any)["labels"].(map[string]any)[targetLabel] = "foreign"
	client, _ = testClient(foreign)
	assert.True(errors.Is(client.StopJob(context.Background(), predeployManifest(), "target-one"), ErrOwnershipConflict))
	assert.True(client.StopJob(context.Background(), deploymentManifest(), "target-one") != nil)
}

// TestJobStatusUsesTerminalConditions verifies a running web server blocks the
// rollout, failed pods wait for the Job controller, and terminal failure wins.
func TestJobStatusUsesTerminalConditions(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     map[string]any
		suspended  bool
		state      string
		diagnostic string
	}{
		{name: "pending", state: "running", diagnostic: "Job controller"},
		{name: "running server", status: map[string]any{"active": int64(1)}, state: "running", diagnostic: "complete"},
		{name: "completed", status: map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}, state: "succeeded", diagnostic: "successfully"},
		{name: "failed", status: map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "message": "migration exited 1"}}}, state: "failed", diagnostic: "BackoffLimitExceeded: migration exited 1"},
		{name: "failure wins", status: map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}, map[string]any{"type": "Failed", "status": "True"}}}, state: "failed", diagnostic: "Job failed"},
		{name: "deadline", status: map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "DeadlineExceeded"}}}, state: "failed", diagnostic: "DeadlineExceeded"},
		{name: "terminating failed pods", status: map[string]any{"conditions": []any{map[string]any{"type": "FailureTarget", "status": "True", "message": "migration failed"}}}, state: "running", diagnostic: "failed Job pods to stop"},
		{name: "suspended", suspended: true, state: "failed", diagnostic: "suspended"},
		{name: "failed condition false", status: map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "False"}}}, state: "running", diagnostic: "Job controller"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := predeployManifest()
			live := copyMap(desired)
			live["status"] = test.status
			if test.suspended {
				live["spec"].(map[string]any)["suspend"] = true
			}
			client, _ := testClient(live)
			status, err := client.JobStatus(context.Background(), desired, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, test.state)
			assert.True(strings.Contains(status.Message, test.diagnostic))
			all, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
			assert.NoErr(err)
			assert.Equal(all.State, test.state)
		})
	}
}

// TestJobStatusMissingAndValidation checks absent Jobs stay pending and only
// explicitly labelled predeploy Jobs can be created through this API.
func TestJobStatusMissingAndValidation(t *testing.T) {
	assert := is.New(t)
	client, _ := testClient()
	status, err := client.JobStatus(context.Background(), predeployManifest(), "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "running")
	for _, manifest := range []map[string]any{deploymentManifest(), {"apiVersion": "batch/v1", "kind": "Job"}} {
		assert.True(client.StartJob(context.Background(), manifest, "target-one") != nil)
		_, err := client.JobStatus(context.Background(), manifest, "target-one")
		assert.True(err != nil)
	}
	desired := predeployManifest()
	delete(desired["metadata"].(map[string]any)["labels"].(map[string]any), "borq.run/phase")
	assert.True(client.StartJob(context.Background(), desired, "target-one") != nil)
}

func predeployManifest() map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "frontend-migrate-plan-one", "namespace": "shop", "labels": map[string]any{targetLabel: "target-one", "borq.run/phase": "predeploy"}},
		"spec": map[string]any{
			"activeDeadlineSeconds": int64(300), "backoffLimit": int64(0), "completions": int64(1), "parallelism": int64(1),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{targetLabel: "target-one", "borq.run/phase": "predeploy"}},
				"spec":     map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "migrate", "image": "nginx:1.27.5", "command": []any{"/bin/sh", "-c"}, "args": []any{"exit 0"}}}},
			},
		},
	}
}

func jobResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
}
