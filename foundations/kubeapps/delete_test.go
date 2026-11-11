package kubeapps

import (
	"errors"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestApplicationDeletionOwnership checks the complete inventory before deletion and protects ownership races with Kubernetes preconditions.
func TestApplicationDeletionOwnership(t *testing.T) {
	for _, change := range []string{"none", "target", "application", "race"} {
		t.Run(change, func(t *testing.T) {
			assert := is.New(t)
			deployment := defaultResource(deploymentManifest())
			service := defaultResource(copyMap(deploymentManifest()))
			service["apiVersion"], service["kind"] = "v1", "Service"
			for _, resource := range []map[string]any{deployment, service} {
				valueAt(resource, "metadata", "labels").(map[string]any)["app.kubernetes.io/name"] = "frontend"
				resource["metadata"].(map[string]any)["uid"] = "owned-" + textAt(resource, "kind")
				resource["metadata"].(map[string]any)["resourceVersion"] = "7"
			}
			foreign := copyMap(service)
			if change == "target" {
				valueAt(foreign, "metadata", "labels").(map[string]any)[targetLabel] = "another-space"
			}
			if change == "application" {
				valueAt(foreign, "metadata", "labels").(map[string]any)["app.kubernetes.io/name"] = "another-app"
			}
			client, fake := testClient(deployment, foreign)
			deleted := 0
			fake.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deleted++
				options := action.(clienttesting.DeleteAction).GetDeleteOptions()
				assert.Equal(*options.PropagationPolicy, metav1.DeletePropagationForeground)
				assert.Equal(*options.Preconditions.ResourceVersion, "7")
				assert.Equal(string(*options.Preconditions.UID), "owned-"+map[string]string{"deployments": "Deployment", "services": "Service"}[action.GetResource().Resource])
				if change == "race" {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "frontend", errors.New("ownership changed"))
				}
				return false, nil, nil
			})
			status, err := client.DeleteApplicationResources(t.Context(), []map[string]any{deployment, service}, "target-one", "frontend")
			if change == "target" || change == "application" {
				assert.True(errors.Is(err, ErrOwnershipConflict))
				assert.Equal(deleted, 0)
				return
			}
			if change == "race" {
				assert.True(apierrors.IsConflict(err))
				assert.Equal(deleted, 1)
				return
			}
			assert.NoErr(err)
			assert.Equal(status.State, "running")
			assert.Equal(deleted, 2)
			status, err = client.DeleteApplicationResources(t.Context(), []map[string]any{deployment, service}, "target-one", "frontend")
			assert.NoErr(err)
			assert.Equal(status.State, "succeeded")
		})
	}
}

// TestApplicationDeletionRetainsSqlite prevents a caller from passing durable storage or recovery state to the destructive boundary.
func TestApplicationDeletionRetainsSqlite(t *testing.T) {
	for _, kind := range []string{"PersistentVolumeClaim", "ConfigMap", "Secret", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			assert := is.New(t)
			manifest := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": "frontend-sqlite", "namespace": "shop", "labels": map[string]any{targetLabel: "target-one", "app.kubernetes.io/name": "frontend", "borq.run/phase": "sqlite"}}}
			client, fake := testClient()
			_, err := client.DeleteApplicationResources(t.Context(), []map[string]any{manifest}, "target-one", "frontend")
			assert.True(err != nil)
			assert.Equal(len(fake.Actions()), 0)
		})
	}
}
