package kubeapps

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (client Client) deploymentDiagnostic(ctx context.Context, deployment, desired map[string]any) (string, error) {
	if paused, _ := valueAt(deployment, "spec", "paused").(bool); paused {
		return "", nil
	}
	if integerAt(deployment, "spec", "replicas") == 0 ||
		integerAt(deployment, "status", "observedGeneration") < integerAt(deployment, "metadata", "generation") ||
		!matchesPodTemplate(valueAt(deployment, "spec", "template"), valueAt(desired, "spec", "template")) {
		return "", nil
	}
	namespace := textAt(deployment, "metadata", "namespace")
	replicaSets, err := client.dynamic.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("read replica sets: %w", err)
	}
	current := map[string]string{}
	for _, replicaSet := range replicaSets.Items {
		if ownedByDeployment(replicaSet.Object, deployment) && replicaSet.GetUID() != "" && integerAt(replicaSet.Object, "spec", "replicas") > 0 &&
			matchesPodTemplate(valueAt(replicaSet.Object, "spec", "template"), valueAt(desired, "spec", "template")) {
			current[string(replicaSet.GetUID())] = replicaSet.GetName()
		}
	}
	if len(current) == 0 {
		return "", nil
	}
	pods, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("read pods: %w", err)
	}
	for _, pod := range pods.Items {
		owned := false
		for _, owner := range pod.GetOwnerReferences() {
			if owner.Controller != nil && *owner.Controller && owner.Kind == "ReplicaSet" && current[string(owner.UID)] == owner.Name && owner.Name != "" {
				owned = true
			}
		}
		if !owned || pod.GetDeletionTimestamp() != nil || textAt(pod.Object, "status", "phase") == "Succeeded" || textAt(pod.Object, "status", "phase") == "Failed" {
			continue
		}
		conditions, _ := valueAt(pod.Object, "status", "conditions").([]any)
		ready := false
		for _, item := range conditions {
			condition, _ := item.(map[string]any)
			if textAt(condition, "type") == "Ready" && textAt(condition, "status") == "True" {
				ready = true
			}
		}
		if ready {
			continue
		}
		message := containerDiagnostic(pod.Object)
		// Events supplement current pod state with missing pull secrets and mount
		// failures. UID checks exclude old pods and unrelated apps in the space.
		warning := client.podWarning(ctx, pod.Object)
		if warning != "" {
			if message != "" {
				message += "; "
			}
			// Kubernetes retains events after their cause changes. Keep the live
			// container failure first and identify event text as historical.
			message += "Last pod warning: " + warning
		}
		if message != "" {
			return "Pod " + pod.GetName() + ": " + message, nil
		}
	}
	return "", nil
}

func containerDiagnostic(pod map[string]any) string {
	for _, field := range []string{"initContainerStatuses", "containerStatuses"} {
		containers, _ := valueAt(pod, "status", field).([]any)
		for _, item := range containers {
			container, _ := item.(map[string]any)
			if valueAt(container, "state", "terminated") != nil && integerAt(container, "state", "terminated", "exitCode") != 0 {
				reason := textAt(container, "state", "terminated", "reason")
				if reason == "" {
					reason = "Terminated"
				}
				message := fmt.Sprintf("container %s · %s: exited with code %d", textAt(container, "name"), reason, integerAt(container, "state", "terminated", "exitCode"))
				if detail := textAt(container, "state", "terminated", "message"); detail != "" {
					message += ": " + detail
				}
				return message
			}
			reason := textAt(container, "state", "waiting", "reason")
			switch reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "CrashLoopBackOff":
				message := textAt(container, "state", "waiting", "message")
				return strings.TrimSuffix(fmt.Sprintf("container %s · %s: %s", textAt(container, "name"), reason, message), ": ")
			}
		}
	}
	return ""
}

func (client Client) podWarning(ctx context.Context, pod map[string]any) string {
	uid := textAt(pod, "metadata", "uid")
	if uid == "" {
		return ""
	}
	allowed := map[string]bool{}
	for _, field := range []string{"initContainerStatuses", "containerStatuses"} {
		containers, _ := valueAt(pod, "status", field).([]any)
		for _, item := range containers {
			container, _ := item.(map[string]any)
			switch textAt(container, "state", "waiting", "reason") {
			case "ErrImagePull", "ImagePullBackOff":
				allowed["FailedToRetrieveImagePullSecret"] = true
			case "ContainerCreating":
				allowed["FailedToRetrieveImagePullSecret"], allowed["FailedMount"], allowed["FailedAttachVolume"] = true, true, true
			}
		}
	}
	conditions, _ := valueAt(pod, "status", "conditions").([]any)
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "PodScheduled" && textAt(condition, "status") == "False" {
			allowed["FailedScheduling"] = true
		}
	}
	if len(allowed) == 0 {
		return ""
	}
	events, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "events"}).Namespace(textAt(pod, "metadata", "namespace")).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + uid})
	if err != nil {
		return "" // Container state remains useful when events have expired or cannot be read.
	}
	message, newest := "", time.Time{}
	for _, event := range events.Items {
		if textAt(event.Object, "involvedObject", "uid") != uid || textAt(event.Object, "involvedObject", "kind") != "Pod" || textAt(event.Object, "type") != "Warning" {
			continue
		}
		if !allowed[textAt(event.Object, "reason")] {
			continue
		}
		observed := time.Time{}
		for _, stamp := range []string{textAt(event.Object, "lastTimestamp"), textAt(event.Object, "eventTime"), textAt(event.Object, "series", "lastObservedTime"), textAt(event.Object, "metadata", "creationTimestamp")} {
			if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil && parsed.After(observed) {
				observed = parsed
			}
		}
		if message == "" || observed.After(newest) {
			message, newest = textAt(event.Object, "reason")+": "+textAt(event.Object, "message"), observed
		}
	}
	return message
}
