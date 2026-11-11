package kubeapps

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DeleteApplicationResources removes only saved runtime resources still owned by this target and application.
// Foreground propagation waits for dependent pods; SQLite storage and recovery resources are never deleted.
func (client Client) DeleteApplicationResources(ctx context.Context, manifests []map[string]any, targetId, appName string) (RolloutStatus, error) {
	resources := []ResourceStatus{}
	liveResources := []unstructured.Unstructured{}
	for _, manifest := range manifests {
		if err := validateManifest(manifest, targetId); err != nil {
			return RolloutStatus{}, err
		}
		if appName == "" || textAt(manifest, "metadata", "labels", "app.kubernetes.io/name") != appName {
			return RolloutStatus{}, fmt.Errorf("%w: deletion requires the selected application's label", ErrOwnershipConflict)
		}
		if textAt(manifest, "metadata", "labels", "borq.run/phase") == "sqlite" && textAt(manifest, "kind") != "CronJob" {
			return RolloutStatus{}, fmt.Errorf("refuse to delete retained SQLite resource %s", textAt(manifest, "metadata", "name"))
		}
		handle, err := client.resource(manifest)
		if err != nil {
			return RolloutStatus{}, err
		}
		live, err := handle.Get(ctx, textAt(manifest, "metadata", "name"), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return RolloutStatus{}, err
		}
		if err := checkOwnership(live.Object, targetId); err != nil {
			return RolloutStatus{}, err
		}
		if textAt(live.Object, "metadata", "labels", "app.kubernetes.io/name") != appName {
			return RolloutStatus{}, fmt.Errorf("%w: %s no longer belongs to application %s", ErrOwnershipConflict, live.GetName(), appName)
		}
		liveResources = append(liveResources, *live)
	}
	// Check the entire inventory before starting any destructive requests.
	for _, live := range liveResources {
		resources = append(resources, ResourceStatus{ApiVersion: live.GetAPIVersion(), Kind: live.GetKind(), Namespace: live.GetNamespace(), Name: live.GetName(), State: "running", Message: "waiting for deletion and dependent resources"})
		if live.GetDeletionTimestamp() != nil {
			continue
		}
		handle, err := client.resource(live.Object)
		if err != nil {
			return RolloutStatus{}, err
		}
		uid, revision := live.GetUID(), live.GetResourceVersion()
		if uid == "" || revision == "" {
			return RolloutStatus{}, fmt.Errorf("refuse to delete %s without its Kubernetes identity", live.GetName())
		}
		// Preconditions protect against ownership changes between the read and delete.
		propagation := metav1.DeletePropagationForeground
		err = handle.Delete(ctx, live.GetName(), metav1.DeleteOptions{PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}})
		if err != nil && !apierrors.IsNotFound(err) {
			return RolloutStatus{}, fmt.Errorf("delete %s %s: %w", live.GetKind(), live.GetName(), err)
		}
	}
	state := "running"
	if len(resources) == 0 {
		state = "succeeded"
	}
	return RolloutStatus{State: state, Resources: resources}, nil
}
