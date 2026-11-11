package kubeapps

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/matryer/is"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// TestDeploymentBlockingDiagnostics keeps retryable container failures visible
// in rollout readback without turning them into terminal deployment failures.
func TestDeploymentBlockingDiagnostics(t *testing.T) {
	for _, input := range []struct {
		reason, message, field string
	}{
		{"ImagePullBackOff", "failed to authorize: 401 Unauthorized", "containerStatuses"},
		{"ErrImagePull", "failed to pull private image", "initContainerStatuses"},
		{"CreateContainerConfigError", `secret "ghcr" not found`, "containerStatuses"},
		{"CrashLoopBackOff", "back-off restarting failed container", "containerStatuses"},
		{"InvalidImageName", "invalid reference format", "containerStatuses"},
		{"ErrImageNeverPull", "image is not present", "containerStatuses"},
		{"CreateContainerError", "container could not be created", "containerStatuses"},
		{"RunContainerError", "container could not start", "containerStatuses"},
	} {
		t.Run(input.reason+input.field, func(t *testing.T) {
			assert := is.New(t)
			deployment, replicaSet, pod := diagnosticResources(input.reason, input.message)
			if input.field == "initContainerStatuses" {
				pod["status"].(map[string]any)[input.field] = pod["status"].(map[string]any)["containerStatuses"]
				delete(pod["status"].(map[string]any), "containerStatuses")
			}
			client, _ := testClient(deployment, replicaSet, pod)
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			assert.Equal(status.Resources[0].Message, "Pod frontend-pod: container frontend · "+input.reason+": "+input.message)
			assert.True(strings.Contains(status.Message, input.message))
		})
	}
}

// TestDeploymentDeadlineRetainsContainerDiagnostic keeps the blocking cause
// visible after Kubernetes declares its own progress deadline exceeded.
func TestDeploymentDeadlineRetainsContainerDiagnostic(t *testing.T) {
	assert := is.New(t)
	deployment, replicaSet, pod := diagnosticResources("ImagePullBackOff", "401 Unauthorized")
	deployment["status"].(map[string]any)["conditions"] = []any{map[string]any{
		"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded", "message": "replicas did not become ready",
	}}
	client, _ := testClient(deployment, replicaSet, pod)
	status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "failed")
	assert.True(strings.Contains(status.Message, "deployment exceeded its progress deadline"))
	assert.True(strings.Contains(status.Message, "401 Unauthorized"))
}

// TestDeploymentWarningPreservesCurrentContainerError distinguishes a past
// missing secret event from an authentication error that persists after replacement.
func TestDeploymentWarningPreservesCurrentContainerError(t *testing.T) {
	assert := is.New(t)
	deployment, replicaSet, pod := diagnosticResources("ImagePullBackOff", "401 Unauthorized")
	event := diagnosticEvent("old-secret-warning", "FailedToRetrieveImagePullSecret", "unable to retrieve secret ghcr")
	client, _ := testClient(deployment, replicaSet, pod, event)
	status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.Resources[0].Message, "Pod frontend-pod: container frontend · ImagePullBackOff: 401 Unauthorized; Last pod warning: FailedToRetrieveImagePullSecret: unable to retrieve secret ghcr")
}

// TestDeploymentTerminatedContainer reports the current failed exit during the
// interval before Kubernetes changes its state to CrashLoopBackOff.
func TestDeploymentTerminatedContainer(t *testing.T) {
	for _, input := range []struct {
		reason, detail, want string
		code                 int64
	}{
		{reason: "Error", code: 1, want: "Error: exited with code 1"},
		{reason: "OOMKilled", code: 137, detail: "memory limit exceeded", want: "OOMKilled: exited with code 137: memory limit exceeded"},
		{code: 2, want: "Terminated: exited with code 2"},
		{reason: "Completed", code: 0},
	} {
		t.Run(input.reason, func(t *testing.T) {
			assert := is.New(t)
			deployment, replicaSet, pod := diagnosticResources("", "")
			container := valueAt(pod, "status", "containerStatuses").([]any)[0].(map[string]any)
			container["state"] = map[string]any{"terminated": map[string]any{"reason": input.reason, "exitCode": input.code, "message": input.detail}}
			client, _ := testClient(deployment, replicaSet, pod)
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			if input.want == "" {
				assert.Equal(status.Resources[0].Message, "waiting for updated replicas to become available")
				return
			}
			assert.True(strings.Contains(status.Message, input.want))
		})
	}
}

// TestDeploymentWarnings selects current blockers rather than the full event
// stream, and ignores stale events after the pod has moved past that blocker.
func TestDeploymentWarnings(t *testing.T) {
	for _, input := range []struct {
		name, waiting, warning, message string
		scheduled, stale                bool
	}{
		{name: "missing pull secret", waiting: "ContainerCreating", warning: "FailedToRetrieveImagePullSecret", message: "Unable to retrieve image pull secret ghcr"},
		{name: "image pull and missing secret", waiting: "ImagePullBackOff", warning: "FailedToRetrieveImagePullSecret", message: "Unable to retrieve image pull secret ghcr"},
		{name: "missing mounted secret", waiting: "ContainerCreating", warning: "FailedMount", message: `secret "config" not found`},
		{name: "volume attachment", waiting: "ContainerCreating", warning: "FailedAttachVolume", message: "volume is attached elsewhere"},
		{name: "scheduling", scheduled: true, warning: "FailedScheduling", message: "insufficient memory"},
		{name: "normal startup", waiting: "ContainerCreating"},
		{name: "resolved mount", warning: "FailedMount", stale: true},
		{name: "resolved credentials", warning: "FailedToRetrieveImagePullSecret", stale: true},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			deployment, replicaSet, pod := diagnosticResources(input.waiting, "retrying image")
			if input.scheduled {
				pod["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "PodScheduled", "status": "False"}}
			}
			events := []map[string]any{
				diagnosticEvent("scheduled", "Scheduled", "normal scheduling noise"),
				diagnosticEvent("pulling", "Pulling", "normal image pulling noise"),
				diagnosticEvent("selected", input.warning, input.message),
				diagnosticEvent("foreign", "FailedToRetrieveImagePullSecret", "another pod secret"),
				diagnosticEvent("old", input.warning, "older warning"),
			}
			events[0]["type"], events[1]["type"] = "Normal", "Normal"
			events[3]["involvedObject"].(map[string]any)["uid"] = "different-pod-uid"
			events[4]["lastTimestamp"] = "2026-10-30T04:00:00Z"
			manifests := append([]map[string]any{deployment, replicaSet, pod}, events...)
			client, _ := testClient(manifests...)
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			if input.warning == "" || input.stale {
				assert.Equal(status.Resources[0].Message, "waiting for updated replicas to become available")
				return
			}
			assert.True(strings.Contains(status.Resources[0].Message, input.warning+": "+input.message))
			assert.True(!strings.Contains(status.Message, "noise"))
			assert.True(!strings.Contains(status.Message, "another pod"))
			assert.True(!strings.Contains(status.Message, "older warning"))
		})
	}
}

// TestDeploymentDiagnosticsScope prevents previous revisions, unrelated pods,
// and already recovered containers from supplying the current rollout's error.
func TestDeploymentDiagnosticsScope(t *testing.T) {
	for _, input := range []struct {
		name   string
		change func(deployment, replicaSet, pod map[string]any)
	}{
		{"old replica set", func(_, replicaSet, _ map[string]any) { managedContainer(replicaSet)["image"] = "frontend:old" }},
		{"inactive replica set", func(_, replicaSet, _ map[string]any) { replicaSet["spec"].(map[string]any)["replicas"] = int64(0) }},
		{"unrelated deployment", func(_, replicaSet, _ map[string]any) {
			valueAt(replicaSet, "metadata", "ownerReferences").([]any)[0].(map[string]any)["uid"] = "another-deployment"
		}},
		{"replaced replica set", func(_, _, pod map[string]any) {
			valueAt(pod, "metadata", "ownerReferences").([]any)[0].(map[string]any)["uid"] = "another-replica-set"
		}},
		{"non-controller owner", func(_, _, pod map[string]any) {
			valueAt(pod, "metadata", "ownerReferences").([]any)[0].(map[string]any)["controller"] = false
		}},
		{"other namespace", func(_, _, pod map[string]any) { pod["metadata"].(map[string]any)["namespace"] = "another-space" }},
		{"terminating pod", func(_, _, pod map[string]any) {
			pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-30T05:00:00Z"
		}},
		{"ready pod", func(_, _, pod map[string]any) {
			pod["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
		}},
		{"stopped pod", func(_, _, pod map[string]any) { pod["status"].(map[string]any)["phase"] = "Succeeded" }},
		{"failed old pod", func(_, _, pod map[string]any) { pod["status"].(map[string]any)["phase"] = "Failed" }},
		{"controller catching up", func(deployment, _, _ map[string]any) {
			deployment["status"].(map[string]any)["observedGeneration"] = int64(1)
		}},
		{"changed deployment", func(deployment, _, _ map[string]any) { managedContainer(deployment)["image"] = "frontend:other" }},
		{"scaled down", func(deployment, _, _ map[string]any) { deployment["spec"].(map[string]any)["replicas"] = int64(0) }},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			deployment, replicaSet, pod := diagnosticResources("ImagePullBackOff", "private-image-error")
			input.change(deployment, replicaSet, pod)
			client, _ := testClient(deployment, replicaSet, pod)
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			assert.True(!strings.Contains(status.Message, "private-image-error"))
		})
	}
}

// TestDeploymentDiagnosticsUnavailable preserves ordinary readiness information
// when credentials cannot read supplemental pod or event diagnostics.
func TestDeploymentDiagnosticsUnavailable(t *testing.T) {
	for _, resource := range []string{"replicasets", "pods", "events"} {
		t.Run(resource, func(t *testing.T) {
			assert := is.New(t)
			deployment, replicaSet, pod := diagnosticResources("ImagePullBackOff", "401 Unauthorized")
			client, fakeClient := testClient(deployment, replicaSet, pod)
			fakeClient.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("diagnostic access denied")
			})
			status, err := client.Status(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			if resource == "events" {
				assert.True(strings.Contains(status.Message, "401 Unauthorized"))
				return
			}
			assert.True(strings.Contains(status.Message, "waiting for updated replicas"))
			assert.True(strings.Contains(status.Message, "diagnostic access denied"))
		})
	}
}

func diagnosticResources(reason, message string) (map[string]any, map[string]any, map[string]any) {
	deployment := readyDeployment()
	deployment["status"].(map[string]any)["availableReplicas"] = int64(0)
	replicaSet := readyReplicaSet(deployment)
	replicaSet["metadata"].(map[string]any)["uid"] = "replica-set-uid"
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "frontend-pod", "namespace": "shop", "uid": "pod-uid",
			"ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": "frontend-current", "uid": "replica-set-uid", "controller": true}}},
		"status": map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{"name": "frontend", "state": map[string]any{"waiting": map[string]any{"reason": reason, "message": message}}}}},
	}
	return deployment, replicaSet, pod
}

func diagnosticEvent(name, reason, message string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"name": name, "namespace": "shop"},
		"involvedObject": map[string]any{"kind": "Pod", "name": "frontend-pod", "uid": "pod-uid"},
		"type":           "Warning", "reason": reason, "message": message, "lastTimestamp": "2026-10-30T05:00:00Z",
	}
}
