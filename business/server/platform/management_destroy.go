package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/stefanmcshane/borq/foundations/cloudflare"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

// ManagementDestroyInput identifies the management installation and durable teardown operation.
type ManagementDestroyInput struct {
	// KubeconfigPath is optional only for the server, whose pod credentials select its own cluster.
	KubeconfigPath string
	// Namespace selects the management installation; empty uses borq-mgmt.
	Namespace string
	// OperationID binds completion to one durably accepted server operation.
	OperationID string
	// Timeout bounds cleanup and controller finalizer waits.
	Timeout time.Duration
}

// ManagementDestroyAction describes one scoped cleanup operation without private settings.
type ManagementDestroyAction struct {
	Action    string `json:"action"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ManagementDestroyIdentity fences one exact resource without exporting its configuration.
type ManagementDestroyIdentity struct {
	Group           string `json:"group"`
	Version         string `json:"version"`
	Resource        string `json:"resource"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	OwnershipHash   string `json:"ownershipHash"`
}

// ManagementDestroyPlan binds cleanup and final removal to immutable installation identities.
type ManagementDestroyPlan struct {
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUid"`
	ClusterUID   string `json:"clusterUid"`
	// Server is retained by server cleanup and removed only by the separate finalizer.
	Server    []ManagementDestroyIdentity `json:"server"`
	Resources []ManagementDestroyIdentity `json:"resources"`
	// Dependencies binds retained provider scope without granting authority to delete it.
	Dependencies []ManagementDestroyIdentity `json:"dependencies"`
	Actions      []ManagementDestroyAction   `json:"actions"`
	Retained     []string                    `json:"retained"`
	Executed     bool                        `json:"executed"`
}

// ManagementDestroyReceipt proves server cleanup completed before CLI-only server removal.
type ManagementDestroyReceipt struct {
	OperationID string                `json:"operationId"`
	Plan        ManagementDestroyPlan `json:"plan"`
}

type destroyObject struct {
	resource schema.GroupVersionResource
	object   unstructured.Unstructured
}
type destroyState struct {
	client                   dynamic.Interface
	namespace                string
	objects                  []destroyObject
	dependencies             []ManagementDestroyIdentity
	serverObjects            []destroyObject
	clusterUID, namespaceUID string
	dns                      cloudflare.Client
	dnsInput                 cloudflare.DnsCleanupInput
	tunnel                   cloudflare.Tunnel
	owner, domain            string
	externalDns              *destroyObject // The optional controller is quiesced, then restored after all DNS sources are gone.
	tunnelState              *destroyObject // The receipt survives errors and is removed only after verified provider deletion.
}

// PlanManagementDestroy performs read-only resource and provider ownership checks.
func PlanManagementDestroy(ctx context.Context, input ManagementDestroyInput) (ManagementDestroyPlan, error) {
	input, err := normalizeManagementDestroy(input)
	if err != nil {
		return ManagementDestroyPlan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	_, plan, err := prepareDestroy(ctx, input, ManagementDestroyPlan{})
	return plan, err
}

// DestroyManagementResources removes managed exposure while retaining Borq itself.
// The caller must first durably stop and drain its in-process reconcilers.
func DestroyManagementResources(ctx context.Context, input ManagementDestroyInput, expected ManagementDestroyPlan) (ManagementDestroyPlan, error) {
	input, err := normalizeManagementDestroy(input)
	if err != nil {
		return ManagementDestroyPlan{}, err
	}
	if input.OperationID == "" {
		return ManagementDestroyPlan{}, errors.New("a durable management destroy operation ID is required")
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	if receipt, err := ReadManagementDestroyReceipt(ctx, input); err == nil {
		if !sameDestroyPlan(receipt.Plan, expected) {
			return expected, errors.New("completed destroy receipt belongs to another installation plan")
		}
		return receipt.Plan, nil
	} else if !apierrors.IsNotFound(err) && !errors.Is(err, ErrManagementDestroyReceiptStale) {
		return expected, err
	}
	state, result, err := prepareDestroy(ctx, input, expected)
	if err != nil {
		return result, err
	}
	if err := validateDestroyPlan(state, result, expected); err != nil {
		return result, err
	}
	for index := range state.objects {
		item := &state.objects[index]
		if item.resource.Resource == "deployments" {
			if err := state.scale(ctx, item, 0, false); err != nil {
				return result, err
			}
		}
	}
	if state.externalDns != nil {
		if err := state.scale(ctx, state.externalDns, 0, true); err != nil {
			return result, err
		}
	}
	if state.domain != "" {
		state.dnsInput.Confirm = true
		if _, err := state.dns.CleanupExternalDns(ctx, state.dnsInput); err != nil {
			return result, fmt.Errorf("DNS cleanup stopped; network credentials and receipts remain for retry: %w", err)
		}
		if state.tunnel.ID != "" {
			if err := state.dns.CleanupTunnel(ctx, state.tunnel, state.owner, state.namespace, state.domain, true); err != nil {
				return result, fmt.Errorf("tunnel cleanup stopped; its receipt remains for retry: %w", err)
			}
		}
	}
	for _, item := range state.objects {
		if err := state.remove(ctx, item); err != nil {
			return result, err
		}
	}
	if state.tunnelState != nil {
		if err := state.remove(ctx, *state.tunnelState); err != nil {
			return result, err
		}
	}
	if state.externalDns != nil {
		replicas, found, _ := unstructured.NestedString(state.externalDns.object.Object, "metadata", "annotations", "borq.run/destroy-replicas")
		desired := int64(1)
		if found {
			if err := json.Unmarshal([]byte(replicas), &desired); err != nil {
				return result, errors.New("saved ExternalDNS replica count is invalid; controller remains stopped")
			}
		}
		if err := state.scale(ctx, state.externalDns, desired, false); err != nil {
			return result, err
		}
	}
	finalState, finalPlan, err := prepareDestroy(ctx, input, expected)
	if err != nil {
		return result, err
	}
	if err := validateDestroyPlan(finalState, finalPlan, expected); err != nil {
		return result, err
	}
	if len(finalState.objects) != 0 || finalState.tunnelState != nil {
		return result, errors.New("owned management exposure reappeared before cleanup completed")
	}
	result = expected
	result.Executed = true
	if err := writeManagementDestroyReceipt(ctx, state, input.OperationID, result); err != nil {
		return result, err
	}
	return result, nil
}

func normalizeManagementDestroy(input ManagementDestroyInput) (ManagementDestroyInput, error) {
	if input.Namespace == "" {
		input.Namespace = ManagementNamespace
	}
	if len(validation.IsDNS1123Label(input.Namespace)) != 0 {
		return input, errors.New("provide a valid management namespace")
	}
	if input.Timeout <= 0 {
		input.Timeout = 10 * time.Minute
	}
	return input, nil
}

func prepareDestroy(ctx context.Context, request ManagementDestroyInput, accepted ManagementDestroyPlan) (destroyState, ManagementDestroyPlan, error) {
	result := ManagementDestroyPlan{Namespace: request.Namespace, Server: []ManagementDestroyIdentity{}, Resources: []ManagementDestroyIdentity{}, Dependencies: []ManagementDestroyIdentity{}, Actions: []ManagementDestroyAction{}, Retained: []string{"Namespace, all PVCs/PVs, databases and backups", "Borq credentials, admin tokens, network settings and recovery configuration; Tailscale removes its own proxy state", "Shared Helm releases/controllers, cert-manager Certificates/Issuers and ACME challenges", "Kubernetes cluster and infrastructure load balancers shared by retained controllers"}}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: request.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return destroyState{}, result, err
	}
	configuration := connection.RestConfig()
	client, err := dynamic.NewForConfig(&configuration)
	if err != nil {
		return destroyState{}, result, err
	}
	state := destroyState{client: client, namespace: request.Namespace}
	namespace, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, request.Namespace, metav1.GetOptions{})
	if err != nil || namespace.GetUID() == "" {
		return state, result, errors.New("read selected management namespace identity before destroy")
	}
	state.namespaceUID, result.NamespaceUID = string(namespace.GetUID()), string(namespace.GetUID())
	system, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || system.GetUID() == "" {
		return state, result, errors.New("read physical cluster identity before destroy")
	}
	state.clusterUID, result.ClusterUID = string(system.GetUID()), string(system.GetUID())
	state.owner = "borq-" + state.clusterUID
	groups := []schema.GroupVersionResource{{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}, {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}, {Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}, {Version: "v1", Resource: "services"}, {Group: "apps", Version: "v1", Resource: "deployments"}, {Version: "v1", Resource: "serviceaccounts"}}
	targets := []string{}
	for _, resource := range groups {
		list, err := client.Resource(resource).Namespace(request.Namespace).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return state, result, fmt.Errorf("read %s before destroy: %w", resource.Resource, err)
		}
		for _, object := range list.Items {
			if resource.Resource == "services" && adminEgressService(object) {
				continue // Captured below with its management installation association.
			}
			if resource.Resource == "deployments" && object.GetName() == "external-dns" {
				if object.GetAnnotations()["meta.helm.sh/release-name"] == "external-dns" && object.GetAnnotations()["meta.helm.sh/release-namespace"] == request.Namespace && object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" {
					item := destroyObject{resource, object}
					state.externalDns = &item
				}
				continue
			}
			if !destroyOwned(object, resource.Resource) {
				if destroyServerResource(resource.Resource, object.GetName()) || (object.GetName() == "borq-cloudflared" && resource.Resource == "deployments") {
					return state, result, fmt.Errorf("%s/%s is not owned by this Borq installation", resource.Resource, object.GetName())
				}
				if resource.Resource == "dnsendpoints" && object.GetLabels()["borq.run/network-dns"] == "true" {
					return state, result, errors.New("unrecognized DNS source prevents safe destroy")
				}
				continue
			}
			item := destroyObject{resource, object}
			if destroyServerResource(resource.Resource, object.GetName()) {
				state.serverObjects = append(state.serverObjects, item)
				result.Server = append(result.Server, destroyIdentity(item))
				result.Actions = append(result.Actions, ManagementDestroyAction{"retain until server cleanup receipt; CLI finalizer removes", object.GetKind(), request.Namespace, object.GetName()})
				continue
			}
			state.objects = append(state.objects, item)
			result.Resources = append(result.Resources, destroyIdentity(item))
			action := "remove"
			if resource.Resource == "deployments" {
				action = "stop, wait for Pods, then remove"
			}
			result.Actions = append(result.Actions, ManagementDestroyAction{action, object.GetKind(), request.Namespace, object.GetName()})
			if resource.Resource == "dnsendpoints" {
				entries, _, err := unstructured.NestedSlice(object.Object, "spec", "endpoints")
				if err != nil {
					return state, result, errors.New("invalid managed DNS endpoints")
				}
				for _, entry := range entries {
					fields, ok := entry.(map[string]any)
					if !ok {
						return state, result, errors.New("invalid managed DNS entry")
					}
					values, _, err := unstructured.NestedStringSlice(fields, "targets")
					if err != nil {
						return state, result, errors.New("invalid managed DNS targets")
					}
					targets = append(targets, values...)
				}
			}
		}
	}
	for _, item := range state.objects {
		if item.resource.Resource == "gateways" {
			if err := state.checkGatewayReferences(ctx, item.object); err != nil {
				return state, result, err
			}
		}
	}
	// Revoke only the exact bootstrap binding; shared ClusterRoles are retained.
	bindingResource := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	binding, err := client.Resource(bindingResource).Get(ctx, "borq", metav1.GetOptions{})
	if err == nil {
		subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
		role, _, _ := unstructured.NestedMap(binding.Object, "roleRef")
		expected := map[string]any{"kind": "ServiceAccount", "name": "borq", "namespace": request.Namespace}
		if !destroyOwned(*binding, "clusterrolebindings") || len(subjects) != 1 || !equalDestroySubject(subjects[0], expected) || role["kind"] != "ClusterRole" || role["name"] != "borq" || role["apiGroup"] != "rbac.authorization.k8s.io" {
			return state, result, errors.New("bootstrap ClusterRoleBinding is shared or foreign; refusing destroy")
		}
		state.serverObjects = append(state.serverObjects, destroyObject{bindingResource, *binding})
		result.Server = append(result.Server, destroyIdentity(destroyObject{bindingResource, *binding}))
		result.Actions = append(result.Actions, ManagementDestroyAction{"revoke", binding.GetKind(), "", binding.GetName()})
	} else if !apierrors.IsNotFound(err) {
		return state, result, err
	}
	if err := state.captureAdminEgress(ctx, &result, accepted); err != nil {
		return state, result, err
	}
	configResource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	tunnelState, tunnelErr := client.Resource(configResource).Namespace(request.Namespace).Get(ctx, "borq-cloudflared", metav1.GetOptions{})
	if tunnelErr != nil && !apierrors.IsNotFound(tunnelErr) {
		return state, result, tunnelErr
	}
	requiresCloudflare := tunnelErr == nil || slices.ContainsFunc(state.objects, func(item destroyObject) bool {
		return item.resource.Resource == "dnsendpoints" || item.object.GetName() == "borq-cloudflared" || item.object.GetName() == "borq-management-public"
	})
	settings, err := client.Resource(configResource).Namespace(request.Namespace).Get(ctx, "borq-network-settings", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if requiresCloudflare {
			return state, result, errors.New("network settings missing; restore them before cleaning up provider resources")
		}
		state.externalDns = nil
		return state, result, nil
	}
	if err != nil {
		return state, result, err
	}
	if settings.GetLabels()["app.kubernetes.io/managed-by"] != "borq" || len(settings.GetOwnerReferences()) != 0 || settings.GetDeletionTimestamp() != nil {
		return state, result, errors.New("network settings are not owned by Borq")
	}
	dependency := destroyIdentity(destroyObject{configResource, *settings})
	state.dependencies = append(state.dependencies, dependency)
	result.Dependencies = append(result.Dependencies, dependency)
	text, _, _ := unstructured.NestedString(settings.Object, "data", "network.json")
	var network NetworkSettings
	if json.Unmarshal([]byte(text), &network) != nil || ValidateNetworkSettings(network) != nil {
		return state, result, errors.New("stored network settings are invalid")
	}
	if network.Cloudflare == nil {
		if requiresCloudflare {
			return state, result, errors.New("provider receipts or DNS sources require stored Cloudflare configuration before destroy")
		}
		state.externalDns = nil
		return state, result, nil
	}
	hasConnector := slices.ContainsFunc(state.objects, func(item destroyObject) bool {
		return item.resource.Resource == "deployments" && item.object.GetName() == "borq-cloudflared"
	})
	if apierrors.IsNotFound(tunnelErr) && (hasConnector || (network.Cloudflare.ExposureMode() == "tunnel" && requiresCloudflare)) {
		return state, result, errors.New("Cloudflare tunnel receipt is missing; restore it before provider cleanup")
	}
	state.domain = network.Cloudflare.WildcardDomain
	secret, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(request.Namespace).Get(ctx, "borq-network-credentials", metav1.GetOptions{})
	if err != nil || secret.GetLabels()["app.kubernetes.io/managed-by"] != "borq" {
		return state, result, errors.New("owned Cloudflare credentials are required before destroy")
	}
	// Decode Kubernetes's base64 wire representation only at this private boundary.
	encoded, err := json.Marshal(secret.Object)
	if err != nil {
		return state, result, err
	}
	var credentials struct {
		Data map[string][]byte `json:"data"`
	}
	if json.Unmarshal(encoded, &credentials) != nil {
		return state, result, errors.New("stored network credentials are invalid")
	}
	state.dns, err = cloudflare.New(cloudflare.Config{ZoneID: network.Cloudflare.ZoneID, ApiToken: string(credentials.Data["cloudflareApiToken"])})
	if err != nil {
		return state, result, err
	}
	err = tunnelErr
	if err == nil {
		if !destroyNetworkOwned(*tunnelState) {
			return state, result, errors.New("Cloudflare tunnel receipt is not owned by Borq")
		}
		data, _, _ := unstructured.NestedStringMap(tunnelState.Object, "data")
		if data["domain"] != state.domain {
			return state, result, errors.New("Cloudflare tunnel receipt domain differs from installed networking")
		}
		state.tunnel = cloudflare.Tunnel{ID: data["tunnelId"], AccountID: data["accountId"], Name: data["name"]}
		if err := state.dns.CleanupTunnel(ctx, state.tunnel, state.owner, state.namespace, state.domain, false); err != nil {
			return state, result, err
		}
		item := destroyObject{configResource, *tunnelState}
		state.tunnelState = &item
		result.Resources = append(result.Resources, destroyIdentity(item))
		targets = append(targets, state.tunnel.ID+".cfargotunnel.com")
		result.Actions = append(result.Actions, ManagementDestroyAction{"delete verified Cloudflare tunnel", "CloudflareTunnel", "", state.tunnel.ID})
	} else if !apierrors.IsNotFound(err) {
		return state, result, err
	}
	if network.Cloudflare.DirectTarget != "" {
		targets = append(targets, network.Cloudflare.DirectTarget)
	}
	state.dnsInput = cloudflare.DnsCleanupInput{OwnerID: state.owner, Namespace: request.Namespace, Domain: state.domain, Targets: targets}
	dnsRecords, err := state.dns.CleanupExternalDns(ctx, state.dnsInput)
	if err != nil {
		return state, result, err
	}
	hasPublicRoute := slices.ContainsFunc(state.objects, func(item destroyObject) bool { return item.object.GetName() == "borq-management-public" })
	if state.tunnel.ID == "" && (hasConnector || (network.Cloudflare.ExposureMode() == "tunnel" && (len(dnsRecords) > 0 || hasPublicRoute))) {
		return state, result, errors.New("Cloudflare tunnel receipt is missing; restore it before provider cleanup")
	}
	for _, record := range dnsRecords {
		result.Actions = append(result.Actions, ManagementDestroyAction{"delete verified Cloudflare record", record.Type, "", record.Name})
	}
	if state.externalDns == nil {
		deployment, err := client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(request.Namespace).Get(ctx, "external-dns", metav1.GetOptions{})
		if err == nil && deployment != nil {
			return state, result, errors.New("ExternalDNS controller is not owned by the configured management release")
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return state, result, err
		}
	} else {
		result.Resources = append(result.Resources, destroyIdentity(*state.externalDns))
		result.Actions = append(result.Actions, ManagementDestroyAction{"pause during cleanup, then resume", "Deployment", request.Namespace, "external-dns"})
	}
	return state, result, nil
}

func equalDestroySubject(actual any, expected map[string]any) bool {
	fields, ok := actual.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range expected {
		if fields[key] != value {
			return false
		}
	}
	return true
}
func destroyNetworkOwned(object unstructured.Unstructured) bool {
	labels := object.GetLabels()
	return labels["app.kubernetes.io/managed-by"] == "borq" && labels["app.kubernetes.io/part-of"] == "borq" && labels["borq.run/network"] == "true"
}
func destroyOwned(object unstructured.Unstructured, resource string) bool {
	labels, name := object.GetLabels(), object.GetName()
	if labels["app.kubernetes.io/managed-by"] != "borq" || len(object.GetOwnerReferences()) != 0 {
		return false
	}
	if resource == "dnsendpoints" {
		source, host := object.GetAnnotations()["borq.run/source"], object.GetAnnotations()["borq.run/hostname"]
		digest := sha256.Sum256([]byte(source))
		sourceID := hex.EncodeToString(digest[:16])
		endpointDigest := sha256.Sum256([]byte(source + "/" + host))
		return source != "" && host != "" && labels["borq.run/source-id"] == sourceID && name == "borq-"+hex.EncodeToString(endpointDigest[:16]) && labels["borq.run/network-dns"] == "true"
	}
	if instance := labels["borq.io/pocket-id"]; len(instance) == 32 && name == "pocketid-"+instance && labels["app.kubernetes.io/name"] == "pocket-id" && object.GetAnnotations()["borq.io/pocket-id-issuer"] != "" {
		_, err := hex.DecodeString(instance)
		return err == nil
	}
	if destroyNetworkOwned(object) {
		if resource == "services" && adminEgressService(object) {
			return true
		}
		return slices.Contains([]string{"borq-management", "borq-management-public", "borq-cloudflared"}, name)
	}
	return (name == "borq" || name == "borq-telemetry") && labels["app.kubernetes.io/name"] == "borq" && slices.Contains([]string{"server", "telemetry", "rbac"}, labels["app.kubernetes.io/component"])
}

func (state destroyState) scale(ctx context.Context, item *destroyObject, replicas int64, remember bool) error {
	if err := state.verifyDependencies(ctx); err != nil {
		return err
	}
	resource := state.client.Resource(item.resource).Namespace(item.object.GetNamespace())
	current, err := resource.Get(ctx, item.object.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.GetUID() != item.object.GetUID() || !sameDestroyOwner(*current, item.object) || destroyOwnershipHash(*current) != destroyOwnershipHash(item.object) {
		return errors.New("workload ownership changed during destroy")
	}
	if remember {
		annotations := current.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if _, exists := annotations["borq.run/destroy-replicas"]; !exists {
			previous, found, _ := unstructured.NestedInt64(current.Object, "spec", "replicas")
			if !found {
				previous = 1
			}
			annotations["borq.run/destroy-replicas"] = fmt.Sprint(previous)
			current.SetAnnotations(annotations)
		}
	}
	if !remember {
		annotations := current.GetAnnotations()
		delete(annotations, "borq.run/destroy-replicas")
		current.SetAnnotations(annotations)
	}
	if err := unstructured.SetNestedField(current.Object, replicas, "spec", "replicas"); err != nil {
		return err
	}
	updated, err := resource.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("stop or restore %s: %w", current.GetName(), err)
	}
	item.object = *updated
	if replicas != 0 {
		return nil
	}
	return wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current, err := resource.Get(ctx, item.object.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.GetUID() != item.object.GetUID() || destroyOwnershipHash(*current) != destroyOwnershipHash(item.object) {
			return false, errors.New("workload replaced or changed while stopping")
		}
		desired, found, err := unstructured.NestedInt64(current.Object, "spec", "replicas")
		if err != nil || !found || desired != 0 {
			return false, errors.New("workload was reactivated during destroy")
		}
		observed, _, _ := unstructured.NestedInt64(current.Object, "status", "observedGeneration")
		running, _, _ := unstructured.NestedInt64(current.Object, "status", "replicas")
		if running != 0 || observed < current.GetGeneration() {
			return false, nil
		}
		selector, _, err := unstructured.NestedStringMap(current.Object, "spec", "selector", "matchLabels")
		if err != nil || len(selector) == 0 {
			return false, errors.New("workload has no safe pod selector")
		}
		pods, err := state.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(state.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set(selector).String()})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 0 {
			return false, nil
		}
		item.object = *current
		return true, nil
	})
}
func sameDestroyOwner(current, previous unstructured.Unstructured) bool {
	if !reflect.DeepEqual(current.GetLabels(), previous.GetLabels()) {
		return false
	}
	for _, key := range []string{"tailscale.com/tailnet-fqdn", "tailscale.com/tags", "borq.io/pocket-id-issuer", "borq.run/source", "borq.run/hostname", "meta.helm.sh/release-name", "meta.helm.sh/release-namespace"} {
		if current.GetAnnotations()[key] != previous.GetAnnotations()[key] {
			return false
		}
	}
	return true
}

func (state destroyState) remove(ctx context.Context, item destroyObject) error {
	if err := state.verifyDependencies(ctx); err != nil {
		return err
	}
	resource := state.client.Resource(item.resource).Namespace(item.object.GetNamespace())
	current, err := resource.Get(ctx, item.object.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.GetUID() != item.object.GetUID() || !destroyOwned(*current, item.resource.Resource) || !sameDestroyOwner(*current, item.object) || destroyOwnershipHash(*current) != destroyOwnershipHash(item.object) {
		return errors.New("resource ownership changed during destroy")
	}
	if item.resource.Resource == "clusterrolebindings" && (!reflect.DeepEqual(current.Object["roleRef"], item.object.Object["roleRef"]) || !reflect.DeepEqual(current.Object["subjects"], item.object.Object["subjects"])) {
		return errors.New("bootstrap binding authority changed during destroy")
	}
	if item.resource.Resource == "configmaps" && !reflect.DeepEqual(current.Object["data"], item.object.Object["data"]) {
		return errors.New("tunnel recovery receipt changed during destroy")
	}
	if item.resource.Resource == "gateways" {
		if err := state.checkGatewayReferences(ctx, *current); err != nil {
			return err
		}
	}
	uid, version := current.GetUID(), current.GetResourceVersion()
	propagation := metav1.DeletePropagationOrphan
	if err := resource.Delete(ctx, current.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}, PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove %s/%s: %w", item.resource.Resource, current.GetName(), err)
	}
	err = wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current, err := resource.Get(ctx, item.object.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if current.GetUID() != uid {
			return false, errors.New("resource replaced during deletion")
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if item.resource.Resource == "services" && adminEgressService(item.object) {
		return state.waitAdminEgressCleanup(ctx)
	}
	return nil
}

// Parent references are independent ownership claims: removing a known Gateway
// must not also detach a route outside this installation's retirement plan.
func (state destroyState) checkGatewayReferences(ctx context.Context, gateway unstructured.Unstructured) error {
	resources := []schema.GroupVersionResource{
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "tlsroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1alpha2", Resource: "tlsroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1alpha2", Resource: "tcproutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1alpha2", Resource: "udproutes"},
	}
	for _, resource := range resources {
		routes, err := state.client.Resource(resource).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check shared Gateway route references: %w", err)
		}
		for _, route := range routes.Items {
			parents, _, err := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
			if err != nil {
				return errors.New("invalid route parent references prevent safe Gateway removal")
			}
			for _, parent := range parents {
				fields, ok := parent.(map[string]any)
				if !ok {
					return errors.New("invalid route parent reference prevents safe Gateway removal")
				}
				name, _, _ := unstructured.NestedString(fields, "name")
				namespace, _, _ := unstructured.NestedString(fields, "namespace")
				if namespace == "" {
					namespace = route.GetNamespace()
				}
				kind, _, _ := unstructured.NestedString(fields, "kind")
				group, _, _ := unstructured.NestedString(fields, "group")
				if name != gateway.GetName() || namespace != gateway.GetNamespace() || (kind != "" && kind != "Gateway") || (group != "" && group != "gateway.networking.k8s.io") {
					continue
				}
				planned := slices.ContainsFunc(state.objects, func(item destroyObject) bool {
					return item.resource == resource && item.object.GetUID() == route.GetUID() && item.object.GetNamespace() == route.GetNamespace() && item.object.GetName() == route.GetName() && destroyOwned(route, resource.Resource)
				})
				if !planned {
					return fmt.Errorf("Gateway %s/%s is referenced by a route outside this destroy plan: %s/%s", gateway.GetNamespace(), gateway.GetName(), route.GetNamespace(), route.GetName())
				}
			}
		}
	}
	return nil
}
