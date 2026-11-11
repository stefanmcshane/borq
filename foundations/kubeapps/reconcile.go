package kubeapps

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

// Observe compares complete desired resources against the live cluster without changing it.
// A missing namespace is permitted so the first deployment can be planned before creation.
func (client Client) Observe(ctx context.Context, manifests []map[string]any, targetId string) ([]ResourceDiff, error) {
	results := make([]ResourceDiff, 0, len(manifests))
	seen := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		if err := validateManifest(manifest, targetId); err != nil {
			return nil, err
		}
		identity := textAt(manifest, "apiVersion") + "/" + textAt(manifest, "kind") + "/" + textAt(manifest, "metadata", "namespace") + "/" + textAt(manifest, "metadata", "name")
		if seen[identity] {
			return nil, fmt.Errorf("duplicate manifest %s", identity)
		}
		seen[identity] = true
		var result ResourceDiff
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			observed, observeError := client.observeResource(ctx, manifest, targetId)
			result = observed
			return observeError
		})
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (client Client) observeResource(ctx context.Context, manifest map[string]any, targetId string) (ResourceDiff, error) {
	result := ResourceDiff{
		ApiVersion: textAt(manifest, "apiVersion"), Kind: textAt(manifest, "kind"),
		Namespace: textAt(manifest, "metadata", "namespace"), Name: textAt(manifest, "metadata", "name"),
		Action: "create", Desired: Canonical(manifest), Fingerprint: Fingerprint(nil),
	}
	resource, err := client.resource(manifest)
	if err != nil {
		return ResourceDiff{}, err
	}
	live, err := resource.Get(ctx, result.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return ResourceDiff{}, fmt.Errorf("read %s %s: %w", result.Kind, result.Name, err)
	}
	if apierrors.IsNotFound(err) {
		_, namespaceError := client.namespaceExists(ctx, result.Namespace)
		if apierrors.IsNotFound(namespaceError) {
			return result, nil
		}
		if namespaceError != nil {
			return ResourceDiff{}, fmt.Errorf("read namespace %s: %w", result.Namespace, namespaceError)
		}
	}
	if live != nil {
		if err := checkOwnership(live.Object, targetId); err != nil {
			return ResourceDiff{}, err
		}
		result.Live = Canonical(live.Object)
		result.Fingerprint = Fingerprint(live.Object)
	}
	if result.Kind == "Job" {
		if live != nil {
			if err := checkJob(live.Object, manifest, targetId); err != nil {
				return ResourceDiff{}, err
			}
			result.Action = "unchanged"
			result.Desired = result.Live
			return result, nil
		}
		preview, err := resource.Create(ctx, &unstructured.Unstructured{Object: copyMap(manifest)}, metav1.CreateOptions{FieldManager: fieldManager, DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			return ResourceDiff{}, fmt.Errorf("preview predeploy Job %s: %w", result.Name, err)
		}
		result.Desired = Canonical(preview.Object)
		return result, nil
	}
	force := true
	patchType, configuration := resourcePatch(manifest, live != nil)
	payload, err := json.Marshal(configuration)
	if err != nil {
		return ResourceDiff{}, fmt.Errorf("encode %s %s: %w", result.Kind, result.Name, err)
	}
	options := metav1.PatchOptions{FieldManager: fieldManager, DryRun: []string{metav1.DryRunAll}}
	if patchType == types.ApplyPatchType {
		options.Force = &force
	}
	preview, err := resource.Patch(ctx, result.Name, patchType, payload, options)
	if err != nil {
		return ResourceDiff{}, fmt.Errorf("preview %s %s: %w", result.Kind, result.Name, err)
	}
	cleaned := copyMap(preview.Object)
	removeUnwantedConfiguration(cleaned, manifest)
	if live != nil && !configurationEqual(cleaned, preview.Object) {
		// Preview the same removal performed before apply, including admission defaults.
		patch := []map[string]any{
			{"op": "replace", "path": "/metadata/resourceVersion", "value": live.GetResourceVersion()},
			{"op": "replace", "path": "/spec", "value": cleaned["spec"]},
			{"op": "add", "path": "/metadata/labels", "value": valueAt(cleaned, "metadata", "labels")},
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			return ResourceDiff{}, fmt.Errorf("encode removal preview: %w", err)
		}
		preview, err = resource.Patch(ctx, result.Name, types.JSONPatchType, encoded, metav1.PatchOptions{FieldManager: fieldManager, DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			return ResourceDiff{}, fmt.Errorf("preview configuration removal for %s %s: %w", result.Kind, result.Name, err)
		}
	}
	result.Desired = Canonical(preview.Object)
	if live == nil {
		return result, nil
	}
	result.Action = "update"
	if configurationEqual(result.Live, result.Desired) {
		result.Action = "unchanged"
	}
	return result, nil
}

// NamespaceInput supplies the saved ownership metadata for a deployment namespace.
type NamespaceInput struct {
	// Name is the Kubernetes namespace selected by the space.
	Name string
	// TargetID prevents assigning a namespace owned by another space.
	TargetID string
	// Labels contains the ownership labels frozen in the deployment plan.
	Labels map[string]string
	// Annotations contains the ownership annotations frozen in the deployment plan.
	Annotations map[string]string
}

// EnsureNamespace creates or updates a target namespace's ownership metadata.
// A namespace owned by a different target is never reassigned.
func (client Client) EnsureNamespace(ctx context.Context, input NamespaceInput) error {
	name, targetId := input.Name, input.TargetID
	if name == "" || targetId == "" {
		return fmt.Errorf("namespace and target ID are required")
	}
	labels := maps.Clone(input.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[targetLabel] = targetId
	labels["app.kubernetes.io/managed-by"] = "borq"
	resource := client.dynamic.Resource(namespaceResource())
	live, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read namespace %s: %w", name, err)
	}
	if apierrors.IsNotFound(err) {
		namespace := unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name},
		}}
		namespace.SetLabels(labels)
		namespace.SetAnnotations(input.Annotations)
		// The Kubernetes dynamic client requires an object pointer for creation.
		_, err := resource.Create(ctx, &namespace, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("create namespace %s: %w", name, err)
		}
		return nil
	}
	owner := live.GetLabels()[targetLabel]
	if owner != "" && owner != targetId {
		return fmt.Errorf("%w: namespace %s belongs to another target %q", ErrOwnershipConflict, name, owner)
	}
	unchanged := true
	for key, value := range labels {
		unchanged = unchanged && live.GetLabels()[key] == value
	}
	for key, value := range input.Annotations {
		unchanged = unchanged && live.GetAnnotations()[key] == value
	}
	if unchanged {
		return nil
	}
	metadata := map[string]any{"resourceVersion": live.GetResourceVersion(), "labels": labels}
	if len(input.Annotations) > 0 {
		metadata["annotations"] = input.Annotations
	}
	patch, _ := json.Marshal(map[string]any{"metadata": metadata})
	_, err = resource.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("assign namespace %s: %w", name, err)
	}
	return nil
}

// Apply reconciles all supplied resources and restores configuration changed outside Borq.
// It never deletes resources or adopts a resource belonging to another target.
func (client Client) Apply(ctx context.Context, manifests []map[string]any, targetId string) error {
	// Validate and preview the complete batch before its first mutation.
	if _, err := client.Observe(ctx, manifests, targetId); err != nil {
		return err
	}
	for _, manifest := range manifests {
		if textAt(manifest, "kind") == "Job" {
			if err := client.StartJob(ctx, manifest, targetId); err != nil {
				return err
			}
			continue
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			return client.applyResource(ctx, manifest, targetId)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (client Client) applyResource(ctx context.Context, manifest map[string]any, targetId string) error {
	resource, err := client.resource(manifest)
	if err != nil {
		return err
	}
	name := textAt(manifest, "metadata", "name")
	live, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Create is conditional on absence, preventing an apply race from adopting another owner's object.
		created, createError := resource.Create(ctx, &unstructured.Unstructured{Object: copyMap(manifest)}, metav1.CreateOptions{FieldManager: fieldManager})
		if createError != nil {
			return fmt.Errorf("create %s %s: %w", textAt(manifest, "kind"), name, createError)
		}
		live = created
		err = nil
	}
	if err != nil {
		return fmt.Errorf("read before apply %s: %w", name, err)
	}
	if err := checkOwnership(live.Object, targetId); err != nil {
		return err
	}
	live, err = restoreControlledConfiguration(ctx, resource, live, manifest)
	if err != nil {
		return err
	}
	patchType, desired := resourcePatch(manifest, true)
	metadata := desired["metadata"].(map[string]any)
	metadata["resourceVersion"] = live.GetResourceVersion()
	payload, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	force := true
	options := metav1.PatchOptions{FieldManager: fieldManager}
	if patchType == types.ApplyPatchType {
		options.Force = &force
	}
	_, err = resource.Patch(ctx, name, patchType, payload, options)
	if err != nil {
		return fmt.Errorf("apply %s %s: %w", textAt(manifest, "kind"), name, err)
	}
	return nil
}

func resourcePatch(manifest map[string]any, existing bool) (types.PatchType, map[string]any) {
	desired := applyConfiguration(manifest)
	kind := textAt(desired, "kind")
	if existing && kind == "Gateway" {
		// Replace the owned listener list atomically. SSA can retain the API's
		// default TLS mode after removing certificateRefs, rejecting an HTTP
		// transition before admission can observe a valid listener.
		return types.MergePatchType, desired
	}
	if !existing || kind != "Deployment" && kind != "Service" {
		return types.ApplyPatchType, desired
	}
	// Kubernetes merges ports by number, not name. Replace the owned port list
	// atomically so a port change cannot retain two entries named "http" and
	// fail admission before controlled configuration can be restored.
	containers, _ := valueAt(desired, "spec", "template", "spec", "containers").([]any)
	portOwners := make([]map[string]any, 0, len(containers))
	if kind == "Service" {
		portOwners = append(portOwners, desired["spec"].(map[string]any))
	}
	for _, item := range containers {
		portOwners = append(portOwners, item.(map[string]any))
	}
	for _, owner := range portOwners {
		ports, _ := owner["ports"].([]any)
		owner["ports"] = append([]any{map[string]any{"$patch": "replace"}}, ports...)
	}
	return types.StrategicMergePatchType, desired
}

func applyConfiguration(manifest map[string]any) map[string]any {
	desired := copyMap(manifest)
	if textAt(desired, "kind") != "Deployment" {
		return desired
	}
	if textAt(desired, "spec", "strategy", "type") == "Recreate" {
		strategy := valueAt(desired, "spec", "strategy").(map[string]any)
		strategy["rollingUpdate"] = nil
	}
	containers, _ := valueAt(desired, "spec", "template", "spec", "containers").([]any)
	for _, item := range containers {
		container, _ := item.(map[string]any)
		variables, _ := container["env"].([]any)
		for _, item := range variables {
			variable, _ := item.(map[string]any)
			if variable == nil {
				continue
			}
			source, _ := variable["valueFrom"].(map[string]any)
			if source == nil {
				variable["valueFrom"] = nil
				continue
			}
			// SSA merges fields introduced by other managers. Explicitly clear
			// alternate union members before admission validates the merged EnvVar.
			variable["value"] = ""
			for _, field := range []string{"secretKeyRef", "configMapKeyRef", "fieldRef", "resourceFieldRef"} {
				if _, supplied := source[field]; !supplied {
					source[field] = nil
				}
			}
		}
	}
	return desired
}

// Unstructured pointers are required by the dynamic Kubernetes SDK.
func restoreControlledConfiguration(ctx context.Context, resource dynamic.ResourceInterface, live *unstructured.Unstructured, desired map[string]any) (*unstructured.Unstructured, error) {
	cleaned := copyMap(live.Object)
	removeUnwantedConfiguration(cleaned, desired)
	if configurationEqual(cleaned, live.Object) {
		return live, nil
	}
	patch := []map[string]any{{"op": "replace", "path": "/metadata/resourceVersion", "value": live.GetResourceVersion()}, {"op": "replace", "path": "/spec", "value": cleaned["spec"]}}
	payload, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("encode controlled configuration: %w", err)
	}
	updated, err := resource.Patch(ctx, live.GetName(), types.JSONPatchType, payload, metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return nil, fmt.Errorf("restore controlled configuration %s: %w", live.GetName(), err)
	}
	return updated, nil
}

func removeUnwantedConfiguration(result, desired map[string]any) {
	if textAt(desired, "kind") == "ExternalSecret" {
		// Additional sources or templates could substitute values from a different
		// store. Borq owns this whole spec; admission restores operator defaults.
		result["spec"] = copyValue(desired["spec"])
		return
	}
	if textAt(desired, "kind") != "Deployment" {
		return
	}
	removeSqliteConfiguration(result, desired)
	desiredContainers, _ := valueAt(desired, "spec", "template", "spec", "containers").([]any)
	liveContainers, _ := valueAt(result, "spec", "template", "spec", "containers").([]any)
	for _, desiredItem := range desiredContainers {
		desiredContainer, _ := desiredItem.(map[string]any)
		for _, liveItem := range liveContainers {
			liveContainer, _ := liveItem.(map[string]any)
			if textAt(liveContainer, "name") != textAt(desiredContainer, "name") {
				continue
			}
			for _, field := range []string{"env", "ports"} {
				pruneNamedEntries(liveContainer, desiredContainer, field)
			}
			for _, field := range []string{"readinessProbe", "startupProbe"} {
				if _, exists := desiredContainer[field]; !exists {
					delete(liveContainer, field)
				}
			}
		}
	}
}

func pruneNamedEntries(live, desired map[string]any, field string) {
	desiredEntries, _ := desired[field].([]any)
	liveEntries, _ := live[field].([]any)
	kept := make([]any, 0, len(desiredEntries))
	for _, liveItem := range liveEntries {
		liveEntry, _ := liveItem.(map[string]any)
		for _, desiredItem := range desiredEntries {
			desiredEntry, _ := desiredItem.(map[string]any)
			matches := textAt(liveEntry, "name") == textAt(desiredEntry, "name")
			if field == "ports" && textAt(desiredEntry, "name") == "" {
				matches = integerAt(liveEntry, "containerPort") == integerAt(desiredEntry, "containerPort")
			}
			if matches {
				kept = append(kept, liveItem)
				break
			}
		}
	}
	if len(kept) == 0 {
		delete(live, field)
		return
	}
	live[field] = kept
}
