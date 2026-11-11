package platform

import (
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestNetworkManagementPublicRoute(t *testing.T) {
	assert := is.New(t)
	settings, err := normalizeSettings(Settings{ClusterName: "production", Network: networkTestSettings()})
	assert.NoErr(err)
	assert.Equal(ManagementPublicHostname(settings), "production.example.com")
	assert.Equal(ManagementPublicHostname(Settings{ClusterName: "production"}), "")
	_, err = NormalizeNetworkSettings(networkTestSettings(), "invalid.cluster")
	assert.True(err != nil && strings.Contains(err.Error(), "cluster name"))
	objects := managementPublicResources(settings)
	certificate := objects[0].(*certmanagerv1.Certificate)
	assert.Equal(certificate.Spec.DNSNames, []string{"production.example.com"})
	assert.Equal(certificate.Spec.IssuerRef.Name, "letsencrypt-cloudflare")
	gateway := objects[1].(*gatewayv1.Gateway)
	assert.Equal(gateway.Spec.GatewayClassName, gatewayv1.ObjectName("borq-traefik"))
	assert.Equal(string(*gateway.Spec.Listeners[0].Hostname), "production.example.com")
	assert.Equal(string(gateway.Spec.Listeners[0].TLS.CertificateRefs[0].Name), certificate.Spec.SecretName)
	route := objects[2].(*gatewayv1.HTTPRoute)
	assert.Equal(route.Spec.Hostnames, []gatewayv1.Hostname{"production.example.com"})
	assert.Equal(string(route.Spec.Rules[0].BackendRefs[0].Name), "borq")
	assert.Equal(*route.Spec.Rules[0].BackendRefs[0].Port, gatewayv1.PortNumber(80))
}

// Ownership guards must fail before creating any of the remaining resources.
// These fake clients do not model reconciliation or certificate issuance.
func TestNetworkManagementPublicOwnership(t *testing.T) {
	for _, kind := range []string{"certificate", "gateway", "route", "secret", "hostname"} {
		t.Run(kind, func(t *testing.T) {
			assert := is.New(t)
			settings, err := normalizeSettings(Settings{ClusterName: "production", Network: networkTestSettings()})
			assert.NoErr(err)
			objects := managementPublicResources(settings)
			var foreign ctrlclient.Object
			switch kind {
			case "certificate":
				foreign = objects[0]
			case "gateway":
				foreign = objects[1]
			case "route":
				foreign = objects[2]
			case "secret":
				foreign = &corev1.Secret{ObjectMeta: networkMetadata(settings.Namespace, managementPublicRouteName+"-tls")}
			case "hostname":
				gateway := objects[1].(*gatewayv1.Gateway)
				gateway.Name = "another-gateway"
				route := objects[2].(*gatewayv1.HTTPRoute)
				route.Name = "another-route"
				route.Spec.ParentRefs[0].Name = gatewayv1.ObjectName(gateway.Name)
				client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(gateway, route).Build()
				err := reconcileManagementPublicRoute(t.Context(), client, settings)
				assert.True(err != nil && strings.Contains(err.Error(), "already routed"))
				assert.True(apierrors.IsNotFound(client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(objects[0]), &certmanagerv1.Certificate{})))
				return
			}
			foreign.SetLabels(nil)
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(foreign).Build()
			err = reconcileManagementPublicRoute(t.Context(), client, settings)
			assert.True(err != nil && strings.Contains(err.Error(), "not owned"))
			for _, object := range objects {
				if object == foreign {
					continue
				}
				assert.True(apierrors.IsNotFound(client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(object), object)))
			}
		})
	}
}

func TestNetworkManagementTunnelRoute(t *testing.T) {
	assert := is.New(t)
	settings, err := normalizeSettings(Settings{ClusterName: "production", Network: networkTestSettings()})
	assert.NoErr(err)
	settings.Network.Cloudflare.Exposure = "tunnel"
	objects := managementPublicResources(settings)
	assert.Equal(len(objects), 2)
	gateway := objects[0].(*gatewayv1.Gateway)
	assert.Equal(gateway.Spec.Listeners[0].Protocol, gatewayv1.HTTPProtocolType)
	assert.Equal(gateway.Spec.Listeners[0].Port, gatewayv1.PortNumber(8000))
	assert.True(gateway.Spec.Listeners[0].TLS == nil)
	assert.Equal(objects[1].(*gatewayv1.HTTPRoute).Spec.Hostnames, []gatewayv1.Hostname{"production.example.com"})
}
