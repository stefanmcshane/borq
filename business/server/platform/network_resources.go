package platform

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func networkClient(connection kubernetes.Connection) (ctrlclient.Client, error) {
	configuration := connection.RestConfig()
	return ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
}

func networkLabels() map[string]string {
	labels := managedLabels()
	labels["borq.run/network"] = "true"
	return labels
}

func networkMetadata(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: networkLabels()}
}

// Kubernetes clients require object pointers for resource reconciliation.
func networkProxyClass() *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailscale.com/v1alpha1",
		"kind":       "ProxyClass",
		"spec": map[string]any{
			"statefulSet": map[string]any{
				"pod": map[string]any{
					"tailscaleContainer": map[string]any{
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "10m", "memory": "64Mi"},
							"limits":   map[string]any{"memory": "256Mi"},
						},
					},
					"tailscaleInitContainer": map[string]any{
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
							"limits":   map[string]any{"memory": "64Mi"},
						},
					},
				},
			},
		},
	}}
	object.SetName("borq")
	object.SetLabels(networkLabels())
	return object
}

func networkObject(object ctrlclient.Object) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object.DeepCopyObject())
	if err != nil {
		return nil, err
	}
	result := &unstructured.Unstructured{Object: content}
	if result.GetKind() == "" {
		kinds, _, err := kubernetes.NewScheme().ObjectKinds(object)
		if err != nil || len(kinds) == 0 {
			return nil, fmt.Errorf("network resource kind unavailable")
		}
		result.SetGroupVersionKind(kinds[0])
	}
	return result, nil
}

// Only Borq-owned resources are reconciled; the resource version fences changes
// to ownership between the read and the write.
func ensureNetworkResource(ctx context.Context, client ctrlclient.Client, object ctrlclient.Object) error {
	desired, err := networkObject(object)
	if err != nil {
		return err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(desired.GroupVersionKind())
	err = client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		if err = client.Create(ctx, desired, ctrlclient.FieldOwner("borq-network")); err == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return networkResourceError(desired, err)
		}
		if err = client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), current); err != nil {
			return networkResourceError(desired, err)
		}
	} else if err != nil {
		return networkResourceError(desired, err)
	}
	if err := checkNetworkOwner(current); err != nil {
		return err
	}
	// Credential Secrets are an atomic document owned by this installation.
	// A versioned update permits intentional rotation.
	if desired.GetKind() == "Secret" {
		if desired.GetName() == "operator-oauth" {
			desiredScope, currentScope := desired.GetLabels()["borq.run/operator-scope"], current.GetLabels()["borq.run/operator-scope"]
			if desiredScope != currentScope {
				if desiredScope == "target" && currentScope == "management" {
					return nil
				}
				return fmt.Errorf("operator installation scope changed during reconciliation; retry")
			}
		}
		if reflect.DeepEqual(current.Object["data"], desired.Object["data"]) {
			return nil
		}
		current.Object["data"] = desired.Object["data"]
		if err := client.Update(ctx, current, ctrlclient.FieldOwner("borq-network")); err != nil {
			return networkResourceError(desired, err)
		}
		return nil
	}
	// The operator replaces this placeholder with its proxy's headless Service.
	// It owns routing state; Borq owns the requested tailnet destination annotation.
	if desired.GetKind() == "Service" {
		serviceType, _, _ := unstructured.NestedString(desired.Object, "spec", "type")
		if serviceType == "ExternalName" {
			name, _, _ := unstructured.NestedString(current.Object, "spec", "externalName")
			if name != "" {
				_ = unstructured.SetNestedField(desired.Object, name, "spec", "externalName")
			}
		}
	}
	labels, annotations := current.GetLabels(), current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	maps.Copy(labels, desired.GetLabels())
	maps.Copy(annotations, desired.GetAnnotations())
	current.SetLabels(labels)
	current.SetAnnotations(annotations)
	// Replace configuration in full: apply would retain fields added by another
	// editor. Keep server metadata (including UID and resourceVersion) and status.
	desired.Object["metadata"] = current.Object["metadata"]
	if status, exists := current.Object["status"]; exists {
		desired.Object["status"] = status
	} else {
		delete(desired.Object, "status")
	}
	if err := client.Update(ctx, desired, ctrlclient.FieldOwner("borq-network")); err != nil {
		return networkResourceError(desired, err)
	}
	return nil
}

func checkNetworkOwner(object ctrlclient.Object) error {
	for key, value := range networkLabels() {
		if object.GetLabels()[key] != value {
			return fmt.Errorf("network resource %s/%s is not owned by Borq networking", object.GetNamespace(), object.GetName())
		}
	}
	if object.GetDeletionTimestamp() != nil {
		return fmt.Errorf("network resource %s/%s is being deleted", object.GetNamespace(), object.GetName())
	}
	return nil
}

func networkResourceError(object ctrlclient.Object, err error) error {
	// Kubernetes validation errors may quote Secret data; retain only a safe reason for Secrets.
	_, typedSecret := object.(*corev1.Secret)
	if !typedSecret && object.GetObjectKind().GroupVersionKind().Kind != "Secret" {
		return fmt.Errorf("reconcile network resource %s %s/%s: %w", object.GetObjectKind().GroupVersionKind().Kind, object.GetNamespace(), object.GetName(), err)
	}
	return fmt.Errorf("reconcile network Secret %s/%s: %s", object.GetNamespace(), object.GetName(), apierrors.ReasonForError(err))
}

// EnsureNetworkDnsEndpoint installs the pinned official ExternalDNS CRD used by
// the management inventory, without installing controllers or using credentials.
func EnsureNetworkDnsEndpoint(ctx context.Context, connection kubernetes.Connection) error {
	content, err := componentValues.ReadFile("values/dnsendpoints.yaml")
	if err != nil {
		return err
	}
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(content, &object.Object); err != nil {
		return fmt.Errorf("decode pinned DNS endpoint definition: %w", err)
	}
	object.SetLabels(networkLabels())
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	if err := ensureNetworkResource(ctx, client, object); err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(object), object); err != nil {
			return false, err
		}
		return networkCondition(object, "Established"), nil
	})
}

func networkCondition(object *unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, condition := range conditions {
		value, ok := condition.(map[string]any)
		if ok && value["type"] == kind && value["status"] == "True" {
			return true
		}
	}
	return false
}
