package platform

import (
	"strings"
	"testing"

	"github.com/matryer/is"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestDestroyAdminEgressAssociation binds the one cross-namespace Service to the
// validated bootstrap authority and the selected installation's recorded target.
func TestDestroyAdminEgressAssociation(t *testing.T) {
	for _, scenario := range []string{"legacy", "partial retry", "operator route update", "foreign", "missing binding", "wrong target", "retargeted retry"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			service := destroyAdminEgressFixture()
			accepted := ManagementDestroyPlan{}
			ingress := unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]any{"namespace": "management", "name": "borq-management"}, "status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"hostname": "borq-production-5.example.ts.net"}}}}}}
			state := destroyState{namespace: "management", serverObjects: []destroyObject{{resource: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, object: unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "borq"}}}}}, objects: []destroyObject{{resource: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, object: ingress}}}
			wanted := ""
			switch scenario {
			case "partial retry", "operator route update", "retargeted retry":
				accepted.Resources = []ManagementDestroyIdentity{destroyIdentity(destroyObject{schema.GroupVersionResource{Version: "v1", Resource: "services"}, service})}
				state.objects = nil // The ingress was removed before the interruption.
				if scenario == "operator route update" {
					assert.NoErr(unstructured.SetNestedField(service.Object, "ts-operator-assigned.borq-system.svc.cluster.local", "spec", "externalName"))
				}
				if scenario == "retargeted retry" {
					service.SetAnnotations(map[string]string{"tailscale.com/tailnet-fqdn": "other.example.ts.net"})
					wanted = "does not match"
				}
			case "foreign":
				service.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "someone-else"})
				wanted = "foreign"
			case "missing binding":
				state.serverObjects = nil
				wanted = "bootstrap binding"
			case "wrong target":
				service.SetAnnotations(map[string]string{"tailscale.com/tailnet-fqdn": "other.example.ts.net"})
				wanted = "does not match"
			}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &service)
			state.client = client
			plan := ManagementDestroyPlan{}
			err := state.captureAdminEgress(t.Context(), &plan, accepted)
			if wanted == "" {
				assert.NoErr(err)
				assert.Equal(len(plan.Resources), 1)
				assert.Equal(plan.Resources[0].Namespace, SystemNamespace)
				assert.Equal(plan.Resources[0].UID, "egress-original")
			} else {
				assert.True(err != nil && strings.Contains(err.Error(), wanted))
				assert.Equal(len(plan.Resources), 0)
			}
			for _, action := range client.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
}

// TestDestroyAdminEgressRetarget rejects a same-UID target swap on the final live
// read immediately preceding DELETE, rather than relying on an earlier plan.
func TestDestroyAdminEgressRetarget(t *testing.T) {
	assert := is.New(t)
	service := destroyAdminEgressFixture()
	live := service.DeepCopy()
	live.SetAnnotations(map[string]string{"tailscale.com/tailnet-fqdn": "other.example.ts.net"})
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), live)
	err := (destroyState{namespace: "management", client: client}).remove(t.Context(), destroyObject{schema.GroupVersionResource{Version: "v1", Resource: "services"}, service})
	assert.True(err != nil && strings.Contains(err.Error(), "ownership changed"))
	for _, action := range client.Actions() {
		assert.Equal(action.GetVerb(), "get")
	}
}

func destroyAdminEgressFixture() unstructured.Unstructured {
	service := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "spec": map[string]any{"type": "ExternalName", "externalName": "ts-borq-management-admin-original.borq-system.svc.cluster.local", "ports": []any{map[string]any{"name": "https", "port": int64(443), "targetPort": int64(443), "protocol": "TCP"}}}}}
	service.SetNamespace(SystemNamespace)
	service.SetName("borq-management-admin")
	service.SetUID(types.UID("egress-original"))
	service.SetResourceVersion("1")
	service.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "borq.run/network": "true"})
	service.SetAnnotations(map[string]string{"tailscale.com/tailnet-fqdn": "borq-production-5.example.ts.net", "tailscale.com/tags": "tag:borq-admin"})
	service.SetCreationTimestamp(metav1.Now())
	return service
}
