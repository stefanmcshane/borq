package kubeapps

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// StartJob creates a planned predeploy Job once, or resumes observing the same
// owned Job after a retry or server restart. It never patches or recreates Jobs.
func (client Client) StartJob(ctx context.Context, manifest map[string]any, targetId string) error {
	if err := validateJob(manifest, targetId); err != nil {
		return err
	}
	resource, err := client.resource(manifest)
	if err != nil {
		return err
	}
	name := textAt(manifest, "metadata", "name")
	live, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resource.Create(ctx, &unstructured.Unstructured{Object: copyMap(manifest)}, metav1.CreateOptions{FieldManager: fieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create predeploy Job %s: %w", name, err)
		}
		// Another attempt can win creation during restart recovery. Reuse only
		// the exact owned Job, including completed Jobs, rather than rerun it.
		live, err = resource.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("read predeploy Job %s: %w", name, err)
	}
	return checkJob(live.Object, manifest, targetId)
}

// JobStatus observes terminal Kubernetes Job conditions without changing the
// Job. A running server process remains running until it exits or its deadline.
func (client Client) JobStatus(ctx context.Context, manifest map[string]any, targetId string) (ResourceStatus, error) {
	if err := validateJob(manifest, targetId); err != nil {
		return ResourceStatus{}, err
	}
	return client.resourceStatus(ctx, manifest, targetId)
}

// StopJob suspends an unfinished owned Job after an operation fails or times
// out. Kubernetes terminates its pods asynchronously; completed Jobs stay intact.
func (client Client) StopJob(ctx context.Context, manifest map[string]any, targetId string) error {
	if err := validateJob(manifest, targetId); err != nil {
		return err
	}
	resource, err := client.resource(manifest)
	if err != nil {
		return err
	}
	name := textAt(manifest, "metadata", "name")
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := resource.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read predeploy Job before stopping %s: %w", name, err)
		}
		if err := checkJob(live.Object, manifest, targetId); err != nil {
			return err
		}
		state, _ := jobStatus(live.Object, manifest, targetId)
		if state != "running" {
			return nil
		}
		payload, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": live.GetResourceVersion()},
			"spec":     map[string]any{"suspend": true},
		})
		if err != nil {
			return fmt.Errorf("encode predeploy Job suspension: %w", err)
		}
		_, err = resource.Patch(ctx, name, types.MergePatchType, payload, metav1.PatchOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("stop predeploy Job %s: %w", name, err)
		}
		return nil
	})
}

func validateJob(manifest map[string]any, targetId string) error {
	if err := validateManifest(manifest, targetId); err != nil {
		return err
	}
	if textAt(manifest, "kind") != "Job" {
		return fmt.Errorf("predeploy resource must be a batch/v1 Job")
	}
	return nil
}

func checkJob(live, desired map[string]any, targetId string) error {
	if err := checkOwnership(live, targetId); err != nil {
		return err
	}
	if textAt(live, "metadata", "labels", "borq.run/phase") != "predeploy" {
		return fmt.Errorf("predeploy Job %s does not carry its planned phase label", textAt(desired, "metadata", "name"))
	}
	if !matchesJobSpec(live, desired) {
		return fmt.Errorf("predeploy Job %s no longer matches its saved plan", textAt(desired, "metadata", "name"))
	}
	return nil
}

func matchesJobSpec(live, desired map[string]any) bool {
	actual, _ := copyValue(live["spec"]).(map[string]any)
	wanted, _ := copyValue(desired["spec"]).(map[string]any)
	if actual == nil || wanted == nil {
		return false
	}
	actualTemplate := actual["template"]
	wantedTemplate := wanted["template"]
	delete(actual, "template")
	delete(wanted, "template")
	if !matchesDesired(actual, wanted) || !matchesPodTemplate(actualTemplate, wantedTemplate) {
		return false
	}
	actualPodTemplate, _ := actualTemplate.(map[string]any)
	wantedPodTemplate, _ := wantedTemplate.(map[string]any)
	actualContainers, _ := valueAt(actualPodTemplate, "spec", "containers").([]any)
	wantedContainers, _ := valueAt(wantedPodTemplate, "spec", "containers").([]any)
	for _, item := range wantedContainers {
		wantedContainer, _ := item.(map[string]any)
		for _, item := range actualContainers {
			actualContainer, _ := item.(map[string]any)
			if textAt(actualContainer, "name") != textAt(wantedContainer, "name") {
				continue
			}
			// Admission defaults and sidecar containers are permitted, but extra
			// arguments or variables change migration behavior and cannot be
			// repaired on an immutable Job template.
			for _, field := range []string{"command", "args", "env"} {
				actualItems, _ := actualContainer[field].([]any)
				wantedItems, _ := wantedContainer[field].([]any)
				if len(actualItems) != len(wantedItems) {
					return false
				}
			}
		}
	}
	return true
}

func jobStatus(live, desired map[string]any, targetId string) (string, string) {
	if err := checkJob(live, desired, targetId); err != nil {
		return "failed", err.Error()
	}
	conditions, _ := valueAt(live, "status", "conditions").([]any)
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "Failed" && textAt(condition, "status") == "True" {
			return "failed", jobConditionMessage(condition, "Job failed")
		}
	}
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "Complete" && textAt(condition, "status") == "True" {
			return "succeeded", "predeploy Job completed successfully"
		}
	}
	if suspended, _ := valueAt(live, "spec", "suspend").(bool); suspended {
		return "failed", "predeploy Job is suspended"
	}
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "FailureTarget" && textAt(condition, "status") == "True" {
			return "running", "waiting for failed Job pods to stop: " + jobConditionMessage(condition, "Job is terminating")
		}
	}
	if integerAt(live, "status", "active") > 0 {
		return "running", "waiting for predeploy Job to complete"
	}
	return "running", "waiting for the Job controller to finish predeploy execution"
}

func jobConditionMessage(condition map[string]any, fallback string) string {
	reason := textAt(condition, "reason")
	message := textAt(condition, "message")
	if reason != "" && message != "" {
		return reason + ": " + message
	}
	if message != "" {
		return message
	}
	if reason != "" {
		return reason
	}
	return fallback
}
