package kubeapps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Canonical copies a resource and removes controller status and runtime bookkeeping.
// Configuration, including manually added environment variables, remains visible.
func Canonical(manifest map[string]any) map[string]any {
	if manifest == nil {
		return nil
	}
	result := copyMap(manifest)
	delete(result, "status")
	metadata, _ := result["metadata"].(map[string]any)
	for _, field := range []string{"creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "generation", "managedFields", "resourceVersion", "selfLink", "uid"} {
		delete(metadata, field)
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	delete(annotations, "deployment.kubernetes.io/revision")
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	if len(annotations) == 0 {
		delete(metadata, "annotations")
	}
	templateMetadata, _ := valueAt(result, "spec", "template", "metadata").(map[string]any)
	delete(templateMetadata, "creationTimestamp")
	canonicalEnvironmentDefaults(result)
	sortKeyedLists(result, "")
	return result
}

func canonicalEnvironmentDefaults(manifest map[string]any) {
	containers, _ := valueAt(manifest, "spec", "template", "spec", "containers").([]any)
	for _, item := range containers {
		container, _ := item.(map[string]any)
		variables, _ := container["env"].([]any)
		for _, item := range variables {
			variable, _ := item.(map[string]any)
			if variable == nil {
				continue
			}
			// Empty literals and null source selectors are omitted by Kubernetes.
			if value, exists := variable["value"]; exists && value == "" {
				delete(variable, "value")
			}
			source, _ := variable["valueFrom"].(map[string]any)
			if source == nil {
				delete(variable, "valueFrom")
				continue
			}
			for key, value := range source {
				if value == nil {
					delete(source, key)
				}
			}
		}
	}
}

// Fingerprint hashes canonical resource configuration to detect changes between plan and apply.
func Fingerprint(manifest map[string]any) string {
	encoded, _ := json.Marshal(Canonical(manifest))
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func copyMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = copyValue(item)
	}
	return result
}

func copyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copyMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = copyValue(item)
		}
		return result
	default:
		return value
	}
}

func valueAt(value map[string]any, fields ...string) any {
	var current any = value
	for _, field := range fields {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[field]
	}
	return current
}

func textAt(value map[string]any, fields ...string) string {
	result, _ := valueAt(value, fields...).(string)
	return result
}

func integerAt(value map[string]any, fields ...string) int64 {
	switch typed := valueAt(value, fields...).(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	default:
		return 0
	}
}

func sortKeyedLists(value any, field string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			sortKeyedLists(item, key)
		}
	case []any:
		for _, item := range typed {
			sortKeyedLists(item, "")
		}
		if field == "containers" || field == "initContainers" || field == "env" || field == "ports" {
			sort.SliceStable(typed, func(left, right int) bool {
				leftMap, _ := typed[left].(map[string]any)
				rightMap, _ := typed[right].(map[string]any)
				leftName := textAt(leftMap, "name")
				rightName := textAt(rightMap, "name")
				if leftName != rightName {
					return leftName < rightName
				}
				leftJson, _ := json.Marshal(leftMap)
				rightJson, _ := json.Marshal(rightMap)
				return string(leftJson) < string(rightJson)
			})
		}
	}
}

func configurationEqual(left, right map[string]any) bool {
	return Fingerprint(left) == Fingerprint(right)
}
