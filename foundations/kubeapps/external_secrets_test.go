package kubeapps

import (
	"context"
	"strings"
	"testing"

	"github.com/matryer/is"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestExternalSecretReadinessRequiresPinnedOwnedSecret verifies a Ready condition
// alone cannot release workloads with a missing, stale, foreign, or partial Secret.
func TestExternalSecretReadinessRequiresPinnedOwnedSecret(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any, map[string]any)
		state  string
	}{
		{name: "ready", state: "succeeded", change: func(map[string]any, map[string]any) {}},
		{name: "operator not ready", state: "running", change: func(live, _ map[string]any) { live["status"] = map[string]any{} }},
		{name: "operator observed previous revision", state: "running", change: func(live, _ map[string]any) { live["status"].(map[string]any)["observedGeneration"] = int64(1) }},
		{name: "condition observed previous revision", state: "running", change: func(live, _ map[string]any) {
			live["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}
		}},
		{name: "changed remote version", state: "running", change: func(live, _ map[string]any) {
			valueAt(live, "spec", "dataFrom").([]any)[0].(map[string]any)["extract"].(map[string]any)["version"] = "1"
		}},
		{name: "extra source", state: "running", change: func(live, _ map[string]any) {
			live["spec"].(map[string]any)["data"] = []any{map[string]any{"secretKey": "TOKEN", "remoteRef": map[string]any{"key": "other-source"}}}
		}},
		{name: "overridden store", state: "running", change: func(live, _ map[string]any) {
			valueAt(live, "spec", "dataFrom").([]any)[0].(map[string]any)["sourceRef"] = map[string]any{"storeRef": map[string]any{"name": "other-store"}}
		}},
		{name: "value transformation", state: "running", change: func(live, _ map[string]any) {
			valueAt(live, "spec", "target", "template").(map[string]any)["data"] = map[string]any{"TOKEN": "replacement"}
		}},
		{name: "foreign target", state: "failed", change: func(_, secret map[string]any) {
			secret["metadata"].(map[string]any)["labels"].(map[string]any)[targetLabel] = "other-target"
		}},
		{name: "old secret version", state: "running", change: func(_, secret map[string]any) {
			secret["metadata"].(map[string]any)["annotations"].(map[string]any)["borq.run/env-version"] = "1"
		}},
		{name: "mutable secret", state: "running", change: func(_, secret map[string]any) { secret["immutable"] = false }},
		{name: "wrong secret type", state: "running", change: func(_, secret map[string]any) { secret["type"] = "kubernetes.io/tls" }},
		{name: "old owner", state: "running", change: func(_, secret map[string]any) {
			valueAt(secret, "metadata", "ownerReferences").([]any)[0].(map[string]any)["uid"] = "previous-external-secret"
		}},
		{name: "missing key", state: "running", change: func(_, secret map[string]any) { delete(secret["data"].(map[string]any), "EMPTY") }},
		{name: "extra key", state: "running", change: func(_, secret map[string]any) { secret["data"].(map[string]any)["EXTRA"] = "not-output" }},
		{name: "wrong key", state: "running", change: func(_, secret map[string]any) {
			delete(secret["data"].(map[string]any), "EMPTY")
			secret["data"].(map[string]any)["WRONG"] = "not-output"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := externalSecretManifest()
			live, secret := readyExternalSecret()
			test.change(live, secret)
			client, _ := testClient(live, secret)
			status, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, test.state)
			assert.True(!strings.Contains(status.Message, "not-output"))
		})
	}
	assert := is.New(t)
	live, _ := readyExternalSecret()
	client, _ := testClient(live)
	status, err := client.Status(context.Background(), []map[string]any{externalSecretManifest()}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "running")
}

// TestExternalSecretEmptyPublishedConfig accepts an explicitly published empty
// map while retaining the same immutable Secret and ownership readiness gates.
func TestExternalSecretEmptyPublishedConfig(t *testing.T) {
	assert := is.New(t)
	desired := externalSecretManifest()
	valueAt(desired, "spec", "target", "template", "metadata", "annotations").(map[string]any)["borq.run/env-keys"] = "[]"
	live, secret := readyExternalSecret()
	valueAt(live, "spec", "target", "template", "metadata", "annotations").(map[string]any)["borq.run/env-keys"] = "[]"
	valueAt(secret, "metadata", "annotations").(map[string]any)["borq.run/env-keys"] = "[]"
	secret["data"] = map[string]any{}
	client, _ := testClient(live, secret)
	status, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "succeeded")
}

// TestExternalSecretReferencesMustBePinned catches malformed resource identities
// before an arbitrary Secret can be accepted as the desired environment snapshot.
func TestExternalSecretReferencesMustBePinned(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		change  func(map[string]any)
	}{
		{name: "unversioned", message: "pinned positive KV version", change: func(desired map[string]any) {
			valueAt(desired, "spec", "target", "template", "metadata", "annotations").(map[string]any)["borq.run/env-version"] = "latest"
		}},
		{name: "missing source", message: "one pinned KV document", change: func(desired map[string]any) { delete(desired["spec"].(map[string]any), "dataFrom") }},
		{name: "different source version", message: "must match its pinned identity", change: func(desired map[string]any) {
			valueAt(desired, "spec", "dataFrom").([]any)[0].(map[string]any)["extract"].(map[string]any)["version"] = "1"
		}},
		{name: "different source path", message: "must match its pinned identity", change: func(desired map[string]any) {
			valueAt(desired, "spec", "dataFrom").([]any)[0].(map[string]any)["extract"].(map[string]any)["key"] = "other/path"
		}},
		{name: "null expected keys", message: "requires its expected environment keys", change: func(desired map[string]any) {
			valueAt(desired, "spec", "target", "template", "metadata", "annotations").(map[string]any)["borq.run/env-keys"] = "null"
		}},
		{name: "duplicate expected keys", message: "must be nonempty and unique", change: func(desired map[string]any) {
			valueAt(desired, "spec", "target", "template", "metadata", "annotations").(map[string]any)["borq.run/env-keys"] = `["TOKEN","TOKEN"]`
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := externalSecretManifest()
			test.change(desired)
			live, secret := readyExternalSecret()
			test.change(live)
			client, _ := testClient(live, secret)
			_, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
		})
	}
}

// TestExternalSecretObserveApplyKeepsValuesOutOfDiff verifies the resource can be
// reconciled through the same application API without reading generated Secret data.
func TestExternalSecretObserveApplyKeepsValuesOutOfDiff(t *testing.T) {
	assert := is.New(t)
	desired := externalSecretManifest()
	client, fakeClient := testClient(namespaceManifest())
	diffs, err := client.Observe(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(diffs[0].Action, "create")
	assert.NoErr(client.Apply(context.Background(), []map[string]any{desired}, "target-one"))
	diffs, err = client.Observe(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(diffs[0].Action, "unchanged")
	for _, action := range fakeClient.Actions() {
		assert.True(action.GetResource().Resource != "secrets")
	}
}

// TestApplyRestoresExternalSecretSources verifies manual overrides cannot survive
// server-side apply merely because they were introduced by another field manager.
func TestApplyRestoresExternalSecretSources(t *testing.T) {
	assert := is.New(t)
	desired := externalSecretManifest()
	live := defaultResource(desired)
	live["spec"].(map[string]any)["data"] = []any{map[string]any{"secretKey": "TOKEN", "remoteRef": map[string]any{"key": "other-source"}}}
	valueAt(live, "spec", "target", "template").(map[string]any)["data"] = map[string]any{"TOKEN": "replacement"}
	client, _ := testClient(namespaceManifest(), live)
	diffs, err := client.Observe(context.Background(), []map[string]any{desired}, "target-one")
	assert.NoErr(err)
	assert.Equal(diffs[0].Action, "update")
	assert.True(valueAt(diffs[0].Desired, "spec", "data") == nil)
	assert.NoErr(client.Apply(context.Background(), []map[string]any{desired}, "target-one"))
	resource := schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}
	current, err := client.dynamic.Resource(resource).Namespace("shop").Get(context.Background(), "frontend-env-2", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(current.Object["spec"], desired["spec"])
}

func externalSecretManifest() map[string]any {
	labels := map[string]any{targetLabel: "target-one", "app.kubernetes.io/name": "frontend", "app.kubernetes.io/managed-by": "borq"}
	return map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": "frontend-env-2", "namespace": "shop", "labels": labels},
		"spec": map[string]any{
			"secretStoreRef": map[string]any{"kind": "ClusterSecretStore", "name": "borq-openbao"}, "refreshPolicy": "OnChange",
			"target": map[string]any{"name": "frontend-env-2", "creationPolicy": "Owner", "deletionPolicy": "Retain", "immutable": true,
				"template": map[string]any{"type": "Opaque", "metadata": map[string]any{"labels": copyMap(labels), "annotations": map[string]any{
					"borq.run/env-version": "2", "borq.run/env-path": "org/project/environment/frontend", "borq.run/env-keys": `["EMPTY","TOKEN"]`,
				}}}},
			"dataFrom": []any{map[string]any{"extract": map[string]any{"key": "org/project/environment/frontend", "version": "2", "property": "values"}}},
		},
	}
}

func readyExternalSecret() (map[string]any, map[string]any) {
	live := externalSecretManifest()
	live["metadata"].(map[string]any)["uid"] = "external-secret-current"
	live["metadata"].(map[string]any)["generation"] = int64(2)
	live["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	metadata := copyMap(valueAt(live, "spec", "target", "template", "metadata").(map[string]any))
	metadata["name"] = "frontend-env-2"
	metadata["namespace"] = "shop"
	metadata["ownerReferences"] = []any{map[string]any{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "name": "frontend-env-2", "uid": "external-secret-current", "controller": true}}
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata, "type": "Opaque", "immutable": true, "data": map[string]any{"EMPTY": "", "TOKEN": "not-output"}}
	return live, secret
}
