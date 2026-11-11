package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Gateway API definitions are pinned with Borq; existing shared definitions are
// never adopted or overwritten during installation.
func ensureGatewayApi(ctx context.Context, connection kubernetes.Connection) error {
	source, err := componentValues.ReadFile("values/gateway-api.yaml")
	if err != nil {
		return err
	}
	definitions, err := kubernetes.ParseCRDs(source)
	if err != nil {
		return err
	}
	if err := kubernetes.NewCRDManager(connection.Objects(), logger.Logger{}).EnsureInstalled(ctx, definitions); err != nil {
		return err
	}
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
			var current apiextensionsv1.CustomResourceDefinition
			if err := client.Get(ctx, ctrlclient.ObjectKey{Name: definition.Name}, &current); err != nil {
				return false, err
			}
			for _, condition := range current.Status.Conditions {
				if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("wait for Gateway API %s: %w", definition.Name, err)
		}
	}
	class := gatewayv1.GatewayClass{ObjectMeta: networkMetadata("", borqTraefikName), Spec: gatewayv1.GatewayClassSpec{ControllerName: "traefik.io/gateway-controller"}}
	return ensureNetworkResource(ctx, client, &class)
}

func pocketIdGateway(input PocketIdInput, hostname string, secure bool) *gatewayv1.Gateway {
	listener := gatewayv1.Listener{Name: "web", Protocol: gatewayv1.HTTPProtocolType, Port: 8000}
	if hostname != "" {
		listener.Hostname = (*gatewayv1.Hostname)(&hostname)
	} // Gateway API models optional hostnames as pointers.
	if secure {
		listener.Name, listener.Protocol, listener.Port = "websecure", gatewayv1.HTTPSProtocolType, 8443
		listener.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: gatewayv1.ObjectName("pocketid-" + input.InstanceID + "-tls")}}} // TLS is optional in Gateway API.
	}
	return &gatewayv1.Gateway{ObjectMeta: pocketIdMetadata(input), Spec: gatewayv1.GatewaySpec{GatewayClassName: borqTraefikName, Listeners: []gatewayv1.Listener{listener}}}
}

func pocketIdHttpRoute(input PocketIdInput, hostname string) *gatewayv1.HTTPRoute {
	pathType, path, port := gatewayv1.PathMatchPathPrefix, "/", gatewayv1.PortNumber(1411)
	metadata := pocketIdMetadata(input)
	route := &gatewayv1.HTTPRoute{ObjectMeta: metadata, Spec: gatewayv1.HTTPRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(metadata.Name)}}},
		Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
		Rules:           []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &path}}}, BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(metadata.Name), Port: &port}}}}}}, // Gateway API uses pointers for optional path and port fields.
	}}
	if input.Tunnel {
		// Cloudflare's public origin stays HTTPS across the local HTTP hop.
		route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Forwarded-Proto", Value: "https"}}}}}
	}
	return route
}
