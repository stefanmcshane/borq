package platform

import (
	"context"
	"fmt"
	"net/url"
	"slices"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const managementPublicRouteName = "borq-management-public"

// ManagementPublicHostname names this installation's Borq server within its
// configured public suffix. Incomplete or private-only settings have no public name.
func ManagementPublicHostname(settings Settings) string {
	if settings.BorqPublicURL != "" {
		address, err := url.Parse(settings.BorqPublicURL)
		if err == nil {
			return address.Hostname()
		}
		return ""
	}
	if settings.Network.Cloudflare == nil || settings.ClusterName == "" {
		return ""
	}
	return settings.ClusterName + "." + settings.Network.Cloudflare.WildcardDomain
}

func reconcileManagementPublicRoute(ctx context.Context, client ctrlclient.Client, settings Settings) error {
	hostname := ManagementPublicHostname(settings)
	if err := checkPublicRouteConflicts(ctx, client, settings.Namespace, managementPublicRouteName, hostname, "management"); err != nil {
		return err
	}
	objects := managementPublicResources(settings)
	// Inspect every destination before writing any resource. A foreign certificate
	// or TLS Secret must never be adopted merely because a route is being added.
	for _, desired := range objects {
		current := desired.DeepCopyObject().(ctrlclient.Object)
		err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := checkNetworkOwner(current); err != nil {
			return err
		}
	}
	if settings.Network.Cloudflare.ExposureMode() == "direct" {
		var secret corev1.Secret
		err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementPublicRouteName + "-tls"}, &secret)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && (checkNetworkOwner(&secret) != nil || secret.Type != corev1.SecretTypeTLS || secret.Annotations["cert-manager.io/certificate-name"] != managementPublicRouteName+"-tls") {
			return fmt.Errorf("management TLS Secret is not owned by its certificate")
		}
	}
	for _, desired := range objects {
		if err := ensureNetworkResource(ctx, client, desired); err != nil {
			return err
		}
	}
	if settings.Network.Cloudflare.ExposureMode() == "tunnel" {
		return retireManagementCertificate(ctx, client, settings)
	}
	return nil
}

func retireManagementCertificate(ctx context.Context, client ctrlclient.Client, settings Settings) error {
	var current certmanagerv1.Certificate
	err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementPublicRouteName + "-tls"}, &current)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if checkNetworkOwner(&current) != nil || current.Labels["borq.run/management-cluster"] != settings.ClusterName ||
		current.Spec.SecretName != current.Name || !slices.Equal(current.Spec.DNSNames, []string{ManagementPublicHostname(settings)}) {
		return fmt.Errorf("management origin Certificate is not owned by this installation")
	}
	// Leave the Secret in place; only the obsolete renewal request is retired.
	return client.Delete(ctx, &current, ctrlclient.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion})
}

// Kubernetes resources model optional TLS, hostnames, paths and ports as pointers.
func managementPublicResources(settings Settings) []ctrlclient.Object {
	hostname := gatewayv1.Hostname(ManagementPublicHostname(settings))
	metadata := networkMetadata(settings.Namespace, managementPublicRouteName)
	metadata.Labels["borq.run/management-cluster"] = settings.ClusterName
	certificateMetadata := *metadata.DeepCopy()
	certificateMetadata.Name += "-tls"
	certificate := &certmanagerv1.Certificate{ObjectMeta: certificateMetadata, Spec: certmanagerv1.CertificateSpec{
		SecretName: certificateMetadata.Name, DNSNames: []string{string(hostname)},
		IssuerRef:      cmmeta.ObjectReference{Name: CloudflareCertificateIssuer, Kind: "ClusterIssuer"},
		SecretTemplate: &certmanagerv1.CertificateSecretTemplate{Labels: certificateMetadata.Labels},
	}}
	gateway := &gatewayv1.Gateway{ObjectMeta: *metadata.DeepCopy(), Spec: gatewayv1.GatewaySpec{
		GatewayClassName: borqTraefikName,
		Listeners: []gatewayv1.Listener{{Name: "websecure", Hostname: &hostname, Port: 8443, Protocol: gatewayv1.HTTPSProtocolType,
			TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: gatewayv1.ObjectName(certificateMetadata.Name)}}}}},
	}}
	pathType, path, port := gatewayv1.PathMatchPathPrefix, "/", gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{ObjectMeta: *metadata.DeepCopy(), Spec: gatewayv1.HTTPRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(metadata.Name)}}},
		Hostnames:       []gatewayv1.Hostname{hostname},
		Rules: []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &path}}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "borq", Port: &port}}}}}},
	}}
	if settings.Network.Cloudflare.ExposureMode() == "tunnel" {
		gateway.Spec.Listeners = []gatewayv1.Listener{{Name: "web", Hostname: &hostname, Port: 8000, Protocol: gatewayv1.HTTPProtocolType}}
		// Public TLS terminates at Cloudflare; keep HTTPS semantics at the backend.
		route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Forwarded-Proto", Value: "https"}}}}}
		return []ctrlclient.Object{gateway, route}
	}
	return []ctrlclient.Object{certificate, gateway, route}
}

// A mode change must not stop the existing connector before direct ingress has
// an explicit public target or an address assigned by the Gateway controller.
func checkManagementDirectTarget(ctx context.Context, client ctrlclient.Client, settings Settings) error {
	if settings.Network.Cloudflare.DirectTarget != "" {
		return validateCloudflareDirectTarget(settings.Network.Cloudflare.DirectTarget)
	}
	var gateway gatewayv1.Gateway
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementPublicRouteName}, &gateway); err != nil {
		return err
	}
	if err := checkNetworkOwner(&gateway); err != nil {
		return err
	}
	for _, address := range gateway.Status.Addresses {
		if validateCloudflareDirectTarget(address.Value) == nil {
			return nil
		}
	}
	return fmt.Errorf("keep existing tunnel running: direct exposure requires a public Gateway address or network.cloudflare.directTarget")
}
