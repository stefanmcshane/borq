package apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Only an unchanged public route switching from managed TLS to the private
// tunnel listener can retire its old Certificate through a regular app plan.
func applicationRetiredCertificates(previous, next []map[string]any) ([]map[string]any, []map[string]any) {
	retained, retired := []map[string]any{}, []map[string]any{}
	for _, manifest := range previous {
		if manifest["kind"] != "Certificate" || manifest["apiVersion"] != "cert-manager.io/v1" {
			retained = append(retained, manifest)
			continue
		}
		var certificate certmanagerv1.Certificate
		if decodeManifest(manifest, &certificate) != nil || !strings.HasSuffix(certificate.Name, "-tls") {
			retained = append(retained, manifest)
			continue
		}
		routeName := strings.TrimSuffix(certificate.Name, "-tls")
		oldRoute, oldGateway, oldResources := applicationRouteResources(previous, routeName)
		newRoute, newGateway, newResources := applicationRouteResources(next, routeName)
		application := App{Name: certificate.Labels["app.kubernetes.io/name"]}
		if len(oldRoute.Spec.Rules) == 1 && len(oldRoute.Spec.Rules[0].BackendRefs) == 1 && oldRoute.Spec.Rules[0].BackendRefs[0].Port != nil {
			application.Port = int(*oldRoute.Spec.Rules[0].BackendRefs[0].Port)
		}
		oldIngress, oldErr := inspectHttpRoute(oldResources.route, application, oldResources.all)
		if len(newRoute.Spec.Rules) == 1 && len(newRoute.Spec.Rules[0].BackendRefs) == 1 && newRoute.Spec.Rules[0].BackendRefs[0].Port != nil {
			application.Port = int(*newRoute.Spec.Rules[0].BackendRefs[0].Port)
		}
		newIngress, newErr := inspectHttpRoute(newResources.route, application, newResources.all)
		if oldErr != nil || newErr != nil || oldIngress != newIngress || len(oldGateway.Spec.Listeners) != 1 || oldGateway.Spec.Listeners[0].TLS == nil || !applicationTunnelRoute(newRoute, newGateway) {
			retained = append(retained, manifest)
			continue
		}
		retired = append(retired, manifest)
	}
	return retained, retired
}

type applicationRouteInventory struct {
	route inspectedResource
	all   []inspectedResource
}

func applicationRouteResources(manifests []map[string]any, name string) (gatewayv1.HTTPRoute, gatewayv1.Gateway, applicationRouteInventory) {
	inventory := applicationRouteInventory{}
	var route gatewayv1.HTTPRoute
	var gateway gatewayv1.Gateway
	for _, manifest := range manifests {
		object := unstructured.Unstructured{Object: manifest}
		if object.GetName() != name && object.GetName() != ingressSecretName(name) {
			continue
		}
		resource := inspectedResource{kind: object.GetKind(), name: object.GetName(), app: object.GetLabels()["app.kubernetes.io/name"], manifest: manifest}
		inventory.all = append(inventory.all, resource)
		if object.GetKind() == "HTTPRoute" && object.GetName() == name {
			inventory.route = resource
			_ = decodeManifest(manifest, &route)
		}
		if object.GetKind() == "Gateway" && object.GetName() == name {
			_ = decodeManifest(manifest, &gateway)
		}
	}
	return route, gateway, inventory
}

func (manager Manager) retireApplicationCertificates(ctx context.Context, saved planRecord) error {
	if len(saved.RetiredCertificates) == 0 {
		return nil
	}
	client, err := networkDnsClient(saved.Connection)
	if err != nil {
		return err
	}
	for _, manifest := range saved.RetiredCertificates {
		var planned certmanagerv1.Certificate
		if err := decodeManifest(manifest, &planned); err != nil {
			return err
		}
		name := strings.TrimSuffix(planned.Name, "-tls")
		route, gateway, _ := applicationRouteResources(saved.Plan.Manifests, name)
		if !applicationTunnelRoute(route, gateway) {
			return errors.New("certificate retirement requires a saved tunnel route")
		}
		var liveRoute gatewayv1.HTTPRoute
		var liveGateway gatewayv1.Gateway
		key := ctrlclient.ObjectKey{Namespace: planned.Namespace, Name: name}
		if err := client.Get(ctx, key, &liveRoute); err != nil {
			return err
		}
		if err := client.Get(ctx, key, &liveGateway); err != nil {
			return err
		}
		if !applicationRetirementOwner(&liveRoute, &planned, saved.Plan.Space.ID) || !applicationRetirementOwner(&liveGateway, &planned, saved.Plan.Space.ID) ||
			!equality.Semantic.DeepDerivative(route.Spec, liveRoute.Spec) || !equality.Semantic.DeepDerivative(gateway.Spec, liveGateway.Spec) || !applicationTunnelRoute(liveRoute, liveGateway) {
			return errors.New("refusing certificate retirement before the owned tunnel route is applied")
		}
		accepted := slices.ContainsFunc(liveRoute.Status.Parents, func(parent gatewayv1.RouteParentStatus) bool {
			return parent.ControllerName == "traefik.io/gateway-controller" && string(parent.ParentRef.Name) == liveRoute.Name &&
				parent.ParentRef.SectionName != nil && *parent.ParentRef.SectionName == "web" && (parent.ParentRef.Namespace == nil || string(*parent.ParentRef.Namespace) == liveRoute.Namespace) &&
				networkDnsCondition(parent.Conditions, "Accepted", liveRoute.Generation) && networkDnsCondition(parent.Conditions, "ResolvedRefs", liveRoute.Generation)
		})
		if !accepted || !networkDnsCondition(liveGateway.Status.Conditions, "Accepted", liveGateway.Generation) || !networkDnsCondition(liveGateway.Status.Conditions, "Programmed", liveGateway.Generation) {
			return errors.New("refusing certificate retirement before the tunnel route is accepted")
		}
		var current certmanagerv1.Certificate
		err = client.Get(ctx, ctrlclient.ObjectKeyFromObject(&planned), &current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !applicationRetirementOwner(&current, &planned, saved.Plan.Space.ID) || planned.UID == "" || current.UID != planned.UID || current.ResourceVersion == "" || current.Spec.SecretName != planned.Spec.SecretName ||
			!reflect.DeepEqual(current.Spec.DNSNames, planned.Spec.DNSNames) || (current.Spec.IssuerRef.Name != planned.Spec.IssuerRef.Name || current.Spec.IssuerRef.Kind != planned.Spec.IssuerRef.Kind || cmp.Or(current.Spec.IssuerRef.Group, "cert-manager.io") != cmp.Or(planned.Spec.IssuerRef.Group, "cert-manager.io")) {
			return errors.New("refusing to retire a Certificate no longer owned by the saved application")
		}
		var gateways gatewayv1.GatewayList
		if err := client.List(ctx, &gateways); err != nil {
			return err
		}
		for _, other := range gateways.Items {
			for _, listener := range other.Spec.Listeners {
				if listener.TLS == nil {
					continue
				}
				for _, reference := range listener.TLS.CertificateRefs {
					namespace := other.Namespace
					if reference.Namespace != nil {
						namespace = string(*reference.Namespace)
					}
					if namespace == planned.Namespace && string(reference.Name) == planned.Spec.SecretName && (reference.Kind == nil || *reference.Kind == "Secret") && (reference.Group == nil || *reference.Group == "") {
						return fmt.Errorf("Certificate is still referenced by Gateway %s/%s", other.Namespace, other.Name)
					}
				}
			}
		}
		// Keep the issued Secret available for a later direct-mode transition,
		// matching Pocket ID's retirement behavior; stop future issuance only.
		err = client.Delete(ctx, &current, ctrlclient.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion}, ctrlclient.PropagationPolicy(metav1.DeletePropagationOrphan))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("retire obsolete application Certificate: %w", err)
		}
	}
	return nil
}

func applicationRetirementOwner(current, planned metav1.Object, target string) bool {
	if current.GetNamespace() != planned.GetNamespace() || current.GetDeletionTimestamp() != nil || current.GetLabels()["borq.run/target-id"] != target || current.GetLabels()["app.kubernetes.io/managed-by"] != "borq" || current.GetLabels()["app.kubernetes.io/name"] != planned.GetLabels()["app.kubernetes.io/name"] {
		return false
	}
	for key, value := range planned.GetLabels() {
		if current.GetLabels()[key] != value {
			return false
		}
	}
	return true
}

func (manager Manager) captureApplicationCertificateRetirements(ctx context.Context, stored spaceRecord, connection Cluster, candidates, next []map[string]any) ([]map[string]any, error) {
	latest, err := manager.latestAcceptedOperation(ctx, stored)
	if err != nil {
		return nil, err
	}
	if latest != "" {
		operation, err := manager.GetOperation(ctx, latest)
		if err != nil {
			return nil, err
		}
		previous, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return nil, err
		}
		for _, manifest := range previous.RetiredCertificates {
			var certificate certmanagerv1.Certificate
			if decodeManifest(manifest, &certificate) != nil {
				return nil, errors.New("read pending certificate retirement")
			}
			name := strings.TrimSuffix(certificate.Name, "-tls")
			route, gateway, resources := applicationRouteResources(next, name)
			if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 || route.Spec.Rules[0].BackendRefs[0].Port == nil {
				continue
			}
			application := App{Name: certificate.Labels["app.kubernetes.io/name"], Port: int(*route.Spec.Rules[0].BackendRefs[0].Port)}
			ingress, err := inspectHttpRoute(resources.route, application, resources.all)
			if err == nil && applicationTunnelRoute(route, gateway) && reflect.DeepEqual(certificate.Spec.DNSNames, []string{ingress.Host}) {
				candidates = append(candidates, manifest)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	client, err := networkDnsClient(connection)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	seen := map[string]bool{}
	for _, manifest := range candidates {
		var planned certmanagerv1.Certificate
		if err := decodeManifest(manifest, &planned); err != nil {
			return nil, err
		}
		if seen[planned.Namespace+"/"+planned.Name] {
			continue
		}
		seen[planned.Namespace+"/"+planned.Name] = true
		var current certmanagerv1.Certificate
		err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(&planned), &current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !applicationRetirementOwner(&current, &planned, stored.Space.ID) || current.UID == "" || (planned.UID != "" && current.UID != planned.UID) ||
			current.Spec.SecretName != planned.Spec.SecretName || !reflect.DeepEqual(current.Spec.DNSNames, planned.Spec.DNSNames) || (current.Spec.IssuerRef.Name != planned.Spec.IssuerRef.Name || current.Spec.IssuerRef.Kind != planned.Spec.IssuerRef.Kind || cmp.Or(current.Spec.IssuerRef.Group, "cert-manager.io") != cmp.Or(planned.Spec.IssuerRef.Group, "cert-manager.io")) {
			return nil, errors.New("refusing to plan retirement of a Certificate outside the saved application")
		}
		object := (&unstructured.Unstructured{Object: manifest}).DeepCopy()
		object.SetUID(current.UID)
		result = append(result, object.Object)
	}
	return result, nil
}
