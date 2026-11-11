package platform

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

// managementAdminEgressName is shared by provisioning and teardown of this proxy.
const managementAdminEgressName = "borq-management-admin"

func adminEgressService(object unstructured.Unstructured) bool {
	return object.GetNamespace() == SystemNamespace && object.GetName() == managementAdminEgressName
}

// The egress Service lives with the shared operator, outside the management
// namespace. Only this exact Service belongs to management teardown.
func (state *destroyState) captureAdminEgress(ctx context.Context, plan *ManagementDestroyPlan, accepted ManagementDestroyPlan) error {
	resource := schema.GroupVersionResource{Version: "v1", Resource: "services"}
	service, err := state.client.Resource(resource).Namespace(SystemNamespace).Get(ctx, managementAdminEgressName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return state.waitAdminEgressCleanup(ctx)
	}
	if err != nil {
		return err
	}
	serviceType, _, _ := unstructured.NestedString(service.Object, "spec", "type")
	if !destroyOwned(*service, "services") || serviceType != "ExternalName" {
		return errors.New("management admin egress Service is foreign; refusing teardown")
	}
	bound := false
	for _, server := range state.serverObjects {
		if server.resource.Resource == "clusterrolebindings" && server.object.GetName() == "borq" {
			bound = true // prepareDestroy already verified the exact role and namespace subject.
		}
	}
	if !bound {
		return errors.New("management admin egress cleanup requires this installation's bootstrap binding")
	}
	identity := destroyIdentity(destroyObject{resource, *service})
	associated := false
	for _, previous := range accepted.Resources {
		if sameDestroyIdentities([]ManagementDestroyIdentity{identity}, []ManagementDestroyIdentity{previous}, false) {
			associated = true // Retain the original proof after the ingress has been removed.
		}
	}
	if !associated {
		for _, item := range state.objects {
			if item.resource.Resource == "ingresses" && item.object.GetName() == "borq-management" && adminEgressMatchesIngress(*service, item.object) {
				associated = true
			}
		}
	}
	if !associated {
		return errors.New("management admin egress target does not match this installation's recorded ingress")
	}
	state.objects = append(state.objects, destroyObject{resource, *service})
	plan.Resources = append(plan.Resources, identity)
	plan.Actions = append(plan.Actions, ManagementDestroyAction{"remove and wait for Tailscale proxy cleanup", "Service", SystemNamespace, service.GetName()})
	return nil
}

func adminEgressMatchesIngress(service, ingress unstructured.Unstructured) bool {
	target := strings.TrimSuffix(strings.ToLower(service.GetAnnotations()["tailscale.com/tailnet-fqdn"]), ".")
	if target == "" || !strings.HasSuffix(target, ".ts.net") {
		return false
	}
	addresses, _, _ := unstructured.NestedSlice(ingress.Object, "status", "loadBalancer", "ingress")
	for _, entry := range addresses {
		address, ok := entry.(map[string]any)
		hostname, _ := address["hostname"].(string)
		if ok && strings.EqualFold(strings.TrimSuffix(hostname, "."), target) {
			return true
		}
	}
	for _, key := range []string{"borq.run/proven-tailnet-url", "borq.run/previous-tailnet-url"} {
		address, err := url.Parse(ingress.GetAnnotations()[key])
		if err == nil && address.Scheme == "https" && address.User == nil && strings.EqualFold(address.Hostname(), target) {
			return true
		}
	}
	return false
}

func (state destroyState) waitAdminEgressCleanup(ctx context.Context) error {
	err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		return adminEgressChildrenAbsent(ctx, state.client)
	})
	if err != nil {
		return fmt.Errorf("wait for management Tailscale proxy cleanup: %w", err)
	}
	return nil
}

// The operator owns its StatefulSet, Pods, headless Service, state Secret and
// tailnet device. Observe its cleanup; never delete shared controller resources.
func adminEgressChildrenAbsent(ctx context.Context, client dynamic.Interface) (bool, error) {
	_, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, SystemNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	selector := labels.Set{"tailscale.com/managed": "true", "tailscale.com/parent-resource": managementAdminEgressName, "tailscale.com/parent-resource-ns": SystemNamespace, "tailscale.com/parent-resource-type": "svc"}.AsSelector().String()
	for _, resource := range []schema.GroupVersionResource{{Group: "apps", Version: "v1", Resource: "statefulsets"}, {Version: "v1", Resource: "pods"}, {Version: "v1", Resource: "services"}, {Version: "v1", Resource: "secrets"}} {
		objects, err := client.Resource(resource).Namespace(SystemNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(objects.Items) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func verifyAdminEgressAbsent(ctx context.Context, client dynamic.Interface) error {
	_, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"}).Namespace(SystemNamespace).Get(ctx, managementAdminEgressName, metav1.GetOptions{})
	if err == nil {
		return errors.New("management admin egress Service remains after cleanup")
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	absent, err := adminEgressChildrenAbsent(ctx, client)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("management admin Tailscale proxy resources remain after cleanup")
	}
	return nil
}
