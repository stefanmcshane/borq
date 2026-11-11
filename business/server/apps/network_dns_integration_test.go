//go:build integration

package apps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestNetworkDnsInventoryIntegration owns accepted inventory -> live Kubernetes
// read -> DNSEndpoint CRUD. Status is explicit input: this proves publication
// and retention rules, not Gateway controller provisioning or Cloudflare writes.
func TestNetworkDnsInventoryIntegration(t *testing.T) {
	kubeconfig := networkDnsInventoryCluster(t)
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	identity, err := newId()
	assert.NoErr(err)
	namespace := "dns-inventory-" + identity[:12]
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	assert.NoErr(platform.EnsureNetworkDnsEndpoint(ctx, connection))
	client, err := networkDnsClient(Cluster{KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	assert.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	issuer := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer", "metadata": map[string]any{"name": "borq"},
		"spec": map[string]any{"acme": map[string]any{"server": "https://acme.example.test/directory", "email": "admin@example.com",
			"privateKeySecretRef": map[string]any{"name": "borq-acme"}, "solvers": []any{map[string]any{
				"selector": map[string]any{"dnsZones": []any{platform.ClusterID("example", "target") + ".example.com"}},
				"dns01":    map[string]any{"cloudflare": map[string]any{"apiTokenSecretRef": map[string]any{"name": "cloudflare", "key": "token"}}},
			}}}},
	}}
	assert.NoErr(client.Create(ctx, &issuer))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Errorf("remove DNS inventory fixture: %v", err)
		}
	})
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "inventory.sqlite"), ManagementKubeconfigPath: kubeconfig,
		PlatformSettings: platform.Settings{Namespace: namespace, Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, Cloudflare: &platform.CloudflareSettings{WildcardDomain: "example.com", ZoneID: strings.Repeat("a", 32), AcmeEmail: "admin@example.com"}}}}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	persist := func(collection, key string, value any) {
		previous, err := manager.database.Get(ctx, collection, key)
		if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
			assert.NoErr(err)
		}
		write, err := documentWrite(collection, key, value, previous.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	}
	// Use a distinct registration path so the deployment source has remote
	// authority even though this fixture shares one disposable Kubernetes API.
	deploymentKubeconfig := filepath.Join(t.TempDir(), "deployment.kubeconfig")
	credentials, err := os.ReadFile(kubeconfig)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(deploymentKubeconfig, credentials, 0600))
	cluster := Cluster{ID: platform.ClusterID("example", "target"), Organization: "example", Name: "target", KubeconfigPath: deploymentKubeconfig}
	hostname := "app." + cluster.ID + ".example.com"
	persist("cluster", "example/target", cluster)
	space := Space{ID: identity, Organization: "example", Name: "app", Cluster: "target", Namespace: namespace, CertificateIssuer: "borq",
		Configuration: Configuration{Apps: []App{{Name: "web", Ingresses: []Ingress{{Name: "public", Host: hostname}}}}}}
	persist("space", "example/app", spaceRecord{Space: space})
	inputClaim := networkDnsClaim{host: hostname, namespace: namespace, spaceID: identity, routes: []networkDnsRoute{{name: "web-public", application: "web"}}}
	route, gateway := networkDnsReadyRoute(inputClaim)
	pathType, path, port := gatewayv1.PathMatchPathPrefix, "/", gatewayv1.PortNumber(80)
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &path}}}, BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "web", Port: &port}}}}}}
	gateway.Spec.Listeners = []gatewayv1.Listener{{Name: "web", Port: 8000, Protocol: gatewayv1.HTTPProtocolType}}
	routeStatus := route.Status
	gatewayStatus := gateway.Status
	assert.NoErr(client.Create(ctx, &gateway))
	assert.NoErr(client.Create(ctx, &route))
	setAddresses := func(addresses ...networkDnsAddress) {
		assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&route), &route))
		route.Status = routeStatus
		for i := range route.Status.Parents {
			for j := range route.Status.Parents[i].Conditions {
				route.Status.Parents[i].Conditions[j].ObservedGeneration = route.Generation
				route.Status.Parents[i].Conditions[j].LastTransitionTime = metav1.Now()
				route.Status.Parents[i].Conditions[j].Reason = "FixtureInput"
			}
		}
		assert.NoErr(client.Status().Update(ctx, &route))
		assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&gateway), &gateway))
		gateway.Status = gatewayStatus
		for i := range gateway.Status.Conditions {
			gateway.Status.Conditions[i].ObservedGeneration = gateway.Generation
			gateway.Status.Conditions[i].LastTransitionTime = metav1.Now()
			gateway.Status.Conditions[i].Reason = "FixtureInput"
		}
		gateway.Status.Addresses = nil
		for _, address := range addresses {
			kind, value := gatewayv1.IPAddressType, address.IP
			if address.Hostname != "" {
				kind, value = gatewayv1.HostnameAddressType, address.Hostname
			}
			gateway.Status.Addresses = append(gateway.Status.Addresses, gatewayv1.GatewayStatusAddress{Type: &kind, Value: value})
		}
		assert.NoErr(client.Status().Update(ctx, &gateway))
	}

	claim := networkDnsClaim{source: "space/" + identity, host: hostname}
	readEndpoint := func() (unstructured.Unstructured, error) {
		endpoint := unstructured.Unstructured{Object: map[string]any{"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint"}}
		err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: claim.name()}, &endpoint)
		return endpoint, err
	}
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	_, err = readEndpoint()
	assert.True(apierrors.IsNotFound(err))
	setAddresses(networkDnsAddress{IP: "8.8.8.8"})
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	first, err := readEndpoint()
	assert.NoErr(err)
	firstEndpoints, _, err := unstructured.NestedSlice(first.Object, "spec", "endpoints")
	assert.NoErr(err)
	assert.Equal(firstEndpoints, []any{map[string]any{"dnsName": hostname, "recordType": "A", "recordTTL": int64(300), "targets": []any{"8.8.8.8"}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	unchanged, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(unchanged.GetResourceVersion(), first.GetResourceVersion())

	// Enabling scoped DNS must not delete previously published records while
	// an existing declaration still uses a hostname outside its cluster scope.
	space.Configuration.Apps[0].Ingresses[0].Host = "legacy.example.com"
	persist("space", "example/app", spaceRecord{Space: space})
	err = manager.reconcileNetworkDns(ctx)
	assert.True(err != nil && strings.Contains(err.Error(), "ClusterIssuer borq"))
	outsideScope, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(outsideScope.GetResourceVersion(), first.GetResourceVersion())
	space.Configuration.Apps[0].Ingresses[0].Host = hostname
	persist("space", "example/app", spaceRecord{Space: space})

	setAddresses()
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	pending, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(pending.GetResourceVersion(), first.GetResourceVersion())
	setAddresses(networkDnsAddress{IP: "10.0.0.1"})
	assert.True(manager.reconcileNetworkDns(ctx) != nil)
	private, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(private.GetResourceVersion(), first.GetResourceVersion())
	cluster.KubeconfigPath = filepath.Join(t.TempDir(), "missing")
	persist("cluster", "example/target", cluster)
	assert.True(manager.reconcileNetworkDns(ctx) != nil)
	unreachable, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(unreachable.GetResourceVersion(), first.GetResourceVersion())
	cluster.KubeconfigPath = deploymentKubeconfig
	persist("cluster", "example/target", cluster)

	// A second accepted scope cannot overwrite the original hostname's route.
	duplicate := space
	duplicate.ID, duplicate.Name = stableId("another-space"), "other"
	persist("space", "example/other", spaceRecord{Space: duplicate})
	setAddresses(networkDnsAddress{Hostname: "new-lb.example.net"})
	assert.True(manager.reconcileNetworkDns(ctx) != nil)
	conflicting, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(conflicting.GetResourceVersion(), first.GetResourceVersion())
	duplicate.Configuration.Apps = nil
	persist("space", "example/other", spaceRecord{Space: duplicate})
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	updated, err := readEndpoint()
	assert.NoErr(err)
	updatedEndpoints, _, err := unstructured.NestedSlice(updated.Object, "spec", "endpoints")
	assert.NoErr(err)
	assert.Equal(updatedEndpoints, []any{map[string]any{"dnsName": hostname, "recordType": "CNAME", "recordTTL": int64(300), "targets": []any{"new-lb.example.net"}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})

	// A failed issuer read retains that source's last record without blocking a
	// healthy application's update. Unmanaged names require no remote reads.
	failedCluster := Cluster{ID: platform.ClusterID("example", "offline"), Organization: "example", Name: "offline", KubeconfigPath: filepath.Join(t.TempDir(), "missing")}
	persist("cluster", "example/offline", failedCluster)
	failedSpace := Space{ID: stableId("offline-space"), Organization: "example", Name: "offline", Cluster: "offline", Namespace: namespace, CertificateIssuer: "borq",
		Configuration: Configuration{Apps: []App{{Name: "offline", Ingresses: []Ingress{{Name: "public", Host: "app." + failedCluster.ID + ".example.com"}}}}}}
	persist("space", "example/offline", spaceRecord{Space: failedSpace})
	failedClaim := networkDnsClaim{source: "space/" + failedSpace.ID, host: failedSpace.Configuration.Apps[0].Ingresses[0].Host}
	assert.NoErr(networkDnsPublish(ctx, client, namespace, failedClaim, []any{map[string]any{"dnsName": failedClaim.host, "recordType": "A", "recordTTL": int64(300), "targets": []any{"8.8.4.4"}}}))
	failedEndpoint := unstructured.Unstructured{Object: map[string]any{"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint"}}
	failedKey := ctrlclient.ObjectKey{Namespace: namespace, Name: failedClaim.name()}
	assert.NoErr(client.Get(ctx, failedKey, &failedEndpoint))
	failedVersion := failedEndpoint.GetResourceVersion()
	setAddresses(networkDnsAddress{IP: "1.1.1.1"})
	err = manager.reconcileNetworkDns(ctx)
	assert.True(err != nil && strings.Contains(err.Error(), "public route choices could not be read"))
	healthy, err := readEndpoint()
	assert.NoErr(err)
	healthyEndpoints, _, err := unstructured.NestedSlice(healthy.Object, "spec", "endpoints")
	assert.NoErr(err)
	assert.Equal(healthyEndpoints, []any{map[string]any{"dnsName": hostname, "recordType": "A", "recordTTL": int64(300), "targets": []any{"1.1.1.1"}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	assert.NoErr(client.Get(ctx, failedKey, &failedEndpoint))
	assert.Equal(failedEndpoint.GetResourceVersion(), failedVersion)
	failedSpace.Configuration.Apps[0].Ingresses[0].Host = "app.external.test"
	persist("space", "example/offline", spaceRecord{Space: failedSpace})
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	setAddresses(networkDnsAddress{Hostname: "new-lb.example.net"})
	assert.NoErr(manager.reconcileNetworkDns(ctx))

	foreign := updated.DeepCopy()
	foreign.SetName("foreign-endpoint")
	foreign.SetResourceVersion("")
	foreign.SetUID("")
	foreign.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "another-controller"})
	foreign.Object["spec"] = map[string]any{"endpoints": []any{map[string]any{"dnsName": "unrelated.example.com", "recordType": "A", "targets": []any{"1.1.1.1"}}}}
	assert.NoErr(client.Create(ctx, foreign))
	// Durable configuration, rather than the previous process's memory, owns cleanup.
	space.Configuration.Apps = nil
	persist("space", "example/app", spaceRecord{Space: space})
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	_, err = readEndpoint()
	assert.True(apierrors.IsNotFound(err))
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(foreign), foreign))

	// A tunnel deliberately fronts a private Gateway. Its exact management DNS
	// record must use the tunnel target without first requiring a public node IP.
	assert.NoErr(manager.Close())
	input.PlatformSettings.ClusterName = "production"
	input.PlatformSettings.Network.Cloudflare.Tunnel = &platform.CloudflareTunnelSettings{Enabled: true}
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	tunnelID := "01234567-89ab-cdef-0123-456789abcdef"
	assert.NoErr(client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-cloudflared", Namespace: namespace,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "borq.run/network": "true"}},
		Data: map[string]string{"domain": "example.com", "ready": "true", "tunnelId": tunnelID}}))
	claims, err := manager.networkDnsClaims(ctx)
	assert.NoErr(err)
	assert.Equal(len(claims), 1)
	claim = claims[0]
	assert.Equal(claim.host, "production.example.com")
	route, gateway = networkDnsReadyRoute(claim)
	for _, labels := range []map[string]string{route.Labels, gateway.Labels} {
		labels["borq.run/network"] = "true"
		labels["borq.run/management-cluster"] = "production"
	}
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "borq", Port: &port}}}}}}
	gateway.Spec.Listeners = []gatewayv1.Listener{{Name: "web", Port: 8000, Protocol: gatewayv1.HTTPProtocolType}}
	routeStatus, gatewayStatus = route.Status, gateway.Status
	assert.NoErr(client.Create(ctx, &gateway))
	assert.NoErr(client.Create(ctx, &route))
	setAddresses(networkDnsAddress{IP: "10.0.0.1"})
	failedSpace.Configuration.Apps[0].Ingresses[0].Host = failedClaim.host
	persist("space", "example/offline", spaceRecord{Space: failedSpace})
	err = manager.reconcileNetworkDns(ctx)
	assert.True(err != nil && strings.Contains(err.Error(), "public route choices could not be read"))
	managementEndpoint, err := readEndpoint()
	assert.NoErr(err)
	managementEndpoints, _, err := unstructured.NestedSlice(managementEndpoint.Object, "spec", "endpoints")
	assert.NoErr(err)
	assert.Equal(managementEndpoints, []any{map[string]any{"dnsName": "production.example.com", "recordType": "CNAME", "recordTTL": int64(300),
		"targets": []any{tunnelID + ".cfargotunnel.com"}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	failedSpace.Configuration.Apps = nil
	persist("space", "example/offline", spaceRecord{Space: failedSpace})

	// Tunnel intent must retain the last working tunnel record while its state
	// is pending, even if Traefik also advertises a public load-balancer address.
	setAddresses(networkDnsAddress{IP: "8.8.4.4"})
	var tunnelState corev1.ConfigMap
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: "borq-cloudflared"}, &tunnelState))
	tunnelState.Data["ready"] = "false"
	assert.NoErr(client.Update(ctx, &tunnelState))
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	retainedTunnel, err := readEndpoint()
	assert.NoErr(err)
	assert.Equal(retainedTunnel.GetResourceVersion(), managementEndpoint.GetResourceVersion())

	// A malformed management tunnel must not block a separate deployment
	// cluster's direct load-balancer publication in the same DNS reconciliation.
	space.Configuration.Apps = []App{{Name: "web", Ingresses: []Ingress{{Name: "public", Host: hostname}}}}
	persist("space", "example/app", spaceRecord{Space: space})
	tunnelState.Data["domain"] = "different.example.com"
	assert.NoErr(client.Update(ctx, &tunnelState))
	assert.True(manager.reconcileNetworkDns(ctx) != nil)
	deploymentEndpoint := unstructured.Unstructured{Object: map[string]any{"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint"}}
	deploymentClaim := networkDnsClaim{source: "space/" + identity, host: hostname}
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: deploymentClaim.name()}, &deploymentEndpoint))
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: claim.name()}, &retainedTunnel))
	assert.Equal(retainedTunnel.GetResourceVersion(), managementEndpoint.GetResourceVersion())

	// Explicit direct mode ignores stale tunnel state, then follows an actual
	// public Gateway address or the supplied static target for management only.
	assert.NoErr(manager.Close())
	input.PlatformSettings.Network.Cloudflare.Exposure = "direct"
	input.PlatformSettings.Network.Cloudflare.Tunnel = nil
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	assert.NoErr(manager.reconcileNetworkDns(ctx))
	directEndpoint, err := readEndpoint()
	assert.NoErr(err)
	directEndpoints, _, err := unstructured.NestedSlice(directEndpoint.Object, "spec", "endpoints")
	assert.NoErr(err)
	assert.Equal(directEndpoints, []any{map[string]any{"dnsName": "production.example.com", "recordType": "A", "recordTTL": int64(300), "targets": []any{"8.8.4.4"}, "providerSpecific": []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}}})
	for _, target := range []string{"1.1.1.1", "2606:4700:4700::1111", "public-lb.example.net"} {
		assert.NoErr(manager.Close())
		input.PlatformSettings.Network.Cloudflare.DirectTarget = target
		manager, err = NewManager(ctx, input)
		assert.NoErr(err)
		setAddresses(networkDnsAddress{IP: "10.0.0.1"})
		assert.NoErr(manager.reconcileNetworkDns(ctx))
		observed, err := readEndpoint()
		assert.NoErr(err)
		endpoints, _, err := unstructured.NestedSlice(observed.Object, "spec", "endpoints")
		assert.NoErr(err)
		endpoint := endpoints[0].(map[string]any)
		assert.Equal(endpoint["targets"], []any{target})
		kind := "CNAME"
		if networkDnsIsIp(target) {
			kind = "AAAA"
			if !strings.Contains(target, ":") {
				kind = "A"
			}
		}
		assert.Equal(endpoint["recordType"], kind)
		assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: deploymentClaim.name()}, &deploymentEndpoint))
		deploymentEndpoints, _, err := unstructured.NestedSlice(deploymentEndpoint.Object, "spec", "endpoints")
		assert.NoErr(err)
		assert.Equal(deploymentEndpoints[0].(map[string]any)["targets"], []any{"new-lb.example.net"})
	}
}

// The inventory fixture owns API persistence with explicit status inputs. A bare
// disposable cluster avoids racing a real Gateway controller over those inputs;
// controller success is separately proved by TestK3sGatewayIntegration.
func networkDnsInventoryCluster(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("borq-dns-%d", time.Now().UnixNano())
	command := func(binary string, args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", binary, err, output)
		}
		return output
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	is.New(t).NoErr(err)
	address := listener.Addr().String()
	is.New(t).NoErr(listener.Close())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if output, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", name).CombinedOutput(); err != nil {
			t.Errorf("delete inventory cluster: %v\n%s", err, output)
		}
	})
	command("k3d", "cluster", "create", name, "--image", "rancher/k3s:v1.35.3-k3s1", "--servers", "1", "--no-lb", "--api-port", address, "--volume", "/dev/null:/dev/kmsg@server:*", "--k3s-arg", "--disable=traefik@server:*", "--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*", "--k3s-arg", "--kube-proxy-arg=conntrack-max-per-core=0@server:*", "--k3s-arg", "--kubelet-arg=eviction-hard=nodefs.available<1%,imagefs.available<1%,nodefs.inodesFree<1%@server:*", "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--timeout", "120s")
	path := filepath.Join(t.TempDir(), "kubeconfig")
	is.New(t).NoErr(os.WriteFile(path, command("k3d", "kubeconfig", "get", name), 0600))
	command("kubectl", "--kubeconfig", path, "apply", "--server-side", "-f", "../platform/values/gateway-api.yaml")
	command("kubectl", "--kubeconfig", path, "apply", "-f", "../../../zarf/crds/certmanager/v1.20.2.yaml")
	command("kubectl", "--kubeconfig", path, "wait", "--for=condition=Established", "crd", "--all", "--timeout=60s")
	return path
}
