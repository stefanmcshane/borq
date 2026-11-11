package kubeapps

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// EnsureSqliteState creates initialization authorization once and never resets an existing database's state.
func (client Client) EnsureSqliteState(ctx context.Context, namespace, application, targetID string, initialize bool) error {
	handle := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(namespace)
	name := application + "-sqlite-state"
	existing, err := handle.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return checkOwnership(existing.Object, targetID)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read SQLite initialization state: %w", err)
	}
	state := "initialized"
	if initialize {
		state = "new"
	}
	// Kubernetes Create accepts a pointer; an existing authorization is never applied or overwritten.
	_, err = handle.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{targetLabel: targetID, "app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": application, "borq.run/phase": "sqlite"}},
		"data":     map[string]any{"state": state},
	}}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, err = handle.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return checkOwnership(existing.Object, targetID)
		}
	}
	return err
}

func removeSqliteConfiguration(result, desired map[string]any) {
	wanted, _ := valueAt(desired, "spec", "template", "spec").(map[string]any)
	actual, _ := valueAt(result, "spec", "template", "spec").(map[string]any)
	volumes, _ := wanted["volumes"].([]any)
	sqlite := false
	for _, item := range volumes {
		volume, _ := item.(map[string]any)
		sqlite = sqlite || textAt(volume, "name") == "borq-sqlite"
	}
	if !sqlite {
		return
	}
	for _, field := range []string{"initContainers", "volumes"} {
		live, _ := actual[field].([]any)
		planned, _ := wanted[field].([]any)
		kept := []any{}
		for _, item := range live {
			entry, _ := item.(map[string]any)
			name := textAt(entry, "name")
			owned := field == "initContainers" && (name == "sqlite-prepare" || name == "sqlite-restore" || name == "litestream") || field == "volumes" && (name == "borq-sqlite" || name == "borq-litestream-config" || name == "borq-sqlite-recovery" || name == "borq-sqlite-token")
			if !owned {
				kept = append(kept, item)
				continue
			}
			for _, candidate := range planned {
				desiredEntry, _ := candidate.(map[string]any)
				if textAt(desiredEntry, "name") == name {
					// Replace owned initializers as a whole so removed mounts and
					// credentials cannot survive a strategic merge.
					kept = append(kept, copyMap(desiredEntry))
				}
			}
		}
		actual[field] = kept
	}
}

func (client Client) placeSqliteJob(ctx context.Context, job map[string]any, targetID string) error {
	pod, _ := valueAt(job, "spec", "template", "spec").(map[string]any)
	volumes, _ := pod["volumes"].([]any)
	claim := ""
	for _, item := range volumes {
		volume, _ := item.(map[string]any)
		if textAt(volume, "name") == "borq-sqlite" {
			claim = textAt(volume, "persistentVolumeClaim", "claimName")
		}
	}
	if claim == "" {
		return nil
	}
	pods, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(textAt(job, "metadata", "namespace")).List(ctx, metav1.ListOptions{LabelSelector: targetLabel + "=" + targetID})
	if err != nil {
		return fmt.Errorf("locate SQLite volume consumer: %w", err)
	}
	for _, existing := range pods.Items {
		if textAt(existing.Object, "metadata", "labels", "borq.run/phase") == "predeploy" || textAt(existing.Object, "status", "phase") == "Succeeded" || textAt(existing.Object, "status", "phase") == "Failed" {
			continue
		}
		mounted, _ := valueAt(existing.Object, "spec", "volumes").([]any)
		for _, item := range mounted {
			volume, _ := item.(map[string]any)
			if textAt(volume, "persistentVolumeClaim", "claimName") == claim {
				node := textAt(existing.Object, "spec", "nodeName")
				if node != "" {
					// Match the node's identity, since provider hostname labels can
					// differ. Affinity keeps volume binding under scheduler control.
					pod["affinity"] = map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []any{node}}}}}}}}
					return nil
				}
			}
		}
	}
	return nil
}
