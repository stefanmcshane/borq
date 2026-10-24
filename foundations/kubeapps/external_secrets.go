package kubeapps

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (client Client) externalSecretStatus(ctx context.Context, live, desired map[string]any) (string, string, error) {
	if !matchesDesired(live["spec"], desired["spec"]) {
		return "running", "external secret configuration does not match the applied revision", nil
	}
	if !externalSecretSourcesMatch(live, desired) {
		return "running", "external secret sources do not match the pinned environment revision", nil
	}
	ready := false
	generation := integerAt(live, "metadata", "generation")
	if valueAt(live, "status", "observedGeneration") != nil && integerAt(live, "status", "observedGeneration") < generation {
		return "running", "waiting for the external secret controller to observe this revision", nil
	}
	conditions, _ := valueAt(live, "status", "conditions").([]any)
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") != "Ready" || textAt(condition, "status") != "True" {
			continue
		}
		// ESO versions without observedGeneration are additionally guarded by
		// the immutable target name, version annotations, owner, and exact keys.
		if valueAt(condition, "observedGeneration") == nil || integerAt(condition, "observedGeneration") >= generation {
			ready = true
		}
	}
	if !ready {
		return "running", "waiting for the external secret controller to synchronize this revision", nil
	}
	keys, err := externalSecretKeys(desired)
	if err != nil {
		return "", "", err
	}
	namespace := textAt(desired, "metadata", "namespace")
	secretName := textAt(desired, "spec", "target", "name")
	if secretName == "" {
		secretName = textAt(desired, "metadata", "name")
	}
	resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secret, err := client.dynamic.Resource(resource).Namespace(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "running", "waiting for the synchronized environment Secret", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read synchronized environment Secret %s/%s: %w", namespace, secretName, err)
	}
	if err := checkOwnership(secret.Object, textAt(desired, "metadata", "labels", targetLabel)); err != nil {
		return "failed", err.Error(), nil
	}
	if !ownedByExternalSecret(secret.Object, live) {
		return "running", "waiting for an environment Secret owned by this external secret", nil
	}
	if textAt(secret.Object, "type") != "Opaque" || valueAt(secret.Object, "immutable") != true {
		return "running", "waiting for an immutable environment Secret", nil
	}
	metadata := valueAt(desired, "spec", "target", "template", "metadata")
	if !matchesDesired(valueAt(secret.Object, "metadata"), metadata) {
		return "running", "environment Secret identity does not match the applied revision", nil
	}
	data, _ := valueAt(secret.Object, "data").(map[string]any)
	if len(data) != len(keys) {
		return "running", "environment Secret keys do not match the applied revision", nil
	}
	for _, key := range keys {
		if _, exists := data[key]; !exists {
			return "running", "environment Secret keys do not match the applied revision", nil
		}
	}
	return "succeeded", "pinned environment values are synchronized into an immutable Secret", nil
}

func externalSecretSourcesMatch(live, desired map[string]any) bool {
	actual, _ := valueAt(live, "spec", "dataFrom").([]any)
	expected, _ := valueAt(desired, "spec", "dataFrom").([]any)
	if len(actual) != len(expected) {
		return false
	}
	for _, item := range actual {
		source, _ := item.(map[string]any)
		if source["sourceRef"] != nil || source["find"] != nil || source["rewrite"] != nil {
			return false
		}
	}
	data, _ := valueAt(live, "spec", "data").([]any)
	templates, _ := valueAt(live, "spec", "target", "template", "templateFrom").([]any)
	templateData, _ := valueAt(live, "spec", "target", "template", "data").(map[string]any)
	return len(data) == 0 && len(templates) == 0 && len(templateData) == 0
}

func externalSecretKeys(desired map[string]any) ([]string, error) {
	annotations, _ := valueAt(desired, "spec", "target", "template", "metadata", "annotations").(map[string]any)
	version := textAt(annotations, "borq.run/env-version")
	number, err := strconv.ParseUint(version, 10, 64)
	if err != nil || number == 0 {
		return nil, fmt.Errorf("environment ExternalSecret requires a pinned positive KV version")
	}
	sources, _ := valueAt(desired, "spec", "dataFrom").([]any)
	if len(sources) != 1 {
		return nil, fmt.Errorf("environment ExternalSecret requires one pinned KV document")
	}
	source, _ := sources[0].(map[string]any)
	path := textAt(annotations, "borq.run/env-path")
	if path == "" || textAt(source, "extract", "key") != path || textAt(source, "extract", "version") != version || textAt(source, "extract", "property") != "values" {
		return nil, fmt.Errorf("environment ExternalSecret KV reference must match its pinned identity")
	}
	var keys []string
	if err := json.Unmarshal([]byte(textAt(annotations, "borq.run/env-keys")), &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("environment ExternalSecret requires its expected environment keys")
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			return nil, fmt.Errorf("environment ExternalSecret keys must be nonempty and unique")
		}
		seen[key] = true
	}
	return keys, nil
}

func ownedByExternalSecret(secret, externalSecret map[string]any) bool {
	uid := textAt(externalSecret, "metadata", "uid")
	if uid == "" {
		return false
	}
	owners, _ := valueAt(secret, "metadata", "ownerReferences").([]any)
	for _, item := range owners {
		owner, _ := item.(map[string]any)
		if textAt(owner, "apiVersion") == "external-secrets.io/v1" && textAt(owner, "kind") == "ExternalSecret" &&
			textAt(owner, "name") == textAt(externalSecret, "metadata", "name") && textAt(owner, "uid") == uid && valueAt(owner, "controller") == true {
			return true
		}
	}
	return false
}
