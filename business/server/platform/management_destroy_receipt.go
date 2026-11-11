package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ErrManagementDestroyReceiptStale is shared across server and CLI boundaries so
// only a verified new Borq Deployment may supersede an old completion receipt.
var ErrManagementDestroyReceiptStale = errors.New("management destroy receipt belongs to a previous Borq server installation")

func destroyServerResource(resource, name string) bool {
	switch resource {
	case "deployments", "serviceaccounts", "clusterrolebindings":
		return name == "borq"
	case "services":
		return name == "borq" || name == "borq-telemetry"
	default:
		return false
	}
}

func destroyIdentity(item destroyObject) ManagementDestroyIdentity {
	return ManagementDestroyIdentity{Group: item.resource.Group, Version: item.resource.Version, Resource: item.resource.Resource,
		Kind: item.object.GetKind(), Namespace: item.object.GetNamespace(), Name: item.object.GetName(), UID: string(item.object.GetUID()),
		ResourceVersion: item.object.GetResourceVersion(), OwnershipHash: destroyOwnershipHash(item.object)}
}

func destroyOwnershipHash(object unstructured.Unstructured) string {
	annotations := map[string]string{}
	for _, key := range []string{"tailscale.com/tailnet-fqdn", "tailscale.com/tags", "borq.io/pocket-id-issuer", "borq.run/source", "borq.run/hostname", "meta.helm.sh/release-name", "meta.helm.sh/release-namespace"} {
		if value, exists := object.GetAnnotations()[key]; exists {
			annotations[key] = value
		}
	}
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	// Our own pause/resume changes replicas, while template and authority changes
	// must continue to invalidate an accepted plan throughout partial retries.
	if object.GetKind() == "Deployment" {
		delete(spec, "replicas")
	}
	// Tailscale replaces the egress placeholder with its headless Service name.
	// The requested tailnet destination stays fenced by its annotations.
	if object.GetKind() == "Service" && adminEgressService(object) && spec["type"] == "ExternalName" {
		delete(spec, "externalName")
	}
	value := []any{object.GetLabels(), annotations, spec, object.Object["roleRef"], object.Object["subjects"], object.Object["data"], object.Object["secrets"], object.Object["imagePullSecrets"], object.Object["automountServiceAccountToken"]}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func destroyIdentityKey(identity ManagementDestroyIdentity) string {
	return identity.Group + "/" + identity.Version + "/" + identity.Resource + "/" + identity.Namespace + "/" + identity.Name
}

func sameDestroyIdentities(current, expected []ManagementDestroyIdentity, allowMissing bool) bool {
	if !allowMissing && len(current) != len(expected) {
		return false
	}
	previous := make(map[string]ManagementDestroyIdentity, len(expected))
	for _, identity := range expected {
		key := destroyIdentityKey(identity)
		if _, duplicate := previous[key]; duplicate || identity.UID == "" || identity.OwnershipHash == "" {
			return false
		}
		previous[key] = identity
	}
	seen := make(map[string]bool, len(current))
	for _, identity := range current {
		key := destroyIdentityKey(identity)
		planned, exists := previous[key]
		if !exists || seen[key] || planned.UID != identity.UID || planned.Kind != identity.Kind || planned.OwnershipHash != identity.OwnershipHash {
			return false
		}
		seen[key] = true
	}
	return true
}

func sameDestroyPlan(left, right ManagementDestroyPlan) bool {
	return left.Namespace == right.Namespace && left.NamespaceUID == right.NamespaceUID && left.ClusterUID == right.ClusterUID &&
		sameDestroyIdentities(left.Server, right.Server, false) && sameDestroyIdentities(left.Resources, right.Resources, false) && sameDestroyIdentities(left.Dependencies, right.Dependencies, false) &&
		reflect.DeepEqual(left.Actions, right.Actions) && reflect.DeepEqual(left.Retained, right.Retained)
}

func validateDestroyPlan(state destroyState, current, expected ManagementDestroyPlan) error {
	if expected.Namespace == "" || expected.NamespaceUID == "" || expected.ClusterUID == "" || current.Namespace != expected.Namespace ||
		state.namespaceUID != expected.NamespaceUID || state.clusterUID != expected.ClusterUID {
		return errors.New("management cluster or namespace identity differs from the accepted destroy plan")
	}
	if !sameDestroyIdentities(current.Dependencies, expected.Dependencies, false) {
		return errors.New("retained provider configuration changed after destroy was planned")
	}
	if !sameDestroyIdentities(current.Server, expected.Server, false) {
		return errors.New("Borq server identity or authority changed after destroy was planned")
	}
	if !sameDestroyIdentities(current.Resources, expected.Resources, true) {
		return errors.New("managed resources changed after destroy was planned; inspect a new plan")
	}
	return nil
}

func (state destroyState) verifyDependencies(ctx context.Context) error {
	if len(state.dependencies) > 1 {
		return errors.New("destroy receipt contains duplicate retained dependencies")
	}
	for _, identity := range state.dependencies {
		// A receipt may freeze this retained public scope, never arbitrary files,
		// credentials, provider resources, or another namespace's configuration.
		if identity.Group != "" || identity.Version != "v1" || identity.Resource != "configmaps" || identity.Name != "borq-network-settings" || identity.Namespace != state.namespace {
			return errors.New("destroy receipt contains an unsupported retained dependency")
		}
	}
	resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	current, err := state.client.Resource(resource).Namespace(state.namespace).Get(ctx, "borq-network-settings", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if len(state.dependencies) == 0 {
			return nil
		}
		// Only a missing completion receipt may send the CLI back to the API.
		return errors.New("retained provider configuration disappeared after destroy was planned")
	}
	if err != nil {
		return fmt.Errorf("read retained destroy dependency: %w", err)
	}
	actual := destroyIdentity(destroyObject{resource, *current})
	if !sameDestroyIdentities([]ManagementDestroyIdentity{actual}, state.dependencies, false) {
		return errors.New("retained provider configuration changed after destroy was planned")
	}
	return nil
}

func managementDestroyClient(input ManagementDestroyInput) (dynamic.Interface, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, err
	}
	configuration := connection.RestConfig()
	return dynamic.NewForConfig(&configuration)
}

func verifyDestroyLocation(ctx context.Context, client dynamic.Interface, input ManagementDestroyInput, plan ManagementDestroyPlan) error {
	if plan.Namespace != input.Namespace || plan.NamespaceUID == "" || plan.ClusterUID == "" {
		return errors.New("destroy receipt identifies another management namespace")
	}
	namespaces := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	selected, err := namespaces.Get(ctx, input.Namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	system, err := namespaces.Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(selected.GetUID()) != plan.NamespaceUID || string(system.GetUID()) != plan.ClusterUID {
		return errors.New("destroy receipt identifies another physical cluster or namespace incarnation")
	}
	return nil
}

func readDestroyReceipt(ctx context.Context, client dynamic.Interface, input ManagementDestroyInput) (ManagementDestroyReceipt, *unstructured.Unstructured, error) {
	object, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(input.Namespace).Get(ctx, "borq-management-destroy", metav1.GetOptions{})
	if err != nil {
		return ManagementDestroyReceipt{}, nil, err
	}
	immutable, _, _ := unstructured.NestedBool(object.Object, "immutable")
	data, _, _ := unstructured.NestedStringMap(object.Object, "data")
	if object.GetLabels()["app.kubernetes.io/managed-by"] != "borq" || object.GetLabels()["app.kubernetes.io/component"] != "management-destroy" ||
		!immutable || len(object.GetOwnerReferences()) != 0 || object.GetDeletionTimestamp() != nil || len(data) != 1 || len(data["receipt.json"]) > 1<<20 {
		return ManagementDestroyReceipt{}, nil, errors.New("management destroy completion receipt is not an intact owned immutable record")
	}
	var receipt ManagementDestroyReceipt
	if json.Unmarshal([]byte(data["receipt.json"]), &receipt) != nil || receipt.OperationID == "" || !receipt.Plan.Executed ||
		object.GetAnnotations()["borq.run/destroy-operation"] != receipt.OperationID || object.GetAnnotations()["borq.run/cluster-uid"] != receipt.Plan.ClusterUID ||
		object.GetAnnotations()["borq.run/namespace-uid"] != receipt.Plan.NamespaceUID {
		return receipt, nil, errors.New("management destroy completion receipt is invalid")
	}
	if err := verifyDestroyLocation(ctx, client, input, receipt.Plan); err != nil {
		return receipt, nil, err
	}
	return receipt, object, nil
}

func currentDestroyServer(ctx context.Context, client dynamic.Interface, namespace string) ([]destroyObject, error) {
	var result []destroyObject
	for _, identity := range []struct {
		resource schema.GroupVersionResource
		name     string
	}{
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "borq"},
		{schema.GroupVersionResource{Version: "v1", Resource: "services"}, "borq"},
		{schema.GroupVersionResource{Version: "v1", Resource: "services"}, "borq-telemetry"},
		{schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, "borq"},
		{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "borq"},
	} {
		selected := namespace
		if identity.resource.Resource == "clusterrolebindings" {
			selected = ""
		}
		object, err := client.Resource(identity.resource).Namespace(selected).Get(ctx, identity.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !destroyOwned(*object, identity.resource.Resource) {
			return nil, errors.New("a Borq core object is foreign; refusing server finalization")
		}
		if identity.resource.Resource == "clusterrolebindings" {
			subjects, _, _ := unstructured.NestedSlice(object.Object, "subjects")
			role, _, _ := unstructured.NestedMap(object.Object, "roleRef")
			if len(subjects) != 1 || !equalDestroySubject(subjects[0], map[string]any{"kind": "ServiceAccount", "name": "borq", "namespace": namespace}) || role["kind"] != "ClusterRole" || role["name"] != "borq" || role["apiGroup"] != "rbac.authorization.k8s.io" {
				return nil, errors.New("bootstrap ClusterRoleBinding authority changed; refusing server finalization")
			}
		}
		result = append(result, destroyObject{identity.resource, *object})
	}
	return result, nil
}

func verifyDestroyExposureAbsent(ctx context.Context, client dynamic.Interface, namespace string) error {
	for _, resource := range []schema.GroupVersionResource{
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
		{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"},
		{Version: "v1", Resource: "services"}, {Group: "apps", Version: "v1", Resource: "deployments"}, {Version: "v1", Resource: "serviceaccounts"},
	} {
		objects, err := client.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, object := range objects.Items {
			if destroyOwned(object, resource.Resource) && !destroyServerResource(resource.Resource, object.GetName()) {
				return errors.New("managed exposure exists after the recorded server cleanup; refusing local finalization")
			}
		}
	}
	if err := verifyAdminEgressAbsent(ctx, client); err != nil {
		return err
	}
	_, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(namespace).Get(ctx, "borq-cloudflared", metav1.GetOptions{})
	if err == nil {
		return errors.New("a tunnel recovery receipt remains after server cleanup; refusing local finalization")
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ManagementServerRecreated distinguishes a fresh owned Deployment from a pod
// restart without consulting DNS, tunnel providers, or mutable operation state.
func ManagementServerRecreated(ctx context.Context, input ManagementDestroyInput, prior ManagementDestroyPlan) (bool, error) {
	input, err := normalizeManagementDestroy(input)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	client, err := managementDestroyClient(input)
	if err != nil {
		return false, err
	}
	if err := verifyDestroyLocation(ctx, client, input, prior); err != nil {
		return false, err
	}
	return managementServerRecreated(ctx, client, input.Namespace, prior)
}

func managementServerRecreated(ctx context.Context, client dynamic.Interface, namespace string, prior ManagementDestroyPlan) (bool, error) {
	var previous string
	for _, identity := range prior.Server {
		if identity.Resource == "deployments" && identity.Name == "borq" && identity.Namespace == namespace {
			previous = identity.UID
		}
	}
	if previous == "" {
		return false, nil
	}
	current, err := client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(namespace).Get(ctx, "borq", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !destroyOwned(*current, "deployments") || current.GetUID() == "" {
		return false, errors.New("current Borq Deployment is not owned by this installation")
	}
	return string(current.GetUID()) != previous, nil
}

// ReadManagementDestroyReceipt verifies completion against live Kubernetes
// identities. Missing receipts retain the Kubernetes NotFound error contract.
func ReadManagementDestroyReceipt(ctx context.Context, input ManagementDestroyInput) (ManagementDestroyReceipt, error) {
	input, err := normalizeManagementDestroy(input)
	if err != nil {
		return ManagementDestroyReceipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	client, err := managementDestroyClient(input)
	if err != nil {
		return ManagementDestroyReceipt{}, err
	}
	receipt, _, err := readDestroyReceipt(ctx, client, input)
	if err != nil {
		return receipt, err
	}
	recreated, err := managementServerRecreated(ctx, client, input.Namespace, receipt.Plan)
	if err != nil {
		return receipt, err
	}
	if recreated {
		return receipt, ErrManagementDestroyReceiptStale
	}
	if input.OperationID != "" && input.OperationID != receipt.OperationID {
		return receipt, errors.New("completion receipt belongs to a different destroy operation")
	}
	current, err := currentDestroyServer(ctx, client, input.Namespace)
	if err != nil {
		return receipt, err
	}
	identities := make([]ManagementDestroyIdentity, 0, len(current))
	for _, item := range current {
		identities = append(identities, destroyIdentity(item))
	}
	if !sameDestroyIdentities(identities, receipt.Plan.Server, true) {
		return receipt, errors.New("Borq core resources changed since server cleanup completed")
	}
	if err := (destroyState{client: client, namespace: input.Namespace, dependencies: receipt.Plan.Dependencies}).verifyDependencies(ctx); err != nil {
		return receipt, err
	}
	if err := verifyDestroyExposureAbsent(ctx, client, input.Namespace); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// ManagementDestroyCompleted reconciles the receipt-before-database crash window
// for one exact accepted operation. A verified fresh Deployment may make the old
// receipt stale for finalization without invalidating proof that cleanup finished.
func ManagementDestroyCompleted(ctx context.Context, input ManagementDestroyInput, expected ManagementDestroyPlan) (bool, error) {
	if input.OperationID == "" {
		return false, errors.New("receipt reconciliation requires the accepted destroy operation ID")
	}
	receipt, err := ReadManagementDestroyReceipt(ctx, input)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil && !errors.Is(err, ErrManagementDestroyReceiptStale) {
		return false, err
	}
	// A new teardown may crash before replacing the previous incarnation's
	// immutable receipt. That old proof does not complete this accepted operation.
	if errors.Is(err, ErrManagementDestroyReceiptStale) && receipt.OperationID != input.OperationID {
		return false, nil
	}
	if receipt.OperationID != input.OperationID || !receipt.Plan.Executed || !sameDestroyPlan(receipt.Plan, expected) {
		return false, errors.New("completion receipt does not match the accepted destroy operation and full plan")
	}
	return true, nil
}

func writeManagementDestroyReceipt(ctx context.Context, state destroyState, operation string, plan ManagementDestroyPlan) error {
	if err := state.verifyDependencies(ctx); err != nil {
		return err
	}
	receipt := ManagementDestroyReceipt{OperationID: operation, Plan: plan}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	configmaps := state.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(state.namespace)
	existing, object, err := readDestroyReceipt(ctx, state.client, ManagementDestroyInput{Namespace: state.namespace})
	if err == nil {
		if existing.OperationID == operation && sameDestroyPlan(existing.Plan, plan) {
			return nil
		}
		recreated, err := managementServerRecreated(ctx, state.client, state.namespace, existing.Plan)
		if err != nil {
			return err
		}
		if !recreated {
			return errors.New("another immutable management destroy receipt already exists")
		}
		uid, version := object.GetUID(), object.GetResourceVersion()
		if err := configmaps.Delete(ctx, object.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	object = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"name": "borq-management-destroy", "namespace": state.namespace,
			"labels":      map[string]any{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/component": "management-destroy"},
			"annotations": map[string]any{"borq.run/destroy-operation": operation, "borq.run/cluster-uid": plan.ClusterUID, "borq.run/namespace-uid": plan.NamespaceUID}},
		"data": map[string]any{"receipt.json": string(encoded)}}}
	_, err = configmaps.Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		saved, _, readErr := readDestroyReceipt(ctx, state.client, ManagementDestroyInput{Namespace: state.namespace})
		if readErr == nil && saved.OperationID == operation && sameDestroyPlan(saved.Plan, plan) {
			return nil
		}
	}
	return err
}

// FinalizeManagementServer removes only exact core objects after the server has
// recorded successful cleanup. It is the CLI's sole local teardown authority.
func FinalizeManagementServer(ctx context.Context, input ManagementDestroyInput, expected ManagementDestroyReceipt) (ManagementDestroyPlan, error) {
	if input.KubeconfigPath == "" {
		return expected.Plan, errors.New("server finalization requires an explicit --kubeconfig")
	}
	input, err := normalizeManagementDestroy(input)
	if err != nil {
		return expected.Plan, err
	}
	if input.OperationID == "" {
		input.OperationID = expected.OperationID
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	receipt, err := ReadManagementDestroyReceipt(ctx, input)
	if err != nil {
		return expected.Plan, err
	}
	if receipt.OperationID != expected.OperationID || !sameDestroyPlan(receipt.Plan, expected.Plan) {
		return expected.Plan, errors.New("finalization requires the exact server completion receipt")
	}
	client, err := managementDestroyClient(input)
	if err != nil {
		return expected.Plan, err
	}
	current, err := currentDestroyServer(ctx, client, input.Namespace)
	if err != nil {
		return expected.Plan, err
	}
	// Preflight every object before deleting any. A replacement Service, binding
	// or Deployment must never be adopted halfway through a retry.
	identities := make([]ManagementDestroyIdentity, 0, len(current))
	for _, item := range current {
		identities = append(identities, destroyIdentity(item))
	}
	if !sameDestroyIdentities(identities, receipt.Plan.Server, true) {
		return expected.Plan, errors.New("Borq server identities changed before finalization")
	}
	state := destroyState{client: client, namespace: input.Namespace, dependencies: receipt.Plan.Dependencies}
	for index := range current {
		item := &current[index]
		if !destroyServerResource(item.resource.Resource, item.object.GetName()) {
			return expected.Plan, fmt.Errorf("receipt includes a non-core server resource")
		}
		if item.resource.Resource == "deployments" {
			if err := state.scale(ctx, item, 0, false); err != nil {
				return expected.Plan, err
			}
		}
		if err := state.remove(ctx, *item); err != nil {
			return expected.Plan, err
		}
	}
	return receipt.Plan, nil
}
