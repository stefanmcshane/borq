package kubeapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestObserveMissingNamespace verifies that first-deployment planning creates no cluster resources.
func TestObserveMissingNamespace(t *testing.T) {
	is := is.New(t)
	client, fakeClient := testClient()
	desired := deploymentManifest()
	diffs, err := client.Observe(context.Background(), []map[string]any{desired}, "target-one")
	is.NoErr(err)
	is.Equal(len(diffs), 1)
	is.Equal(diffs[0].Action, "create")
	is.Equal(diffs[0].Live, nil)
	is.Equal(diffs[0].Fingerprint, Fingerprint(nil))
	for _, action := range fakeClient.Actions() {
		is.Equal(action.GetVerb(), "get")
	}
}

// TestApplyRestoresManagedConfiguration observes scalar and managed-entry drift
// without writing, then repairs it while preserving injected sidecars.
func TestApplyRestoresManagedConfiguration(t *testing.T) {
	is := is.New(t)
	desired := deploymentManifest()
	live := defaultResource(desired)
	live["spec"].(map[string]any)["replicas"] = int64(5)
	container := managedContainer(live)
	container["env"] = []any{map[string]any{"name": "MANUAL", "value": "added"}}
	container["ports"] = []any{map[string]any{"name": "manual", "containerPort": int64(9090)}}
	container["startupProbe"] = map[string]any{"exec": map[string]any{"command": []any{"true"}}}
	containers := valueAt(live, "spec", "template", "spec", "containers").([]any)
	live["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"] = append(containers, map[string]any{"name": "injected-sidecar", "image": "proxy:v1"})
	client, _ := testClient(namespaceManifest(), live)
	diffs, err := client.Observe(context.Background(), []map[string]any{desired}, "target-one")
	is.NoErr(err)
	is.Equal(diffs[0].Action, "update")
	is.Equal(integerAt(diffs[0].Live, "spec", "replicas"), int64(5))
	is.Equal(integerAt(diffs[0].Desired, "spec", "replicas"), int64(1))
	is.Equal(valueAt(managedContainer(diffs[0].Desired), "env"), nil)
	current, err := client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(context.Background(), "frontend", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(integerAt(current.Object, "spec", "replicas"), int64(5))
	is.Equal(len(managedContainer(current.Object)["env"].([]any)), 1)
	is.NoErr(client.Apply(context.Background(), []map[string]any{desired}, "target-one"))
	current, err = client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(context.Background(), "frontend", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(integerAt(current.Object, "spec", "replicas"), int64(1))
	is.Equal(valueAt(managedContainer(current.Object), "env"), nil)
	is.Equal(valueAt(managedContainer(current.Object), "ports"), nil)
	is.Equal(valueAt(managedContainer(current.Object), "startupProbe"), nil)
	is.Equal(len(valueAt(current.Object, "spec", "template", "spec", "containers").([]any)), 2)
}

// TestApplyRestoresEnvironmentSources reproduces Kubernetes admission rejecting
// a merged literal or alternate valueFrom source before drift can be repaired.
func TestApplyRestoresEnvironmentSources(t *testing.T) {
	for _, current := range []struct {
		name     string
		variable map[string]any
	}{
		{name: "literal", variable: map[string]any{"name": "RELEASE", "value": "manual-literal"}},
		{name: "config map", variable: map[string]any{"name": "RELEASE", "valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": "manual-config", "key": "release"}}}},
	} {
		t.Run(current.name, func(t *testing.T) {
			assert := is.New(t)
			desired := deploymentManifest()
			managedContainer(desired)["env"] = []any{map[string]any{"name": "RELEASE", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "pinned-secret", "key": "RELEASE"}}}}
			original := copyMap(desired)
			live := defaultResource(desired)
			managedContainer(live)["env"] = []any{current.variable}
			client, fakeClient := testClient(namespaceManifest(), live)
			fakeClient.PrependReactor("patch", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				patch := action.(clienttesting.PatchAction)
				if patch.GetPatchType() != types.ApplyPatchType && patch.GetPatchType() != types.StrategicMergePatchType {
					return false, nil, nil
				}
				var requested map[string]any
				if err := json.Unmarshal(patch.GetPatch(), &requested); err != nil {
					return true, nil, err
				}
				object, err := fakeClient.Tracker().Get(action.GetResource(), action.GetNamespace(), patch.GetName())
				if err != nil {
					return true, nil, err
				}
				merged := fakeApply(object.(*unstructured.Unstructured).Object, requested)
				for _, item := range managedContainer(merged)["env"].([]any) {
					variable := item.(map[string]any)
					source, _ := variable["valueFrom"].(map[string]any)
					if textAt(variable, "value") != "" && source != nil {
						return true, nil, fmt.Errorf("EnvVar cannot contain a nonempty value and valueFrom")
					}
					selectors := 0
					for _, selector := range source {
						if selector != nil {
							selectors++
						}
					}
					if selectors > 1 {
						return true, nil, fmt.Errorf("EnvVar cannot contain multiple valueFrom selectors")
					}
				}
				return false, nil, nil
			})
			differences, err := client.Observe(context.Background(), []map[string]any{desired}, "target-one")
			assert.NoErr(err)
			assert.Equal(differences[0].Action, "update")
			assert.NoErr(client.Apply(context.Background(), []map[string]any{desired}, "target-one"))
			differences, err = client.Observe(context.Background(), []map[string]any{desired}, "target-one")
			assert.NoErr(err)
			assert.Equal(differences[0].Action, "unchanged")
			assert.Equal(desired, original)
		})
	}
}

// TestOwnershipIsNeverAdopted verifies a conflicting or unlabeled object blocks the entire apply before mutation.
func TestOwnershipIsNeverAdopted(t *testing.T) {
	for _, owner := range []string{"", "another-target"} {
		t.Run(owner, func(t *testing.T) {
			is := is.New(t)
			live := deploymentManifest()
			live["metadata"].(map[string]any)["labels"] = map[string]any{targetLabel: owner}
			client, fakeClient := testClient(namespaceManifest(), live)
			_, err := client.Observe(context.Background(), []map[string]any{deploymentManifest()}, "target-one")
			is.True(err != nil)
			is.True(errors.Is(err, ErrOwnershipConflict))
			is.True(strings.Contains(err.Error(), "refuse to adopt"))
			is.True(client.Apply(context.Background(), []map[string]any{deploymentManifest()}, "target-one") != nil)
			for _, action := range fakeClient.Actions() {
				is.Equal(action.GetVerb(), "get")
			}
		})
	}
}

// TestEnsureNamespaceMetadata upgrades existing owned namespaces, repairs managed
// metadata, preserves unrelated keys, and refuses to relabel another space.
func TestEnsureNamespaceMetadata(t *testing.T) {
	assert := is.New(t)
	for _, ownership := range []string{"absent", "owned", "unlabelled"} {
		t.Run(ownership, func(t *testing.T) {
			assert := is.New(t)
			client, fakeClient := testClient()
			if ownership != "absent" {
				manifest := namespaceManifest()
				if ownership == "unlabelled" {
					delete(manifest["metadata"].(map[string]any)["labels"].(map[string]any), targetLabel)
				}
				manifest["metadata"].(map[string]any)["annotations"] = map[string]any{"example.com/keep": "yes"}
				manifest["metadata"].(map[string]any)["labels"].(map[string]any)["example.com/keep"] = "yes"
				client, fakeClient = testClient(manifest)
			}
			input := NamespaceInput{Name: "shop", TargetID: "target-one", Labels: map[string]string{"borq.run/organization-id": "acme", "borq.run/space-id": "target-one"}, Annotations: map[string]string{"borq.run/organization-id": "acme", "borq.run/space-id": "target-one"}}
			assert.NoErr(client.EnsureNamespace(context.Background(), input))
			assert.NoErr(client.EnsureNamespace(context.Background(), input))
			current, err := client.dynamic.Resource(namespaceResource()).Get(context.Background(), "shop", metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(current.GetLabels()[targetLabel], "target-one")
			assert.Equal(current.GetLabels()["borq.run/organization-id"], "acme")
			assert.Equal(current.GetLabels()["borq.run/space-id"], "target-one")
			assert.Equal(current.GetAnnotations()["borq.run/organization-id"], "acme")
			assert.Equal(current.GetAnnotations()["borq.run/space-id"], "target-one")
			if ownership != "absent" {
				assert.Equal(current.GetLabels()["example.com/keep"], "yes")
				assert.Equal(current.GetAnnotations()["example.com/keep"], "yes")
			}
			current.SetAnnotations(map[string]string{"borq.run/organization-id": "changed"})
			assert.NoErr(fakeClient.Tracker().Update(namespaceResource(), current, ""))
			assert.NoErr(client.EnsureNamespace(context.Background(), input))
			current, err = client.dynamic.Resource(namespaceResource()).Get(context.Background(), "shop", metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(current.GetAnnotations()["borq.run/organization-id"], "acme")
			input.TargetID = "another-space"
			assert.True(errors.Is(client.EnsureNamespace(context.Background(), input), ErrOwnershipConflict))
		})
	}
	// Older plans without annotations must preserve annotations already on a namespace.
	manifest := namespaceManifest()
	manifest["metadata"].(map[string]any)["annotations"] = map[string]any{"example.com/keep": "yes"}
	client, _ := testClient(manifest)
	assert.NoErr(client.EnsureNamespace(context.Background(), NamespaceInput{Name: "shop", TargetID: "target-one"}))
	current, err := client.dynamic.Resource(namespaceResource()).Get(context.Background(), "shop", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(current.GetAnnotations()["example.com/keep"], "yes")
	for _, input := range []NamespaceInput{{TargetID: "target-one"}, {Name: "shop"}} {
		assert.True(client.EnsureNamespace(context.Background(), input) != nil)
	}
}

// TestManifestValidation verifies malformed or out-of-scope resources cannot reach the cluster.
func TestManifestValidation(t *testing.T) {
	for _, test := range []struct {
		name, target, message string
		change                func(map[string]any)
		duplicate             bool
	}{
		{name: "missing target", message: "target ID is required"},
		{name: "unsupported kind", target: "target-one", message: "unsupported application resource", change: func(manifest map[string]any) { manifest["kind"] = "Secret" }},
		{name: "missing namespace", target: "target-one", message: "requires a name and namespace", change: func(manifest map[string]any) { delete(manifest["metadata"].(map[string]any), "namespace") }},
		{name: "wrong target", target: "wrong-target", message: "must carry target label"},
		{name: "duplicate", target: "target-one", message: "duplicate manifest", duplicate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			client, fakeClient := testClient()
			manifest := deploymentManifest()
			if test.change != nil {
				test.change(manifest)
			}
			manifests := []map[string]any{manifest}
			if test.duplicate {
				manifests = append(manifests, deploymentManifest())
			}
			_, err := client.Observe(context.Background(), manifests, test.target)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			for _, action := range fakeClient.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
			if !test.duplicate {
				assert.Equal(len(fakeClient.Actions()), 0)
			}
		})
	}
}

// TestFingerprintIgnoresRuntimeData verifies live preconditions track configuration rather than controller churn.
func TestFingerprintIgnoresRuntimeData(t *testing.T) {
	is := is.New(t)
	before := deploymentManifest()
	after := copyMap(before)
	metadata := after["metadata"].(map[string]any)
	metadata["resourceVersion"] = "1234"
	metadata["uid"] = "changed-uid"
	metadata["generation"] = int64(42)
	metadata["annotations"] = map[string]any{"deployment.kubernetes.io/revision": "99"}
	after["status"] = map[string]any{"availableReplicas": int64(1)}
	is.Equal(Fingerprint(after), Fingerprint(before))
	managedContainer(after)["image"] = "frontend:manual"
	is.True(Fingerprint(after) != Fingerprint(before))
	is.Equal(managedContainer(before)["image"], "frontend:v1")
}

// TestNewLoadsCredentialsWithoutNetwork verifies registration can construct clients before explicit validation.
func TestNewLoadsCredentialsWithoutNetwork(t *testing.T) {
	is := is.New(t)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	is.NoErr(os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: http://127.0.0.1:1\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n"), 0600))
	client, err := New(NewInput{KubeconfigPath: path, Context: "test"})
	is.NoErr(err)
	is.True(client.Validate(context.Background()) != nil)
	_, err = New(NewInput{})
	is.True(err != nil)
	_, err = New(NewInput{InCluster: true, KubeconfigPath: path})
	is.True(err != nil)
	_, err = New(NewInput{KubeconfigPath: filepath.Join(t.TempDir(), "missing")})
	is.True(err != nil)
	is.True((Client{}).Validate(context.Background()) != nil)
}

func deploymentManifest() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "frontend", "namespace": "shop", "labels": map[string]any{targetLabel: "target-one"}},
		"spec": map[string]any{
			"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "frontend"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "frontend"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "frontend", "image": "frontend:v1", "env": []any{}, "ports": []any{}}}},
			},
		},
	}
}

func namespaceManifest() map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": map[string]any{targetLabel: "target-one"}}}
}

func deploymentResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

func managedContainer(manifest map[string]any) map[string]any {
	containers := valueAt(manifest, "spec", "template", "spec", "containers").([]any)
	for _, item := range containers {
		container := item.(map[string]any)
		if textAt(container, "name") == "frontend" {
			return container
		}
	}
	return nil
}

func defaultResource(manifest map[string]any) map[string]any {
	result := copyMap(manifest)
	metadata := result["metadata"].(map[string]any)
	metadata["resourceVersion"] = "1"
	if textAt(result, "kind") == "Deployment" {
		spec := result["spec"].(map[string]any)
		if spec["strategy"] == nil {
			spec["strategy"] = map[string]any{"type": "RollingUpdate"}
		}
		containers, _ := valueAt(result, "spec", "template", "spec", "containers").([]any)
		for _, item := range containers {
			container := item.(map[string]any)
			if container["imagePullPolicy"] == nil {
				container["imagePullPolicy"] = "IfNotPresent"
			}
			for _, field := range []string{"env", "ports"} {
				if entries, ok := container[field].([]any); ok && len(entries) == 0 {
					delete(container, field)
				}
			}
		}
	}
	return result
}

// Fake clients are pointers because client-go's reactor and object tracker APIs mutate shared state.
func testClient(manifests ...map[string]any) (Client, *fake.FakeDynamicClient) {
	objects := make([]runtime.Object, 0, len(manifests))
	for _, manifest := range manifests {
		objects = append(objects, &unstructured.Unstructured{Object: copyMap(manifest)})
	}
	listKinds := map[schema.GroupVersionResource]string{
		deploymentResource(): "DeploymentList",
		{Group: "apps", Version: "v1", Resource: "replicasets"}:                        "ReplicaSetList",
		{Version: "v1", Resource: "services"}:                                          "ServiceList",
		{Version: "v1", Resource: "namespaces"}:                                        "NamespaceList",
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}:             "IngressList",
		{Group: "batch", Version: "v1", Resource: "jobs"}:                              "JobList",
		{Version: "v1", Resource: "secrets"}:                                           "SecretList",
		{Version: "v1", Resource: "pods"}:                                              "PodList",
		{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}:     "ExternalSecretList",
		{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}: "ClusterSecretStoreList",
	}
	fakeClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	fakeClient.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		result := &unstructured.Unstructured{Object: defaultResource(created.Object)}
		if len(action.(clienttesting.CreateActionImpl).GetCreateOptions().DryRun) > 0 {
			return true, result, nil
		}
		err := fakeClient.Tracker().Create(action.GetResource(), result, action.GetNamespace())
		return true, result, err
	})
	fakeClient.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch := action.(clienttesting.PatchAction)
		object, err := fakeClient.Tracker().Get(action.GetResource(), action.GetNamespace(), patch.GetName())
		var current map[string]any
		if err == nil {
			current = copyMap(object.(*unstructured.Unstructured).Object)
		}
		var updated map[string]any
		switch patch.GetPatchType() {
		case types.ApplyPatchType:
			if err := json.Unmarshal(patch.GetPatch(), &updated); err != nil {
				return true, nil, err
			}
			updated = fakeApply(current, updated)
		case types.JSONPatchType, types.MergePatchType, types.StrategicMergePatchType:
			if err != nil {
				return true, nil, err
			}
			encoded, _ := json.Marshal(current)
			var transformed []byte
			if patch.GetPatchType() == types.JSONPatchType {
				decoded, decodeError := jsonpatch.DecodePatch(patch.GetPatch())
				if decodeError != nil {
					return true, nil, decodeError
				}
				transformed, err = decoded.Apply(encoded)
			} else if patch.GetPatchType() == types.StrategicMergePatchType {
				var model any = appsv1.Deployment{}
				if action.GetResource().Resource == "services" {
					model = corev1.Service{}
				}
				transformed, err = strategicpatch.StrategicMergePatch(encoded, patch.GetPatch(), model)
			} else {
				transformed, err = jsonpatch.MergePatch(encoded, patch.GetPatch())
			}
			if err != nil {
				return true, nil, err
			}
			if err := json.Unmarshal(transformed, &updated); err != nil {
				return true, nil, err
			}
		default:
			return true, nil, fmt.Errorf("unexpected patch type %s", patch.GetPatchType())
		}
		updated = defaultResource(updated)
		result := &unstructured.Unstructured{Object: updated}
		if len(action.(clienttesting.PatchActionImpl).GetPatchOptions().DryRun) == 0 {
			if current == nil {
				err = fakeClient.Tracker().Create(action.GetResource(), result, action.GetNamespace())
			} else {
				err = fakeClient.Tracker().Update(action.GetResource(), result, action.GetNamespace())
			}
			if err != nil {
				return true, nil, err
			}
		}
		return true, result, nil
	})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "apps", Version: "v1"}, {Group: "networking.k8s.io", Version: "v1"}, {Group: "batch", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}, meta.RESTScopeNamespace)
	return Client{dynamic: fakeClient, mapper: mapper}, fakeClient
}

func fakeApply(live, desired map[string]any) map[string]any {
	if live == nil {
		return copyMap(desired)
	}
	result := copyMap(live)
	for key, value := range desired {
		object, isMap := value.(map[string]any)
		liveObject, _ := result[key].(map[string]any)
		if isMap {
			result[key] = fakeApply(liveObject, object)
			continue
		}
		entries, isList := value.([]any)
		liveEntries, _ := result[key].([]any)
		if isList && (key == "containers" || key == "env" || key == "ports") {
			merged := copyValue(liveEntries).([]any)
			for _, entry := range entries {
				wanted := entry.(map[string]any)
				found := false
				for index, existing := range merged {
					actual := existing.(map[string]any)
					if textAt(actual, "name") == textAt(wanted, "name") {
						merged[index] = fakeApply(actual, wanted)
						found = true
						break
					}
				}
				if !found {
					merged = append(merged, copyMap(wanted))
				}
			}
			result[key] = merged
			continue
		}
		result[key] = copyValue(value)
	}
	return result
}
