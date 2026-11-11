package apps

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestNetworkDnsHostBoundary keeps managed publication inside the configured
// zone and excludes protocol names, addresses and lookalike suffixes.
func TestNetworkDnsHostBoundary(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{"example.com", true}, {"app.example.com", true}, {"*.apps.example.com", true},
		{"otherexample.com", false}, {"example.com.evil.test", false}, {"_acme-challenge.example.com", false},
		{"app..example.com", false}, {"https://app.example.com", false}, {"app.example.com.", false},
	} {
		t.Run(test.host, func(t *testing.T) { is.New(t).Equal(networkDnsPublicHost(test.host, "example.com"), test.want) })
	}
	assert := is.New(t)
	assert.True(!networkDnsPublicHost("app.example.com", ""))
	assert.True(!networkDnsPublicHost("8.8.8.8", "8.8.8.8"))
}

// TestNetworkDnsTargets owns the public record contract for live ingress
// addresses, including dual stack, deterministic deduplication and refusal to
// publish local/private destinations. It does not assert controller readiness.
func TestNetworkDnsTargets(t *testing.T) {
	assert := is.New(t)
	endpoints, err := networkDnsTargets("app.example.com", []networkDnsAddress{{IP: "8.8.8.8"}, {IP: "2606:4700:4700::1111"}, {IP: "1.1.1.1"}, {IP: "8.8.8.8"}})
	assert.NoErr(err)
	assert.Equal(endpoints, []any{
		map[string]any{"dnsName": "app.example.com", "recordType": "A", "recordTTL": int64(300), "targets": []any{"1.1.1.1", "8.8.8.8"}},
		map[string]any{"dnsName": "app.example.com", "recordType": "AAAA", "recordTTL": int64(300), "targets": []any{"2606:4700:4700::1111"}},
	})
	endpoints, err = networkDnsTargets("app.example.com", []networkDnsAddress{{Hostname: "LOAD-BALANCER.example.net."}})
	assert.NoErr(err)
	assert.Equal(endpoints, []any{map[string]any{"dnsName": "app.example.com", "recordType": "CNAME", "recordTTL": int64(300), "targets": []any{"load-balancer.example.net"}}})
	for _, address := range []string{"10.0.0.1", "172.20.0.1", "192.168.1.1", "100.64.0.1", "127.0.0.1", "169.254.1.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "::ffff:10.0.0.1", "invalid"} {
		t.Run(address, func(t *testing.T) {
			_, err := networkDnsTargets("app.example.com", []networkDnsAddress{{IP: address}})
			is.New(t).True(err != nil)
		})
	}
	for _, hostname := range []string{"localhost", "service.namespace.svc.cluster.local", "app.example.com", "borq.tail123.ts.net", "host.internal", "8.8.8.8"} {
		t.Run(hostname, func(t *testing.T) {
			_, err := networkDnsTargets("app.example.com", []networkDnsAddress{{Hostname: hostname}})
			is.New(t).True(err != nil)
		})
	}
	for _, addresses := range [][]networkDnsAddress{
		{{IP: "8.8.8.8", Hostname: "lb.example.net"}},
		{{Hostname: "one.example.net"}, {Hostname: "two.example.net"}},
	} {
		_, err := networkDnsTargets("app.example.com", addresses)
		assert.True(err != nil)
	}
	endpoints, err = networkDnsTargets("app.example.com", nil)
	assert.NoErr(err)
	assert.Equal(len(endpoints), 0)
}

// TestNetworkDnsOwnership refuses foreign records even if they share Borq's
// generic managed-by label, and detects hostname conflicts under other names.
func TestNetworkDnsOwnership(t *testing.T) {
	assert := is.New(t)
	claim := networkDnsClaim{source: "space/identity", host: "app.example.com"}
	endpoint := unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"endpoints": []any{map[string]any{"dnsName": "app.example.com"}}}}}
	endpoint.SetName(claim.name())
	endpoint.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq"})
	assert.True(!networkDnsOwned(endpoint))
	assert.True(networkDnsEndpointConflict(claim, []unstructured.Unstructured{endpoint}))
	endpoint.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq", "borq.run/network-dns": "true", "borq.run/source-id": stableId(claim.source)})
	endpoint.SetAnnotations(map[string]string{"borq.run/source": claim.source, "borq.run/hostname": claim.host})
	assert.True(networkDnsOwned(endpoint))
	assert.True(!networkDnsEndpointConflict(claim, []unstructured.Unstructured{endpoint}))
	endpoint.SetName("foreign-name")
	assert.True(!networkDnsOwned(endpoint))
	assert.True(networkDnsEndpointConflict(claim, []unstructured.Unstructured{endpoint}))
	assert.True(!networkDnsEndpointConflict(networkDnsClaim{source: claim.source, host: "other.example.com"}, []unstructured.Unstructured{endpoint}))
}

// TestNetworkDnsLiveOwnership prevents live metadata or hostname drift from
// turning an accepted declaration into authority over a different workload.
func TestNetworkDnsLiveOwnership(t *testing.T) {
	claim := networkDnsClaim{source: "space/one", host: "app.example.com", namespace: "apps", spaceID: "one", routes: []networkDnsRoute{{name: "web-public", application: "web"}}}
	route, gateway := networkDnsReadyRoute(claim)
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "8.8.8.8"}}
	for _, test := range []struct {
		name   string
		change func(*gatewayv1.HTTPRoute)
	}{
		{"different name", func(value *gatewayv1.HTTPRoute) { value.Name = "different" }},
		{"foreign manager", func(value *gatewayv1.HTTPRoute) { value.Labels["app.kubernetes.io/managed-by"] = "another" }},
		{"foreign space", func(value *gatewayv1.HTTPRoute) { value.Labels["borq.run/space-id"] = "two" }},
		{"foreign application", func(value *gatewayv1.HTTPRoute) { value.Labels["app.kubernetes.io/name"] = "another" }},
		{"different host", func(value *gatewayv1.HTTPRoute) { value.Spec.Hostnames[0] = "another.example.com" }},
		{"foreign Gateway", func(value *gatewayv1.HTTPRoute) { value.Spec.ParentRefs[0].Name = "foreign" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := route.DeepCopy()
			test.change(value)
			_, err := networkDnsLiveEndpoints(claim, []gatewayv1.HTTPRoute{*value}, []gatewayv1.Gateway{gateway})
			is.New(t).True(err != nil)
		})
	}
	assert := is.New(t)
	endpoints, err := networkDnsLiveEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway})
	assert.NoErr(err)
	assert.Equal(len(endpoints), 1)
	for _, test := range []string{"pending route", "stale route", "pending gateway", "stale gateway"} {
		t.Run(test, func(t *testing.T) {
			r, g := route.DeepCopy(), gateway.DeepCopy()
			switch test {
			case "pending route":
				r.Status.Parents = nil
			case "stale route":
				r.Generation++
			case "pending gateway":
				g.Status.Conditions = nil
			case "stale gateway":
				g.Generation++
			}
			endpoints, err := networkDnsLiveEndpoints(claim, []gatewayv1.HTTPRoute{*r}, []gatewayv1.Gateway{*g})
			is.New(t).NoErr(err)
			is.New(t).Equal(len(endpoints), 0)
		})
	}
	pocket := claim
	pocket.spaceID, pocket.pocketID = "", "provider-one"
	_, err = networkDnsLiveEndpoints(pocket, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway})
	assert.True(err != nil)
	route.Labels["borq.io/pocket-id"] = pocket.pocketID
	gateway.Labels["borq.io/pocket-id"] = pocket.pocketID
	_, err = networkDnsLiveEndpoints(pocket, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway})
	assert.NoErr(err)
	secondClaim := claim
	secondClaim.routes = []networkDnsRoute{{name: "another-route", application: "web"}}
	second, secondGateway := networkDnsReadyRoute(secondClaim)
	secondGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "1.1.1.1"}}
	claim.routes = append(claim.routes, secondClaim.routes...)
	_, err = networkDnsLiveEndpoints(claim, []gatewayv1.HTTPRoute{route, second}, []gatewayv1.Gateway{gateway, secondGateway})
	assert.True(err != nil)
}

// networkDnsReadyRoute supplies controller status as input to pure DNS policy
// tests. Real controller acceptance is verified by the Gateway integration test.
func networkDnsReadyRoute(claim networkDnsClaim) (gatewayv1.HTTPRoute, gatewayv1.Gateway) {
	name := claim.routes[0].name
	labels := map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": claim.routes[0].application, "borq.run/space-id": claim.spaceID, "borq.io/pocket-id": claim.pocketID}
	metadata := metav1.ObjectMeta{Name: name, Namespace: claim.namespace, Labels: labels, Generation: 1}
	parent := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(name)}
	route := gatewayv1.HTTPRoute{ObjectMeta: metadata, Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}, Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(claim.host)}}}
	route.Status.Parents = []gatewayv1.RouteParentStatus{{ParentRef: parent, ControllerName: "traefik.io/gateway-controller", Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: "ResolvedRefs", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	gateway := gatewayv1.Gateway{ObjectMeta: metadata, Spec: gatewayv1.GatewaySpec{GatewayClassName: "borq-traefik"}, Status: gatewayv1.GatewayStatus{Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	return route, gateway
}

// TestNetworkDnsAcceptedInventory includes managed Pocket ID alongside accepted
// app routes, while excluding unrelated zones and grouping same-space hosts.
// Live policy failures are scoped to observation, leaving inventory available.
func TestNetworkDnsAcceptedInventory(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	api := newManagerCluster(t, managerClusterOptions{})
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "inventory.sqlite"),
		PlatformSettings: platform.Settings{Namespace: "borq-mgmt", Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, Cloudflare: &platform.CloudflareSettings{WildcardDomain: "example.com"}}}})
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	organization, err := manager.CreateOrganization(ctx, "example")
	assert.NoErr(err)
	cluster := Cluster{ID: platform.ClusterID("example", "target"), Organization: "example", Name: "target", KubeconfigPath: api.kubeconfig}
	hostname := "app." + cluster.ID + ".example.com"
	space := Space{ID: "space-one", Organization: "example", Name: "application", Namespace: "borq-space", Cluster: "target", CertificateIssuer: "borq",
		Configuration: Configuration{Apps: []App{{Name: "web", Ingresses: []Ingress{
			{Name: "public", Host: hostname}, {Name: "other-path", Host: hostname},
		}}}}}
	provider := organizationPocketIdRecord{ID: organization.ID, OrganizationID: organization.ID, Organization: organization.Name,
		Status: OrganizationPocketId{State: "failed", IssuerURL: "https://id.example.com", BaseURL: "https://borq.example.com", CertificateIssuer: "borq", AdminEmail: "admin@example.com"}}
	clusterWrite, err := documentWrite("cluster", "example/target", cluster, 0)
	assert.NoErr(err)
	spaceWrite, err := documentWrite("space", "example/application", spaceRecord{Space: space}, 0)
	assert.NoErr(err)
	providerWrite, err := documentWrite("organization-pocketid", organization.ID, provider, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{clusterWrite, spaceWrite, providerWrite}))
	claims, err := manager.networkDnsClaims(ctx)
	assert.NoErr(err)
	assert.Equal(len(claims), 2)
	byHost := map[string]networkDnsClaim{}
	for _, claim := range claims {
		byHost[claim.host] = claim
	}
	assert.Equal(byHost[hostname].namespace, "borq-space")
	assert.Equal(byHost[hostname].cluster.KubeconfigPath, api.kubeconfig)
	assert.Equal(byHost[hostname].routes, []networkDnsRoute{{name: "web-public", application: "web"}, {name: "web-other-path", application: "web"}})
	assert.Equal(byHost["id.example.com"].namespace, "borq-mgmt")
	assert.Equal(byHost["id.example.com"].pocketID, organization.ID)
	assert.Equal(byHost["id.example.com"].routes, []networkDnsRoute{{name: "pocketid-" + organization.ID}})

	for _, name := range []string{"app.example.com", "app." + platform.ClusterID("other", "target") + ".example.com", "*.example.com", "id.example.com"} {
		space.Configuration.Apps[0].Ingresses[0].Host = name
		previous, err := manager.database.Get(ctx, "space", "example/application")
		assert.NoErr(err)
		write, err := documentWrite("space", "example/application", spaceRecord{Space: space}, previous.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
		claims, err = manager.networkDnsClaims(ctx)
		assert.NoErr(err)
		for _, claim := range claims {
			if claim.spaceID == space.ID {
				observation := manager.networkDnsObserve(ctx, claim)
				assert.True(observation.err != nil && strings.Contains(observation.err.Error(), "ClusterIssuer borq"))
			}
		}
	}
	space.Configuration.Apps[0].Ingresses[0].Host = hostname
	previous, err := manager.database.Get(ctx, "space", "example/application")
	assert.NoErr(err)
	write, err := documentWrite("space", "example/application", spaceRecord{Space: space}, previous.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	api.server.Close()
	claims, err = manager.networkDnsClaims(ctx)
	assert.NoErr(err)
	assert.Equal(len(claims), 2)
	for _, claim := range claims {
		if claim.spaceID == space.ID {
			assert.True(manager.networkDnsObserve(ctx, claim).err != nil)
		}
	}
	provider.Status.IssuerURL = "https://id.management.example.com"
	previous, err = manager.database.Get(ctx, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	write, err = documentWrite("organization-pocketid", organization.ID, provider, previous.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	_, err = manager.networkDnsClaims(ctx)
	assert.True(err != nil && strings.Contains(err.Error(), "outside management DNS wildcard"))
}

// TestManagementTunnelDnsPublication proves a verified management ingress can
// publish a proxied tunnel CNAME without any public load-balancer IP, while
// missing or foreign ingresses cannot acquire a DNS record through the tunnel.
func TestManagementTunnelDnsPublication(t *testing.T) {
	assert := is.New(t)
	claim := networkDnsClaim{source: "pocketid/organization", host: "id.example.com", namespace: "borq-mgmt", pocketID: "identity", routes: []networkDnsRoute{{name: "pocketid-identity"}}}
	route, gateway := networkDnsReadyRoute(claim)
	target := "11111111-1111-4111-8111-111111111111.cfargotunnel.com"
	endpoints, err := networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.NoErr(err)
	assert.Equal(endpoints, []any{map[string]any{"dnsName": claim.host, "recordType": "CNAME", "recordTTL": int64(300), "targets": []any{target}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	assert.Equal(len(gateway.Status.Addresses), 0)
	_, err = networkDnsTunnelEndpoints(claim, nil, nil, target)
	assert.True(err != nil)
	route.Labels["borq.io/pocket-id"] = "someone-else"
	_, err = networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.True(err != nil)
}

// TestNetworkManagementWildcard keeps identity names directly inside the public
// wildcard while preventing DNS01 authority over target-cluster ID subtrees.
func TestNetworkManagementWildcard(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{"id.prod-test.borq.io", true}, {"other.prod-test.borq.io", true}, {"prod-test.borq.io", false},
		{"id.management.prod-test.borq.io", false}, {"*.prod-test.borq.io", false}, {"outside.borq.io", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.prod-test.borq.io", false},
	} {
		t.Run(test.host, func(t *testing.T) {
			is.New(t).Equal(networkDnsManagementHost(test.host, "prod-test.borq.io"), test.want)
		})
	}
}

// Management publication shares the wildcard tunnel target but retains its own
// exact HTTPRoute ownership and cluster-name-based DNS identity.
func TestNetworkManagementDnsClaim(t *testing.T) {
	assert := is.New(t)
	settings := platform.Settings{ClusterName: "production", Namespace: "borq-mgmt", Network: platform.NetworkSettings{Cloudflare: &platform.CloudflareSettings{WildcardDomain: "prod-test.borq.io"}}}
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: "/management/config", PlatformSettings: settings})
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	claims, err := manager.networkDnsClaims(t.Context())
	assert.NoErr(err)
	assert.Equal(len(claims), 1)
	claim := claims[0]
	assert.Equal(claim.host, "production.prod-test.borq.io")
	assert.Equal(claim.cluster.KubeconfigPath, "/management/config")
	assert.True(claim.management)
	assert.Equal(claim.routes, []networkDnsRoute{{name: "borq-management-public"}})
	route, gateway := networkDnsReadyRoute(claim)
	for _, labels := range []map[string]string{route.Labels, gateway.Labels} {
		labels["borq.run/network"] = "true"
		labels["borq.run/management-cluster"] = "production"
	}
	target := "01234567-89ab-cdef-0123-456789abcdef.cfargotunnel.com"
	gateway.Status.Addresses = nil
	endpoints, err := networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.NoErr(err)
	assert.Equal(endpoints, []any{map[string]any{"dnsName": claim.host, "recordType": "CNAME", "recordTTL": int64(300), "targets": []any{target}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	gateway.Labels["borq.run/management-cluster"] = "another-cluster"
	_, err = networkDnsTunnelEndpoints(claim, []gatewayv1.HTTPRoute{route}, []gatewayv1.Gateway{gateway}, target)
	assert.True(err != nil && strings.Contains(err.Error(), "not owned"))
	err = manager.validateNetworkPocketIdName("https://" + claim.host)
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "reserved for the management server"))
}
