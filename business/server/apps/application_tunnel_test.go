package apps

import (
	"encoding/json"
	"testing"

	"github.com/matryer/is"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestApplicationTunnelRoundTrip owns the rendered private origin contract and
// its strict inspection; it does not claim acceptance by a live controller.
func TestApplicationTunnelRoundTrip(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"traefik/whoami:v1.11","port":80,"ingresses":[{"name":"public","host":"web.example.com"}]}]}`))
	assert.NoErr(err)
	rendered, err := renderWithEnv(configuration, "apps", "space-one", "plan-one", "pinned-issuer", nil, ResourceScope{Organization: "acme", Space: "production", Cluster: "local", tunnelDomain: "example.com"}, nil, ManagementS3{})
	assert.NoErr(err)
	for _, manifest := range rendered.Manifests {
		assert.True(manifest["kind"] != "Certificate")
	}
	route := decodeResource[gatewayv1.HTTPRoute](t, findManifest(t, rendered, "HTTPRoute", "web-public"))
	gateway := decodeResource[gatewayv1.Gateway](t, findManifest(t, rendered, "Gateway", "web-public"))
	assert.True(applicationTunnelRoute(route, gateway))
	assert.Equal(gateway.Spec.Listeners[0].Port, gatewayv1.PortNumber(8000))
	assert.Equal(gateway.Spec.Listeners[0].Protocol, gatewayv1.HTTPProtocolType)
	assert.Equal(route.Spec.Rules[0].Filters[0].RequestHeaderModifier.Set, []gatewayv1.HTTPHeader{{Name: "X-Forwarded-Proto", Value: "https"}})
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	for _, test := range []struct {
		name   string
		change func(gatewayv1.HTTPRoute, gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway)
	}{
		{"missing forwarding header", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			route.Spec.Rules[0].Filters = nil
			return route, gateway
		}},
		{"different forwarding header", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			route.Spec.Rules[0].Filters[0].RequestHeaderModifier.Set[0].Value = "http"
			return route, gateway
		}},
		{"additional forwarding header", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			route.Spec.Rules[0].Filters[0].RequestHeaderModifier.Add = []gatewayv1.HTTPHeader{{Name: "Host", Value: "other.example.com"}}
			return route, gateway
		}},
		{"wrong listener port", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			gateway.Spec.Listeners[0].Port = 80
			return route, gateway
		}},
		{"TLS on tunnel listener", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			gateway.Spec.Listeners[0].TLS = &gatewayv1.ListenerTLSConfig{}
			return route, gateway
		}},
		{"foreign parent listener", func(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
			name := gatewayv1.SectionName("other")
			route.Spec.ParentRefs[0].SectionName = &name
			return route, gateway
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			changedRoute, changedGateway := test.change(*route.DeepCopy(), *gateway.DeepCopy())
			manifests := append([]map[string]any{}, rendered.Manifests...)
			for index, manifest := range manifests {
				switch manifest["kind"] {
				case "HTTPRoute":
					manifests[index] = applicationTunnelManifest(t, changedRoute)
				case "Gateway":
					manifests[index] = applicationTunnelManifest(t, changedGateway)
				}
			}
			_, err := Inspect(manifests)
			assert.True(err != nil)
		})
	}
	mixed := configuration
	mixed.Apps = append([]App{}, configuration.Apps...)
	mixed.Apps[0].Ingresses = append(append([]Ingress{}, configuration.Apps[0].Ingresses...), Ingress{Name: "direct", Host: "web.other.test", Path: "/"})
	mixedRendered, err := renderWithEnv(mixed, "apps", "space-one", "mixed", "pinned-issuer", nil, ResourceScope{Organization: "acme", Space: "production", Cluster: "local", tunnelDomain: "example.com"}, nil, ManagementS3{})
	assert.NoErr(err)
	direct := decodeResource[gatewayv1.Gateway](t, findManifest(t, mixedRendered, "Gateway", "web-direct"))
	assert.Equal(direct.Spec.Listeners[0].Protocol, gatewayv1.HTTPSProtocolType)
	assert.Equal(direct.Spec.Listeners[0].Port, gatewayv1.PortNumber(8443))
	assert.True(direct.Spec.Listeners[0].TLS != nil)
	certificateForOtherDomain := findManifest(t, mixedRendered, "Certificate", "web-direct-tls")
	assert.Equal(certificateForOtherDomain["spec"].(map[string]any)["issuerRef"].(map[string]any)["name"], "pinned-issuer")
	_, err = Inspect(mixedRendered.Manifests)
	assert.NoErr(err)
	secure, err := RenderForPlan(configuration, "apps", "space-one", "plan-old", "pinned-issuer")
	assert.NoErr(err)
	certificate := findManifest(t, secure, "Certificate", "web-public-tls")
	_, err = Inspect(append(rendered.Manifests, certificate))
	assert.True(err != nil)
	retained, retired := applicationRetiredCertificates(secure.Manifests, rendered.Manifests)
	assert.Equal(retired, []map[string]any{certificate})
	assert.NoErr(rejectResourceDeletions(retained, rendered.Manifests))
	// A changed hostname or deleted route must retain the normal deletion guard.
	configuration.Apps[0].Ingresses[0].Host = "other.example.com"
	changed, err := renderWithEnv(configuration, "apps", "space-one", "plan-changed", "pinned-issuer", nil, ResourceScope{Organization: "acme", Space: "production", Cluster: "local", tunnelDomain: "example.com"}, nil, ManagementS3{})
	assert.NoErr(err)
	retained, retired = applicationRetiredCertificates(secure.Manifests, changed.Manifests)
	assert.Equal(len(retired), 0)
	assert.True(rejectResourceDeletions(retained, changed.Manifests) != nil)
}

func applicationTunnelManifest(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	is.New(t).NoErr(err)
	var manifest map[string]any
	is.New(t).NoErr(json.Unmarshal(encoded, &manifest))
	return manifest
}

// TestApplicationTunnelDns keeps accepted local tunnel routes publishable
// without a public Gateway address; controller status here is policy input only.
func TestApplicationTunnelDns(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"nginx:1.27-alpine","port":80,"ingresses":[{"name":"public","host":"web.example.com"}]}]}`))
	assert.NoErr(err)
	rendered, err := renderWithEnv(configuration, "apps", "space-one", "origin", "issuer", nil, ResourceScope{Organization: "acme", Space: "production", Cluster: "local", tunnelDomain: "example.com"}, nil, ManagementS3{})
	assert.NoErr(err)
	route := decodeResource[gatewayv1.HTTPRoute](t, findManifest(t, rendered, "HTTPRoute", "web-public"))
	gateway := decodeResource[gatewayv1.Gateway](t, findManifest(t, rendered, "Gateway", "web-public"))
	claim := networkDnsClaim{source: "space/space-one", host: "web.example.com", namespace: "apps", spaceID: "space-one", tunnel: true, routes: []networkDnsRoute{{name: "web-public", application: "web"}}}
	readyRoute, readyGateway := networkDnsReadyRoute(claim)
	route.Generation, gateway.Generation = 1, 1
	route.Status, gateway.Status = readyRoute.Status, readyGateway.Status
	target := "11111111-1111-4111-8111-111111111111.cfargotunnel.com"
	endpoints, err := networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.NoErr(err)
	assert.Equal(endpoints, []any{map[string]any{"dnsName": claim.host, "recordType": "CNAME", "recordTTL": int64(300), "targets": []any{target}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	endpoints, err = networkDnsLiveEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway})
	assert.NoErr(err)
	assert.Equal(len(endpoints), 0)
	route.Spec.Rules[0].Filters = nil
	_, err = networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.True(err != nil)
}
