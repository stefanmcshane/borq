package kubeapps

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestManagedEnvironmentRefreshWaitsForDeliveryAndPatchesOnlyTheConsumer owns live refresh and migration transport.
func TestManagedEnvironmentRefreshWaitsForDeliveryAndPatchesOnlyTheConsumer(t *testing.T) {
	check := is.New(t)
	desired, external, secret := managedEnvironmentFixture()
	deployment := defaultResource(deploymentManifest())
	valueAt(deployment, "spec", "template", "metadata", "labels").(map[string]any)["borq.run/revision"] = "app-revision"
	container := valueAt(deployment, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	container["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "previous-pinned-secret"}}}
	previousExternal, previousSecret := pinnedEnvironmentFixture()
	client, fakeClient := testClient(namespaceManifest(), external, secret, deployment, previousExternal, previousSecret)
	input := EnvironmentConsumerInput{Namespace: "shop", AppName: "frontend", TargetID: "target-one", Revision: "app-revision", Generation: "database/2", PreviousSecret: "previous-pinned-secret", Reference: `{"name":"database","version":"latest"}`, ExternalSecret: desired, ExpectedValues: map[string]string{"TOKEN": "new-password", "EMPTY": ""}}
	ready, err := client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	check.True(!ready)
	current, err := client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(t.Context(), "frontend", metav1.GetOptions{})
	check.NoErr(err)
	check.Equal(current.Object, deployment)
	secret["data"].(map[string]any)["TOKEN"] = base64.StdEncoding.EncodeToString([]byte("new-password"))
	secretResource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	check.NoErr(fakeClient.Tracker().Update(secretResource, &unstructured.Unstructured{Object: secret}, "shop"))
	ready, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	check.True(!ready)
	current, err = client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(t.Context(), "frontend", metav1.GetOptions{})
	check.NoErr(err)
	check.Equal(textAt(current.Object, "spec", "template", "metadata", "annotations", "borq.run/credential-generation"), "database/2")
	updatedContainer := valueAt(current.Object, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	check.Equal(updatedContainer["envFrom"], []any{map[string]any{"secretRef": map[string]any{"name": "frontend-env-2"}}})
	check.Equal(updatedContainer["image"], container["image"])
	check.Equal(current.GetAnnotations()["borq.run/environment-group"], input.Reference)
	patches := 0
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "deployments" {
			patches++
		}
	}
	_, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	secondPatches := 0
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "deployments" {
			secondPatches++
		}
	}
	check.Equal(secondPatches, patches)
	externals := client.dynamic.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}).Namespace("shop")
	_, err = externals.Get(t.Context(), input.PreviousSecret, metav1.GetOptions{})
	check.NoErr(err)
	current, err = client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(t.Context(), "frontend", metav1.GetOptions{})
	check.NoErr(err)
	current.Object["status"] = map[string]any{"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1), "readyReplicas": int64(1)}
	check.NoErr(fakeClient.Tracker().Update(deploymentResource(), current, "shop"))
	ready, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	check.True(!ready)
	_, err = externals.Get(t.Context(), input.PreviousSecret, metav1.GetOptions{})
	check.True(apierrors.IsNotFound(err))
	_, err = client.dynamic.Resource(secretResource).Namespace("shop").Get(t.Context(), input.PreviousSecret, metav1.GetOptions{})
	check.NoErr(err)
	ready, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	check.True(!ready)
	_, err = client.dynamic.Resource(secretResource).Namespace("shop").Get(t.Context(), input.PreviousSecret, metav1.GetOptions{})
	check.True(apierrors.IsNotFound(err))
	ready, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.NoErr(err)
	check.True(ready)
	deletes := []string{}
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() != "delete" {
			continue
		}
		deletion := action.(clienttesting.DeleteAction)
		check.Equal(deletion.GetName(), input.PreviousSecret)
		check.Equal(*deletion.GetDeleteOptions().Preconditions.ResourceVersion, "3")
		check.Equal(string(*deletion.GetDeleteOptions().Preconditions.UID), "previous-"+action.GetResource().Resource)
		deletes = append(deletes, action.GetResource().Resource)
	}
	check.Equal(deletes, []string{"externalsecrets", "secrets"})
	input.Revision = "another-revision"
	_, err = client.RefreshEnvironmentConsumer(t.Context(), input)
	check.True(err != nil)
}

// TestPinnedEnvironmentRetirementWaitsForPods protects credentials still referenced by nonterminal application or init containers.
func TestPinnedEnvironmentRetirementWaitsForPods(t *testing.T) {
	for _, test := range []struct {
		phase, field string
		blocked      bool
	}{
		{"Pending", "containers", true},
		{"Running", "containers", true},
		{"Unknown", "containers", true},
		{"Running", "initContainers", true},
		{"Succeeded", "containers", false},
		{"Failed", "initContainers", false},
	} {
		t.Run(test.phase+"/"+test.field, func(t *testing.T) {
			check := is.New(t)
			desired, _, _ := managedEnvironmentFixture()
			previousExternal, previousSecret := pinnedEnvironmentFixture()
			pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "previous-consumer", "namespace": "shop"}, "spec": map[string]any{test.field: []any{map[string]any{"name": "frontend", "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "previous-pinned-secret"}}}}}}, "status": map[string]any{"phase": test.phase}}
			client, _ := testClient(previousExternal, previousSecret, pod)
			ready, err := client.retirePinnedEnvironment(t.Context(), EnvironmentConsumerInput{Namespace: "shop", AppName: "frontend", TargetID: "target-one", PreviousSecret: "previous-pinned-secret", ExternalSecret: desired}, "frontend-live")
			check.NoErr(err)
			check.True(!ready)
			_, err = client.dynamic.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}).Namespace("shop").Get(t.Context(), "previous-pinned-secret", metav1.GetOptions{})
			if test.blocked {
				check.NoErr(err)
			} else {
				check.True(apierrors.IsNotFound(err))
			}
			_, err = client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace("shop").Get(t.Context(), "previous-pinned-secret", metav1.GetOptions{})
			check.NoErr(err)
		})
	}
}

// TestPinnedEnvironmentRetirementRejectsForeignResources keeps cleanup scoped to the accepted target, application and environment family.
func TestPinnedEnvironmentRetirementRejectsForeignResources(t *testing.T) {
	for _, kind := range []string{"ExternalSecret", "Secret"} {
		for _, change := range []string{"target", "application", "environment group"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				check := is.New(t)
				desired, _, _ := managedEnvironmentFixture()
				external, secret := pinnedEnvironmentFixture()
				resource := external
				if kind == "Secret" {
					resource = secret
				}
				switch change {
				case "target":
					valueAt(resource, "metadata", "labels").(map[string]any)[targetLabel] = "other-target"
				case "application":
					valueAt(resource, "metadata", "labels").(map[string]any)["app.kubernetes.io/name"] = "other-app"
				default:
					valueAt(resource, "metadata", "annotations").(map[string]any)["borq.run/environment-group-name"] = "other-group"
				}
				client, fakeClient := testClient(resource)
				_, err := client.retirePinnedEnvironment(t.Context(), EnvironmentConsumerInput{Namespace: "shop", AppName: "frontend", TargetID: "target-one", PreviousSecret: "previous-pinned-secret", ExternalSecret: desired}, "frontend-live")
				if change == "target" {
					check.True(errors.Is(err, ErrOwnershipConflict))
				} else {
					check.True(err != nil && strings.Contains(err.Error(), "previous environment credential ownership changed"))
				}
				for _, action := range fakeClient.Actions() {
					check.True(action.GetVerb() != "delete")
				}
			})
		}
	}
}

func pinnedEnvironmentFixture() (map[string]any, map[string]any) {
	external, secret := readyExternalSecret()
	for _, resource := range []map[string]any{external, secret} {
		metadata := resource["metadata"].(map[string]any)
		metadata["name"], metadata["resourceVersion"] = "previous-pinned-secret", "3"
		metadata["annotations"] = map[string]any{"borq.run/environment-group-name": "database"}
		resourceName := "secrets"
		if resource["kind"] == "ExternalSecret" {
			resourceName = "externalsecrets"
		}
		metadata["uid"] = "previous-" + resourceName
	}
	return external, secret
}

// TestManagedEnvironmentReadinessRequiresLiveProducerIdentity keeps mutable readiness separate from ordinary pins.
func TestManagedEnvironmentReadinessRequiresLiveProducerIdentity(t *testing.T) {
	check := is.New(t)
	desired, external, secret := managedEnvironmentFixture()
	client, _ := testClient(external, secret)
	state, _, err := client.externalSecretStatus(t.Context(), external, desired)
	check.NoErr(err)
	check.Equal(state, "succeeded")
	secret["immutable"] = true
	client, _ = testClient(external, secret)
	state, _, err = client.externalSecretStatus(t.Context(), external, desired)
	check.NoErr(err)
	check.Equal(state, "running")
}

func managedEnvironmentFixture() (map[string]any, map[string]any, map[string]any) {
	external, secret := readyExternalSecret()
	external["metadata"].(map[string]any)["annotations"] = map[string]any{"borq.run/postgres-id": "database", "borq.run/environment-group-name": "database"}
	spec := external["spec"].(map[string]any)
	spec["refreshPolicy"], spec["refreshInterval"] = "Periodic", "15s"
	spec["target"].(map[string]any)["immutable"] = false
	delete(valueAt(external, "spec", "dataFrom").([]any)[0].(map[string]any)["extract"].(map[string]any), "version")
	for _, annotations := range []map[string]any{valueAt(external, "spec", "target", "template", "metadata", "annotations").(map[string]any), valueAt(secret, "metadata", "annotations").(map[string]any)} {
		annotations["borq.run/env-version"], annotations["borq.run/postgres-id"] = "latest", "database"
	}
	secret["immutable"] = false
	secret["data"].(map[string]any)["TOKEN"] = base64.StdEncoding.EncodeToString([]byte("previous-password"))
	desired := copyMap(external)
	delete(desired, "status")
	return desired, external, secret
}
