package kubeapps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// EnvironmentConsumerInput identifies the owned workload and expected live credential delivery.
type EnvironmentConsumerInput struct {
	// Namespace, AppName and TargetID scope one existing application.
	Namespace, AppName, TargetID string
	// Revision guards against refreshing a different application deployment.
	Revision string
	// Generation identifies the current producer KV version without hashing credential data.
	Generation string
	// PreviousSecret permits the one-time migration from an accepted pinned binding.
	PreviousSecret string
	// Reference is the public live environment selector stored on the deployment.
	Reference string
	// ExternalSecret is the owned live delivery manifest.
	ExternalSecret map[string]any
	// ExpectedValues are transient current producer values, never persisted or logged.
	ExpectedValues map[string]string `json:"-"`
}

// RefreshEnvironmentConsumer migrates and restarts an owned consumer after its live Secret is synchronized.
// Repeated calls observe the same producer generation without restarting it again.
func (client Client) RefreshEnvironmentConsumer(ctx context.Context, input EnvironmentConsumerInput) (bool, error) {
	if input.Namespace == "" || input.AppName == "" || input.TargetID == "" || input.Revision == "" || input.Generation == "" || textAt(input.ExternalSecret, "metadata", "annotations", "borq.run/postgres-id") == "" {
		return false, fmt.Errorf("live environment refresh requires an application, revision and managed producer")
	}
	if err := client.Apply(ctx, []map[string]any{input.ExternalSecret}, input.TargetID); err != nil {
		return false, err
	}
	externals := client.dynamic.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}).Namespace(input.Namespace)
	external, err := externals.Get(ctx, textAt(input.ExternalSecret, "metadata", "name"), metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read managed environment delivery: %w", err)
	}
	state, _, err := client.externalSecretStatus(ctx, external.Object, input.ExternalSecret)
	if err != nil || state != "succeeded" {
		return false, err
	}
	secretName := textAt(input.ExternalSecret, "spec", "target", "name")
	secret, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(input.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read managed environment Secret: %w", err)
	}
	values, _ := valueAt(secret.Object, "data").(map[string]any)
	if len(input.ExpectedValues) == 0 || len(values) != len(input.ExpectedValues) {
		return false, nil
	}
	for name, expected := range input.ExpectedValues {
		actual, err := base64.StdEncoding.DecodeString(textAt(values, name))
		if err != nil || string(actual) != expected {
			return false, nil
		}
	}
	generation := input.Generation
	deployments := client.dynamic.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(input.Namespace)
	deployment, err := deployments.Get(ctx, input.AppName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return client.retirePinnedEnvironment(ctx, input, secretName)
	}
	if err != nil {
		return false, fmt.Errorf("read managed environment consumer: %w", err)
	}
	if err := checkOwnership(deployment.Object, input.TargetID); err != nil {
		return false, err
	}
	if textAt(deployment.Object, "spec", "template", "metadata", "labels", "borq.run/revision") != input.Revision {
		return false, fmt.Errorf("application revision changed during managed environment refresh")
	}
	containers, _ := valueAt(deployment.Object, "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 || textAt(containers[0].(map[string]any), "name") != input.AppName {
		return false, fmt.Errorf("managed environment consumer must have its accepted application container")
	}
	container := containers[0].(map[string]any)
	sources, _ := container["envFrom"].([]any)
	if len(sources) != 1 {
		return false, fmt.Errorf("managed environment consumer binding changed")
	}
	currentSecret := textAt(sources[0].(map[string]any), "secretRef", "name")
	if currentSecret != secretName && (input.PreviousSecret == "" || currentSecret != input.PreviousSecret) {
		return false, fmt.Errorf("managed environment consumer references an unexpected Secret")
	}
	if textAt(deployment.Object, "spec", "template", "metadata", "annotations", "borq.run/credential-generation") == generation && currentSecret == secretName {
		state, _ := deploymentStatus(deployment.Object, deployment.Object)
		if state != "succeeded" {
			return false, nil
		}
		return client.retirePinnedEnvironment(ctx, input, secretName)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": deployment.GetResourceVersion(), "uid": deployment.GetUID(), "annotations": map[string]any{"borq.run/environment-group": input.Reference}},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"borq.run/credential-generation": generation}},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": input.AppName, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": secretName}}}}}},
		}},
	})
	if err != nil {
		return false, fmt.Errorf("encode managed environment refresh")
	}
	_, err = deployments.Patch(ctx, input.AppName, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "borq-credentials"})
	if err != nil {
		return false, fmt.Errorf("refresh managed environment consumer: %w", err)
	}
	return false, nil
}

func (client Client) retirePinnedEnvironment(ctx context.Context, input EnvironmentConsumerInput, currentSecret string) (bool, error) {
	if input.PreviousSecret == "" || input.PreviousSecret == currentSecret {
		return true, nil
	}
	pods, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		phase := textAt(pod.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		for _, field := range []string{"containers", "initContainers"} {
			containers, _ := valueAt(pod.Object, "spec", field).([]any)
			for _, value := range containers {
				container, _ := value.(map[string]any)
				sources, _ := container["envFrom"].([]any)
				for _, value := range sources {
					source, _ := value.(map[string]any)
					if textAt(source, "secretRef", "name") == input.PreviousSecret {
						return false, nil
					}
				}
			}
		}
	}
	for _, resource := range []schema.GroupVersionResource{{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}, {Version: "v1", Resource: "secrets"}} {
		handle := client.dynamic.Resource(resource).Namespace(input.Namespace)
		old, err := handle.Get(ctx, input.PreviousSecret, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := checkOwnership(old.Object, input.TargetID); err != nil {
			return false, err
		}
		if old.GetLabels()["app.kubernetes.io/name"] != input.AppName || old.GetAnnotations()["borq.run/environment-group-name"] != textAt(input.ExternalSecret, "metadata", "annotations", "borq.run/environment-group-name") {
			return false, fmt.Errorf("previous environment credential ownership changed")
		}
		uid, revision := old.GetUID(), old.GetResourceVersion()
		if err := handle.Delete(ctx, old.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}
