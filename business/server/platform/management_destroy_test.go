package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestDestroyOwnership refuses generic borrowed labels and requires the persisted source identities used by each owner.
func TestDestroyOwnership(t *testing.T) {
	assert := is.New(t)
	object := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "unrelated", "labels": map[string]any{"app.kubernetes.io/managed-by": "borq"}}}}
	assert.True(!destroyOwned(object, "services"))
	object.SetName("borq")
	object.SetLabels(destroyTestLabels("borq", "server"))
	assert.True(destroyOwned(object, "services"))
	object.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	assert.True(!destroyOwned(object, "services"))
	_, err := FinalizeManagementServer(t.Context(), ManagementDestroyInput{}, ManagementDestroyReceipt{})
	assert.True(err != nil && strings.Contains(err.Error(), "explicit --kubeconfig"))
}

// TestDestroyLiveAuthorityGuard reaches the live recheck after a same-UID binding or tunnel receipt changes, before any delete.
func TestDestroyLiveAuthorityGuard(t *testing.T) {
	for _, kind := range []string{"binding subjects", "tunnel receipt", "foreign tunnel"} {
		t.Run(kind, func(t *testing.T) {
			assert := is.New(t)
			resource := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
			object := unstructured.Unstructured{Object: map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": "borq", "uid": "original", "resourceVersion": "1"}, "roleRef": map[string]any{"kind": "ClusterRole", "name": "borq"}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "borq", "namespace": "management"}}}}
			object.SetLabels(destroyTestLabels("borq", "rbac"))
			if kind != "binding subjects" {
				resource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
				object.SetAPIVersion("v1")
				object.SetKind("ConfigMap")
				object.SetName("borq-cloudflared")
				object.SetNamespace("management")
				object.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "borq.run/network": "true"})
				object.Object["data"] = map[string]any{"tunnelId": "original-tunnel"}
			}
			live := object.DeepCopy()
			switch kind {
			case "binding subjects":
				live.Object["subjects"] = append(live.Object["subjects"].([]any), map[string]any{"kind": "ServiceAccount", "name": "foreign"})
			case "tunnel receipt":
				live.Object["data"].(map[string]any)["tunnelId"] = "replacement"
			case "foreign tunnel":
				live.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "someone-else"})
			}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), live)
			err := (destroyState{client: client, namespace: "management"}).remove(t.Context(), destroyObject{resource, object})
			assert.True(err != nil && strings.Contains(err.Error(), "changed"))
			for _, action := range client.Actions() {
				assert.True(action.GetVerb() != "delete")
			}
		})
	}
}

// TestDestroyWaitsForTerminatingPods treats Deployment status as insufficient evidence that its reconciler process stopped.
func TestDestroyWaitsForTerminatingPods(t *testing.T) {
	assert := is.New(t)
	object := unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "borq", "namespace": "management", "uid": "original", "generation": int64(1)}, "spec": map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "borq"}}}, "status": map[string]any{"observedGeneration": int64(1), "replicas": int64(0)}}}
	object.SetLabels(destroyTestLabels("borq", "server"))
	pod := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "terminating", "namespace": "management", "labels": map[string]any{"app": "borq"}, "deletionTimestamp": metav1.Now().Format(time.RFC3339)}}}
	resource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList"}, &object, &pod)
	state := destroyState{client: client, namespace: "management"}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	item := destroyObject{resource, object}
	assert.True(state.scale(ctx, &item, 0, false) != nil)
}

// TestDestroySharedGateway rejects a foreign route even when it references the Gateway across namespaces.
func TestDestroySharedGateway(t *testing.T) {
	assert := is.New(t)
	gateway := unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]any{"name": "borq-management-public", "namespace": "management", "uid": "owned-gateway"}}}
	route := unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": map[string]any{"name": "foreign", "namespace": "other", "uid": "foreign-route"}, "spec": map[string]any{"parentRefs": []any{map[string]any{"name": "borq-management-public", "namespace": "management"}}}}}
	kinds := map[schema.GroupVersionResource]string{}
	for _, entry := range []struct{ version, resource, kind string }{{"v1", "httproutes", "HTTPRouteList"}, {"v1", "grpcroutes", "GRPCRouteList"}, {"v1", "tlsroutes", "TLSRouteList"}, {"v1alpha2", "tlsroutes", "TLSRouteList"}, {"v1alpha2", "tcproutes", "TCPRouteList"}, {"v1alpha2", "udproutes", "UDPRouteList"}} {
		kinds[schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: entry.version, Resource: entry.resource}] = entry.kind
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, &route)
	state := destroyState{client: client, namespace: "management"}
	err := state.checkGatewayReferences(t.Context(), gateway)
	assert.True(err != nil && strings.Contains(err.Error(), "outside this destroy plan"))
	for _, action := range client.Actions() {
		assert.True(action.GetVerb() != "delete" && action.GetVerb() != "update")
	}
}

func destroyTestLabels(name, component string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/component": component, "app.kubernetes.io/managed-by": "borq"}
}

// TestDestroyLiveSpecFence checks the live mutation read, after planning, rather
// than allowing a same-UID workload template or Service selector replacement.
func TestDestroyLiveSpecFence(t *testing.T) {
	for _, resource := range []string{"deployments", "services"} {
		t.Run(resource, func(t *testing.T) {
			assert := is.New(t)
			object := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "borq", "namespace": "management", "uid": "original", "resourceVersion": "1"}, "spec": map[string]any{"selector": map[string]any{"app": "original"}}}}
			object.SetLabels(destroyTestLabels("borq", "server"))
			group := ""
			if resource == "deployments" {
				group = "apps"
				object.SetAPIVersion("apps/v1")
				object.SetKind("Deployment")
				object.Object["spec"] = map[string]any{"replicas": int64(1), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "borq", "image": "original-image"}}}}}
			}
			live := object.DeepCopy()
			if resource == "deployments" {
				assert.NoErr(unstructured.SetNestedSlice(live.Object, []any{map[string]any{"name": "borq", "image": "new-installation-image"}}, "spec", "template", "spec", "containers"))
			} else {
				assert.NoErr(unstructured.SetNestedField(live.Object, "new-target", "spec", "selector", "app"))
			}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), live)
			state := destroyState{client: client, namespace: "management"}
			item := destroyObject{schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}, object}
			var err error
			if resource == "deployments" {
				err = state.scale(t.Context(), &item, 0, false)
			} else {
				err = state.remove(t.Context(), item)
			}
			assert.True(err != nil && strings.Contains(err.Error(), "changed"))
			for _, action := range client.Actions() {
				assert.True(action.GetVerb() != "delete" && action.GetVerb() != "update")
			}
		})
	}
}

// TestDestroyPlanRetryIdentities permits completed removals and paused replicas,
// while keeping image updates and new server incarnations outside the old plan.
func TestDestroyPlanRetryIdentities(t *testing.T) {
	assert := is.New(t)
	core := unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "borq", "namespace": "management", "uid": "server-original", "resourceVersion": "1"}, "spec": map[string]any{"replicas": int64(1), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "borq", "image": "original"}}}}}}}
	core.SetLabels(destroyTestLabels("borq", "server"))
	resource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	expected := ManagementDestroyPlan{Namespace: "management", NamespaceUID: "namespace-original", ClusterUID: "cluster-original", Server: []ManagementDestroyIdentity{destroyIdentity(destroyObject{resource, core})}, Resources: []ManagementDestroyIdentity{{Version: "v1", Resource: "services", Namespace: "management", Name: "removed", UID: "removed-uid", OwnershipHash: "removed-hash"}}}
	state := destroyState{namespaceUID: expected.NamespaceUID, clusterUID: expected.ClusterUID}
	paused := core.DeepCopy()
	assert.NoErr(unstructured.SetNestedField(paused.Object, int64(0), "spec", "replicas"))
	paused.SetResourceVersion("2")
	current := expected
	current.Server = []ManagementDestroyIdentity{destroyIdentity(destroyObject{resource, *paused})}
	current.Resources = nil
	assert.NoErr(validateDestroyPlan(state, current, expected))
	current.Executed = true
	comparison := expected
	comparison.Executed = true
	assert.True(sameDestroyPlan(expected, comparison))
	paused.SetUID("replacement-server")
	current.Server = []ManagementDestroyIdentity{destroyIdentity(destroyObject{resource, *paused})}
	assert.True(validateDestroyPlan(state, current, expected) != nil)
}

// TestDestroyReceiptPlanComparison prevents a completion receipt from adopting
// another operation's resource plan through duplicate identities or changed scope.
func TestDestroyReceiptPlanComparison(t *testing.T) {
	assert := is.New(t)
	expected := ManagementDestroyPlan{Namespace: "management", NamespaceUID: "namespace", ClusterUID: "cluster",
		Server: []ManagementDestroyIdentity{{Version: "v1", Resource: "services", Namespace: "management", Name: "borq", UID: "service", ResourceVersion: "1", OwnershipHash: "service-hash"},
			{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "management", Name: "borq", UID: "deployment", ResourceVersion: "1", OwnershipHash: "deployment-hash"}}}
	completed := expected
	completed.Executed = true
	completed.Server = append([]ManagementDestroyIdentity{}, expected.Server...)
	completed.Server[0].ResourceVersion = "new-status-version"
	assert.True(sameDestroyPlan(completed, expected))
	completed.Server[0].Kind = "changed-kind"
	assert.True(!sameDestroyPlan(completed, expected))
	completed.Server[0].Kind = expected.Server[0].Kind
	completed.Server[1] = completed.Server[0]
	assert.True(!sameDestroyPlan(completed, expected))
	completed = expected
	completed.NamespaceUID = "new-namespace"
	assert.True(!sameDestroyPlan(completed, expected))
}

// TestDestroyRetainedProviderScope fences a same-UID configuration update at the
// fresh mutation boundary while retaining credential rotation and all objects.
func TestDestroyRetainedProviderScope(t *testing.T) {
	assert := is.New(t)
	dependency := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "borq-network-settings", "namespace": "management", "uid": "original-settings"}, "data": map[string]any{"network.json": `{"cloudflare":{"zoneId":"original-zone","wildcardDomain":"original.test"}}`}}}
	dependency.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq"})
	resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	expected := destroyIdentity(destroyObject{resource, dependency})
	live := dependency.DeepCopy()
	assert.NoErr(unstructured.SetNestedField(live.Object, `{"cloudflare":{"zoneId":"new-zone","wildcardDomain":"new.test"}}`, "data", "network.json"))
	object := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "borq", "namespace": "management", "uid": "original-service"}}}
	object.SetLabels(destroyTestLabels("borq", "server"))
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), live, &object)
	state := destroyState{client: client, namespace: "management", dependencies: []ManagementDestroyIdentity{expected}}
	err := state.remove(t.Context(), destroyObject{schema.GroupVersionResource{Version: "v1", Resource: "services"}, object})
	assert.True(err != nil && strings.Contains(err.Error(), "retained provider configuration changed"))
	for _, action := range client.Actions() {
		assert.True(action.GetVerb() != "delete" && action.GetVerb() != "update")
	}
	plan := ManagementDestroyPlan{Namespace: "management", NamespaceUID: "namespace", ClusterUID: "cluster", Dependencies: []ManagementDestroyIdentity{expected}}
	changed := plan
	changed.Dependencies = []ManagementDestroyIdentity{destroyIdentity(destroyObject{resource, *live})}
	assert.True(!sameDestroyPlan(changed, plan))
	assert.True(validateDestroyPlan(destroyState{namespaceUID: "namespace", clusterUID: "cluster"}, changed, plan) != nil)
	t.Run("configuration appeared after completion", func(t *testing.T) {
		assert := is.New(t)
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &dependency, &object)
		err := (destroyState{client: client, namespace: "management"}).remove(t.Context(), destroyObject{schema.GroupVersionResource{Version: "v1", Resource: "services"}, object})
		assert.True(err != nil && strings.Contains(err.Error(), "retained provider configuration changed"))
		for _, action := range client.Actions() {
			assert.True(action.GetVerb() != "delete" && action.GetVerb() != "update")
		}
	})
	t.Run("missing configuration is not a missing receipt", func(t *testing.T) {
		assert := is.New(t)
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &object)
		err := (destroyState{client: client, namespace: "management", dependencies: []ManagementDestroyIdentity{expected}}).verifyDependencies(t.Context())
		assert.True(err != nil && strings.Contains(err.Error(), "configuration disappeared"))
		assert.True(!apierrors.IsNotFound(err))
		assert.NoErr((destroyState{client: client, namespace: "management"}).verifyDependencies(t.Context()))
	})
}

// TestDestroySharedStableTlsRoute protects routes served only at Gateway API v1;
// the retired v1alpha2 API cannot be used to infer that no foreign TLS route exists.
func TestDestroySharedStableTlsRoute(t *testing.T) {
	assert := is.New(t)
	gateway := unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]any{"name": "borq-management-public", "namespace": "management", "uid": "gateway"}}}
	route := unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "TLSRoute", "metadata": map[string]any{"name": "foreign", "namespace": "other", "uid": "foreign"}, "spec": map[string]any{"parentRefs": []any{map[string]any{"name": "borq-management-public", "namespace": "management"}}}}}
	kinds := map[schema.GroupVersionResource]string{}
	for _, entry := range []struct{ version, resource, kind string }{{"v1", "httproutes", "HTTPRouteList"}, {"v1", "grpcroutes", "GRPCRouteList"}, {"v1", "tlsroutes", "TLSRouteList"}, {"v1alpha2", "tlsroutes", "TLSRouteList"}, {"v1alpha2", "tcproutes", "TCPRouteList"}, {"v1alpha2", "udproutes", "UDPRouteList"}} {
		kinds[schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: entry.version, Resource: entry.resource}] = entry.kind
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, &route)
	err := (destroyState{client: client, namespace: "management"}).checkGatewayReferences(t.Context(), gateway)
	assert.True(err != nil && strings.Contains(err.Error(), "outside this destroy plan"))
	for _, action := range client.Actions() {
		assert.True(action.GetVerb() != "delete" && action.GetVerb() != "update")
	}
}
