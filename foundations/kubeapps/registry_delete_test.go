package kubeapps

import (
	"errors"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestRegistryDeletionOwnership protects the complete registry inventory and
// identity preconditions, without permitting application or foreign secret deletion.
func TestRegistryDeletionOwnership(t *testing.T) {
	for _, change := range []string{"none", "target", "phase", "identity", "race"} {
		t.Run(change, func(t *testing.T) {
			assert := is.New(t)
			manifests := []map[string]any{}
			for _, kind := range []string{"ExternalSecret", "Secret"} {
				version := "v1"
				if kind == "ExternalSecret" {
					version = "external-secrets.io/v1"
				}
				manifests = append(manifests, map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": "borq-registry-example", "namespace": "shop", "uid": "owned-" + kind, "resourceVersion": "7", "labels": map[string]any{targetLabel: "target-one", "borq.run/phase": "registry"}}})
			}
			if change == "target" {
				valueAt(manifests[1], "metadata", "labels").(map[string]any)[targetLabel] = "another-space"
			}
			if change == "phase" {
				valueAt(manifests[1], "metadata", "labels").(map[string]any)["borq.run/phase"] = "sqlite"
			}
			if change == "identity" {
				delete(manifests[1]["metadata"].(map[string]any), "uid")
			}
			client, fake := testClient(manifests...)
			deleted := 0
			fake.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deleted++
				options := action.(clienttesting.DeleteAction).GetDeleteOptions()
				assert.Equal(*options.Preconditions.ResourceVersion, "7")
				assert.Equal(string(*options.Preconditions.UID), "owned-"+map[string]string{"externalsecrets": "ExternalSecret", "secrets": "Secret"}[action.GetResource().Resource])
				if change == "race" {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "borq-registry-example", errors.New("replacement"))
				}
				return false, nil, nil
			})
			err := client.DeleteRegistrySecrets(t.Context(), "shop", "target-one", []string{"borq-registry-example"})
			if change == "target" || change == "phase" || change == "identity" {
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
			assert.Equal(deleted, 2)
			assert.NoErr(client.DeleteRegistrySecrets(t.Context(), "shop", "target-one", []string{"borq-registry-example"}))
		})
	}
}
