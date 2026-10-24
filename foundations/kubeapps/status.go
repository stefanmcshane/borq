package kubeapps

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Status observes rollout progress without changing resources or reconciling drift.
func (client Client) Status(ctx context.Context, manifests []map[string]any, targetId string) (RolloutStatus, error) {
	result := RolloutStatus{State: "succeeded", Message: "all resources are ready", Resources: make([]ResourceStatus, 0, len(manifests))}
	for _, desired := range manifests {
		if err := validateManifest(desired, targetId); err != nil {
			return RolloutStatus{}, err
		}
		status, err := client.resourceStatus(ctx, desired, targetId)
		if err != nil {
			return RolloutStatus{}, err
		}
		result.Resources = append(result.Resources, status)
		if status.State == "failed" || status.State == "running" && result.State == "succeeded" {
			result.State = status.State
			result.Message = status.Kind + " " + status.Name + ": " + status.Message
		}
	}
	return result, nil
}

func (client Client) resourceStatus(ctx context.Context, desired map[string]any, targetId string) (ResourceStatus, error) {
	status := ResourceStatus{
		ApiVersion: textAt(desired, "apiVersion"), Kind: textAt(desired, "kind"),
		Namespace: textAt(desired, "metadata", "namespace"), Name: textAt(desired, "metadata", "name"),
		State: "running", Message: "waiting for resource creation",
	}
	resource, err := client.resource(desired)
	if err != nil {
		return ResourceStatus{}, err
	}
	live, err := resource.Get(ctx, status.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return status, nil
	}
	if err != nil {
		return ResourceStatus{}, fmt.Errorf("observe rollout %s %s: %w", status.Kind, status.Name, err)
	}
	if err := checkOwnership(live.Object, targetId); err != nil {
		status.State = "failed"
		status.Message = err.Error()
		return status, nil
	}
	switch status.Kind {
	case "Deployment":
		status.State, status.Message = deploymentStatus(live.Object, desired)
		if status.State == "succeeded" {
			status.State, status.Message, err = client.replicaSetStatus(ctx, live.Object, desired)
			if err != nil {
				return ResourceStatus{}, err
			}
		}
	case "Service":
		status.State, status.Message = serviceStatus(live.Object, desired)
	case "Ingress":
		status.State, status.Message = ingressStatus(live.Object, desired)
		if status.State == "succeeded" && textAt(desired, "metadata", "annotations", "cert-manager.io/cluster-issuer") != "" {
			status.State, status.Message, err = client.ingressTlsStatus(ctx, live.Object, desired)
			if err != nil {
				return ResourceStatus{}, err
			}
		}
	case "Job":
		status.State, status.Message = jobStatus(live.Object, desired, targetId)
	case "ExternalSecret":
		status.State, status.Message, err = client.externalSecretStatus(ctx, live.Object, desired)
		if err != nil {
			return ResourceStatus{}, err
		}
	}
	return status, nil
}

func deploymentStatus(live, desired map[string]any) (string, string) {
	if paused, _ := valueAt(live, "spec", "paused").(bool); paused {
		return "failed", "deployment is paused"
	}
	if integerAt(live, "status", "observedGeneration") < integerAt(live, "metadata", "generation") {
		return "running", "waiting for the deployment controller to observe this revision"
	}
	if !matchesPodTemplate(valueAt(live, "spec", "template"), valueAt(desired, "spec", "template")) {
		return "running", "pod template does not match the applied revision"
	}
	conditions, _ := valueAt(live, "status", "conditions").([]any)
	diagnostic := "waiting for updated replicas to become available"
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "Progressing" && textAt(condition, "status") == "False" && textAt(condition, "reason") == "ProgressDeadlineExceeded" {
			return "failed", "deployment exceeded its progress deadline: " + textAt(condition, "message")
		}
		if textAt(condition, "type") == "ReplicaFailure" && textAt(condition, "status") == "True" {
			diagnostic = textAt(condition, "message")
		}
	}
	replicas := int64(1)
	if valueAt(desired, "spec", "replicas") != nil {
		replicas = integerAt(desired, "spec", "replicas")
	}
	if integerAt(live, "spec", "replicas") != replicas {
		return "running", "replica configuration does not match the applied revision"
	}
	if integerAt(live, "status", "updatedReplicas") != replicas || integerAt(live, "status", "availableReplicas") < replicas || integerAt(live, "status", "readyReplicas") < replicas || integerAt(live, "status", "replicas") != replicas || integerAt(live, "status", "unavailableReplicas") != 0 {
		return "running", diagnostic
	}
	return "succeeded", "all requested replicas are updated and available"
}

func (client Client) replicaSetStatus(ctx context.Context, deployment, desired map[string]any) (string, string, error) {
	resource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	replicaSets, err := client.dynamic.Resource(resource).Namespace(textAt(deployment, "metadata", "namespace")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("read deployment replica sets: %w", err)
	}
	replicas := integerAt(deployment, "spec", "replicas")
	matched := replicas == 0
	for _, replicaSet := range replicaSets.Items {
		if !ownedByDeployment(replicaSet.Object, deployment) {
			continue
		}
		if !matchesPodTemplate(valueAt(replicaSet.Object, "spec", "template"), valueAt(desired, "spec", "template")) {
			if integerAt(replicaSet.Object, "spec", "replicas") > 0 || integerAt(replicaSet.Object, "status", "replicas") > 0 {
				return "running", "waiting for old replica sets to scale down", nil
			}
			continue
		}
		if integerAt(replicaSet.Object, "spec", "replicas") == replicas && integerAt(replicaSet.Object, "status", "readyReplicas") >= replicas && integerAt(replicaSet.Object, "status", "availableReplicas") >= replicas {
			matched = true
		}
	}
	if !matched {
		return "running", "waiting for the applied replica set to become available", nil
	}
	return "succeeded", "applied replica set is available and old replica sets have stopped", nil
}

func ownedByDeployment(replicaSet, deployment map[string]any) bool {
	owners, _ := valueAt(replicaSet, "metadata", "ownerReferences").([]any)
	for _, item := range owners {
		owner, _ := item.(map[string]any)
		controller, _ := owner["controller"].(bool)
		if controller && textAt(owner, "kind") == "Deployment" && textAt(owner, "uid") == textAt(deployment, "metadata", "uid") && textAt(owner, "name") == textAt(deployment, "metadata", "name") {
			return true
		}
	}
	return false
}

func serviceStatus(live, desired map[string]any) (string, string) {
	if !matchesDesired(live["spec"], desired["spec"]) {
		return "running", "service configuration does not match the applied revision"
	}
	if textAt(live, "spec", "type") != "ExternalName" && textAt(live, "spec", "clusterIP") == "" {
		return "running", "waiting for cluster address allocation"
	}
	return "succeeded", "service configuration is accepted and its cluster address is allocated"
}

func ingressStatus(live, desired map[string]any) (string, string) {
	if !matchesDesired(live["spec"], desired["spec"]) {
		return "running", "ingress configuration does not match the applied revision"
	}
	// Traefik's standard Ingress API has no portable accepted condition. Managed
	// HTTPS routes additionally check their Certificate and Secret in resourceStatus.
	return "succeeded", "ingress configuration and service references are accepted by the Kubernetes API"
}

func matchesPodTemplate(live, desired any) bool {
	template, ok := copyValue(live).(map[string]any)
	if !ok {
		return false
	}
	containers, _ := valueAt(template, "spec", "containers").([]any)
	for _, item := range containers {
		container, _ := item.(map[string]any)
		variables, _ := container["env"].([]any)
		for _, item := range variables {
			variable, _ := item.(map[string]any)
			if variable == nil {
				continue
			}
			// Kubernetes omits an empty literal EnvVar.value on reads. Restore
			// only this default; a missing image or another scalar is not equal.
			if _, exists := variable["value"]; !exists && variable["valueFrom"] == nil {
				variable["value"] = ""
			}
		}
	}
	return matchesDesired(template, desired)
}

func matchesDesired(live, desired any) bool {
	switch wanted := desired.(type) {
	case map[string]any:
		actual, ok := live.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range wanted {
			if !matchesDesired(actual[key], value) {
				return false
			}
		}
		return true
	case []any:
		actual, _ := live.([]any)
		if len(wanted) == 0 {
			return len(actual) == 0
		}
		if len(actual) < len(wanted) {
			return false
		}
		for index, item := range wanted {
			object, named := item.(map[string]any)
			if named && textAt(object, "name") != "" {
				found := false
				for _, actualItem := range actual {
					actualObject, _ := actualItem.(map[string]any)
					if textAt(actualObject, "name") == textAt(object, "name") && matchesDesired(actualObject, object) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
				continue
			}
			if !matchesDesired(actual[index], item) {
				return false
			}
		}
		return true
	default:
		left, _ := json.Marshal(live)
		right, _ := json.Marshal(desired)
		return string(left) == string(right)
	}
}
