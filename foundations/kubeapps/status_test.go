package kubeapps

import (
	"context"
	"testing"

	"github.com/matryer/is"
)

// TestDeploymentStatusRequiresWholeRollout verifies healthy old replicas cannot hide an incomplete update.
func TestDeploymentStatusRequiresWholeRollout(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
		state  string
	}{
		{name: "ready", state: "succeeded", change: func(map[string]any) {}},
		{name: "old healthy replicas", state: "running", change: func(live map[string]any) { live["status"].(map[string]any)["updatedReplicas"] = int64(0) }},
		{name: "old replicas remain", state: "running", change: func(live map[string]any) { live["status"].(map[string]any)["replicas"] = int64(2) }},
		{name: "controller has not observed revision", state: "running", change: func(live map[string]any) { live["metadata"].(map[string]any)["generation"] = int64(3) }},
		{name: "manual image", state: "running", change: func(live map[string]any) { managedContainer(live)["image"] = "frontend:old" }},
		{name: "missing app container", state: "running", change: func(live map[string]any) { managedContainer(live)["name"] = "injected-sidecar" }},
		{name: "manual scale", state: "running", change: func(live map[string]any) { live["spec"].(map[string]any)["replicas"] = int64(2) }},
		{name: "paused", state: "failed", change: func(live map[string]any) { live["spec"].(map[string]any)["paused"] = true }},
		{name: "deadline", state: "failed", change: func(live map[string]any) {
			live["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded", "message": "never became ready"}}
		}},
		{name: "transient failure", state: "running", change: func(live map[string]any) {
			live["status"].(map[string]any)["updatedReplicas"] = int64(0)
			live["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "ReplicaFailure", "status": "True", "message": "retrying temporary image error"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			live := readyDeployment()
			test.change(live)
			client, _ := testClient(live, readyReplicaSet(live))
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			is.NoErr(err)
			is.Equal(status.State, test.state)
			is.True(status.Resources[0].Message != "")
		})
	}
}

// TestStatusChecksReplicaSets verifies the applied ReplicaSet must be ready and older replicas gone.
func TestStatusChecksReplicaSets(t *testing.T) {
	is := is.New(t)
	live := readyDeployment()
	managedContainer(live)["imagePullPolicy"] = "Always"
	containers := valueAt(live, "spec", "template", "spec", "containers").([]any)
	valueAt(live, "spec", "template", "spec").(map[string]any)["containers"] = append([]any{map[string]any{"name": "injected-sidecar", "image": "proxy:v1"}}, containers...)
	newSet := readyReplicaSet(live)
	client, _ := testClient(live, newSet)
	status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	is.NoErr(err)
	is.Equal(status.State, "succeeded")
	is.Equal(status.Resources[0].State, "succeeded")
	oldSet := readyReplicaSet(live)
	oldSet["metadata"].(map[string]any)["name"] = "frontend-old"
	managedContainer(oldSet)["image"] = "frontend:old"
	client, _ = testClient(live, newSet, oldSet)
	status, err = client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	is.NoErr(err)
	is.Equal(status.State, "running")
	is.Equal(status.Resources[0].Message, "waiting for old replica sets to scale down")
	client, _ = testClient(live)
	status, err = client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	is.NoErr(err)
	is.Equal(status.State, "running")
}

// TestStatusAcceptsEmptyEnvironmentValues covers the API's omission of empty
// EnvVar values without accepting changed values in Deployment or ReplicaSet templates.
func TestStatusAcceptsEmptyEnvironmentValues(t *testing.T) {
	assert := is.New(t)
	desired := deploymentManifest()
	managedContainer(desired)["env"] = []any{map[string]any{"name": "EMPTY_TEXT", "value": ""}}
	live := readyDeployment()
	managedContainer(live)["env"] = []any{map[string]any{"name": "EMPTY_TEXT"}}
	currentSet := readyReplicaSet(live)
	client, _ := testClient(live, currentSet)
	status, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "succeeded")
	assert.True(valueAt(managedContainer(live)["env"].([]any)[0].(map[string]any), "value") == nil)

	managedContainer(currentSet)["env"] = []any{map[string]any{"name": "EMPTY_TEXT", "value": "changed"}}
	client, _ = testClient(live, currentSet)
	status, err = client.Status(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "running")
	managedContainer(currentSet)["env"] = []any{map[string]any{"name": "EMPTY_TEXT"}}
	managedContainer(live)["env"] = []any{map[string]any{"name": "EMPTY_TEXT", "value": "changed"}}
	client, _ = testClient(live, currentSet)
	status, err = client.Status(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "running")
	managedContainer(live)["env"] = []any{map[string]any{"name": "EMPTY_TEXT", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "secret", "key": "key"}}}}
	client, _ = testClient(live, currentSet)
	status, err = client.Status(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "running")
}

// TestStatusMissingAndForeignResources checks resource presence, ownership, and
// Service configuration and address allocation before declaring readiness.
func TestStatusMissingAndForeignResources(t *testing.T) {
	is := is.New(t)
	client, _ := testClient()
	status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	is.NoErr(err)
	is.Equal(status.State, "running")
	live := deploymentManifest()
	live["metadata"].(map[string]any)["labels"] = map[string]any{targetLabel: "foreign"}
	client, _ = testClient(live)
	status, err = client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	is.NoErr(err)
	is.Equal(status.State, "failed")
	_, err = client.Status(context.Background(), []map[string]any{deploymentManifest()}, "")
	is.True(err != nil)
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": copyValue(deploymentManifest()["metadata"]), "spec": map[string]any{"type": "ClusterIP", "selector": map[string]any{"app": "frontend"}}}
	for _, test := range []struct{ address, selector, state string }{
		{"", "frontend", "running"},
		{"10.0.0.1", "frontend", "succeeded"},
		{"10.0.0.1", "wrong-app", "running"},
	} {
		live := copyMap(service)
		live["spec"].(map[string]any)["clusterIP"] = test.address
		live["spec"].(map[string]any)["selector"] = map[string]any{"app": test.selector}
		client, _ := testClient(live)
		status, err := client.Status(context.Background(), []map[string]any{service}, "target-one")
		is.NoErr(err)
		is.Equal(status.State, test.state)
	}
}

func readyDeployment() map[string]any {
	live := deploymentManifest()
	live["metadata"].(map[string]any)["uid"] = "deployment-one"
	live["metadata"].(map[string]any)["generation"] = int64(2)
	live["status"] = map[string]any{
		"observedGeneration": int64(2), "replicas": int64(1), "updatedReplicas": int64(1),
		"availableReplicas": int64(1), "readyReplicas": int64(1),
	}
	return live
}

func readyReplicaSet(deployment map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"metadata": map[string]any{
			"name": "frontend-current", "namespace": "shop",
			"ownerReferences": []any{map[string]any{"kind": "Deployment", "name": "frontend", "uid": textAt(deployment, "metadata", "uid"), "controller": true}},
		},
		"spec":   map[string]any{"replicas": int64(1), "template": copyValue(valueAt(deployment, "spec", "template"))},
		"status": map[string]any{"replicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1)},
	}
}
