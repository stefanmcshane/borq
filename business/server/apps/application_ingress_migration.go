package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Only a representation change may retire an Ingress. Removing or renaming
// its public route still requires the explicit application deletion flow.
func applicationIngressReplacement(ingress networkingv1.Ingress, next []map[string]any) bool {
	if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != "traefik" || ingress.Spec.DefaultBackend != nil || len(ingress.Spec.Rules) != 1 || len(ingress.Spec.TLS) != 1 {
		return false
	}
	rule := ingress.Spec.Rules[0]
	if rule.HTTP == nil || len(rule.HTTP.Paths) != 1 || !slices.Equal(ingress.Spec.TLS[0].Hosts, []string{rule.Host}) || ingress.Spec.TLS[0].SecretName != ingressSecretName(ingress.Name) {
		return false
	}
	path := rule.HTTP.Paths[0]
	if path.PathType == nil || *path.PathType != networkingv1.PathTypePrefix || path.Backend.Service == nil || path.Backend.Resource != nil || path.Backend.Service.Name != ingress.Labels["app.kubernetes.io/name"] || path.Backend.Service.Port.Number == 0 {
		return false
	}
	route, gateway, resources := applicationRouteResources(next, ingress.Name)
	if !applicationRetirementOwner(&route, &ingress, ingress.Labels["borq.run/target-id"]) || !applicationRetirementOwner(&gateway, &ingress, ingress.Labels["borq.run/target-id"]) {
		return false
	}
	replacement, err := inspectHttpRoute(resources.route, App{Name: path.Backend.Service.Name, Port: int(path.Backend.Service.Port.Number)}, resources.all)
	return err == nil && replacement.Host == rule.Host && replacement.Path == path.Path
}

func (manager Manager) captureApplicationIngressRetirements(ctx context.Context, stored spaceRecord, connection Cluster, next []map[string]any) ([]map[string]any, []map[string]any, error) {
	retained, candidates := []map[string]any{}, []map[string]any{}
	for _, manifest := range stored.Manifests {
		var ingress networkingv1.Ingress
		if manifest["apiVersion"] == "networking.k8s.io/v1" && manifest["kind"] == "Ingress" && decodeManifest(manifest, &ingress) == nil && applicationIngressReplacement(ingress, next) {
			candidates = append(candidates, manifest)
		} else {
			retained = append(retained, manifest)
		}
	}
	// Accepted desired state already contains Gateways. Carry pending cleanup
	// from its saved plan so failed rollouts and restarts cannot forget it.
	latest, err := manager.latestAcceptedOperation(ctx, stored)
	if err != nil {
		return nil, nil, err
	}
	if latest != "" {
		operation, err := manager.GetOperation(ctx, latest)
		if err != nil {
			return nil, nil, err
		}
		previous, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return nil, nil, err
		}
		candidates = append(candidates, previous.RetiredIngresses...)
	}
	if len(candidates) == 0 {
		return retained, nil, nil
	}
	client, err := networkDnsClient(connection)
	if err != nil {
		return nil, nil, err
	}
	retired := []map[string]any{}
	seen := map[string]bool{}
	for _, manifest := range candidates {
		var planned, current networkingv1.Ingress
		if err := decodeManifest(manifest, &planned); err != nil {
			return nil, nil, err
		}
		if seen[manifestIdentity(manifest)] {
			continue
		}
		seen[manifestIdentity(manifest)] = true
		err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(&planned), &current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !applicationIngressReplacement(planned, next) {
			return nil, nil, fmt.Errorf("%w: finish the pending Ingress migration before changing its route", ErrInvalid)
		}
		if !applicationRetirementOwner(&current, &planned, stored.Space.ID) || current.UID == "" || planned.UID != "" && current.UID != planned.UID || !equality.Semantic.DeepEqual(current.Spec, planned.Spec) {
			return nil, nil, errors.New("refusing to plan retirement of an Ingress outside the saved application")
		}
		object := (&unstructured.Unstructured{Object: manifest}).DeepCopy()
		object.SetUID(current.UID)
		retired = append(retired, object.Object)
	}
	return retained, retired, nil
}

func (manager Manager) retireApplicationIngresses(ctx context.Context, saved planRecord) error {
	if len(saved.RetiredIngresses) == 0 {
		return nil
	}
	applications, err := newClusterClient(saved.Connection)
	if err != nil {
		return err
	}
	client, err := networkDnsClient(saved.Connection)
	if err != nil {
		return err
	}
	for _, manifest := range saved.RetiredIngresses {
		var planned, current networkingv1.Ingress
		if err := decodeManifest(manifest, &planned); err != nil {
			return err
		}
		if !applicationIngressReplacement(planned, saved.Plan.Manifests) {
			return errors.New("Ingress retirement requires its saved replacement route")
		}
		var gateway gatewayv1.Gateway
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(&planned), &gateway); err != nil {
			return err
		}
		if !applicationRetirementOwner(&gateway, &planned, saved.Plan.Space.ID) || !networkDnsCondition(gateway.Status.Conditions, "Programmed", gateway.Generation) {
			return errors.New("refusing Ingress retirement before its owned replacement Gateway is programmed")
		}
		_, _, resources := applicationRouteResources(saved.Plan.Manifests, planned.Name)
		replacements := []map[string]any{}
		for _, resource := range resources.all {
			replacements = append(replacements, resource.manifest)
		}
		status, err := applications.Status(ctx, replacements, saved.Plan.Space.ID)
		if err != nil {
			return err
		}
		if status.State != "succeeded" {
			return errors.New("refusing Ingress retirement before its replacement route is ready")
		}
		err = client.Get(ctx, ctrlclient.ObjectKeyFromObject(&planned), &current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !applicationRetirementOwner(&current, &planned, saved.Plan.Space.ID) || planned.UID == "" || current.UID != planned.UID || current.ResourceVersion == "" || !equality.Semantic.DeepEqual(current.Spec, planned.Spec) {
			return errors.New("refusing to retire an Ingress no longer owned by the saved application")
		}
		// Ingress-shim Certificates and issued Secrets must survive the old
		// Ingress, because the replacement Gateway uses the same TLS identity.
		err = client.Delete(ctx, &current, ctrlclient.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion}, ctrlclient.PropagationPolicy(metav1.DeletePropagationOrphan))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("retire obsolete application Ingress: %w", err)
		}
	}
	return nil
}
