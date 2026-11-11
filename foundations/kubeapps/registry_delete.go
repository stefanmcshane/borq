package kubeapps

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DeleteRegistrySecrets removes only the named, owned registry projections and
// pull secrets. Resource identity preconditions protect replacements from deletion.
func (client Client) DeleteRegistrySecrets(ctx context.Context, namespace, targetID string, names []string) error {
	resources := []unstructured.Unstructured{}
	for _, name := range names {
		for _, kind := range []string{"ExternalSecret", "Secret"} {
			version := "v1"
			if kind == "ExternalSecret" {
				version = "external-secrets.io/v1"
			}
			manifest := map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}}
			handle, err := client.resource(manifest)
			if err != nil {
				return err
			}
			live, err := handle.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := checkOwnership(live.Object, targetID); err != nil {
				return err
			}
			if textAt(live.Object, "metadata", "labels", "borq.run/phase") != "registry" || live.GetUID() == "" || live.GetResourceVersion() == "" {
				return fmt.Errorf("%w: registry resource identity is missing", ErrOwnershipConflict)
			}
			resources = append(resources, *live)
		}
	}
	for _, live := range resources {
		handle, err := client.resource(live.Object)
		if err != nil {
			return err
		}
		uid, revision := live.GetUID(), live.GetResourceVersion()
		// Kubernetes requires pointers to make identity checks mandatory.
		err = handle.Delete(ctx, live.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
