package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type networkDnsRoute struct {
	name        string
	application string
}

type networkDnsObservation struct {
	routes   []gatewayv1.HTTPRoute
	gateways []gatewayv1.Gateway
	tunnel   bool
	err      error
}

type networkDnsClaim struct {
	source     string
	host       string
	cluster    Cluster
	namespace  string
	spaceID    string
	space      Space
	management bool
	tunnel     bool
	pocketID   string
	routes     []networkDnsRoute
}

func (claim networkDnsClaim) name() string {
	return "borq-" + stableId(claim.source+"/"+claim.host)
}

func (manager Manager) runNetworkDnsWorker() {
	defer manager.runtime.workers.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(manager.runtime.context, 2*time.Minute)
		err := manager.reconcileNetworkDns(ctx)
		if err == nil {
			settings, ownershipErr := manager.GetPlatformSettings(ctx)
			if ownershipErr == nil {
				ownershipErr = platform.CheckManagementDnsOwnership(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: settings})
			}
			if ownershipErr != nil && manager.runtime.context.Err() == nil {
				slog.ErrorContext(manager.runtime.context, "network_dns_ownership_check_failed", "error", ownershipErr)
			}
		}
		cancel()
		if err != nil && manager.runtime.context.Err() == nil {
			slog.ErrorContext(manager.runtime.context, "network_dns_inventory_failed", "error", err)
		}
		select {
		case <-manager.runtime.context.Done():
			return
		case <-ticker.C:
		}
	}
}

// Accepted configuration bounds publication; arbitrary HTTPRoutes in a linked
// cluster cannot acquire DNS records just by adding ExternalDNS annotations.
func (manager Manager) networkDnsClaims(ctx context.Context) ([]networkDnsClaim, error) {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings.Network.Cloudflare == nil {
		return nil, nil
	}
	spaces, err := manager.database.List(ctx, "space")
	if err != nil {
		return nil, err
	}
	claims := map[string]networkDnsClaim{}
	domain := settings.Network.Cloudflare.WildcardDomain
	if host := platform.ManagementPublicHostname(settings); host != "" {
		claim := networkDnsClaim{source: "management/" + settings.ClusterName, host: host,
			namespace: manager.networkDnsNamespace(), management: true,
			cluster: Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath}, routes: []networkDnsRoute{{name: "borq-management-public"}}}
		claims[claim.name()] = claim
	}
	for _, record := range spaces {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, fmt.Errorf("read DNS space inventory: %w", err)
		}
		space := stored.Space
		for _, application := range space.Configuration.Apps {
			for _, ingress := range application.Ingresses {
				if !networkDnsPublicHost(ingress.Host, domain) {
					continue
				}
				cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
				if err != nil {
					return nil, fmt.Errorf("read DNS cluster registration: %w", err)
				}
				claim := networkDnsClaim{source: "space/" + space.ID, host: ingress.Host, namespace: space.Namespace, spaceID: space.ID, space: space, cluster: cluster}
				previous := claims[claim.name()]
				claim.routes = append(previous.routes, networkDnsRoute{name: application.Name + "-" + ingress.Name, application: application.Name})
				claims[claim.name()] = claim
			}
		}
	}
	providers, err := manager.database.List(ctx, "organization-pocketid")
	if err != nil {
		return nil, err
	}
	for _, record := range providers {
		provider, _, err := manager.readPocketId(ctx, record.Key)
		if err != nil {
			return nil, fmt.Errorf("read DNS Pocket ID inventory: %w", err)
		}
		address, err := url.Parse(provider.Status.IssuerURL)
		if err != nil || address.Scheme != "https" || !networkDnsPublicHost(address.Hostname(), domain) {
			continue
		}
		if !networkDnsManagementHost(address.Hostname(), domain) {
			return nil, fmt.Errorf("Pocket ID hostname %s is outside management DNS wildcard %s; existing DNS records are preserved", address.Hostname(), domain)
		}
		claim := networkDnsClaim{source: "pocketid/" + provider.OrganizationID, host: address.Hostname(), namespace: manager.networkDnsNamespace(),
			pocketID: provider.ID, cluster: Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath}, routes: []networkDnsRoute{{name: "pocketid-" + provider.ID}}}
		claims[claim.name()] = claim
	}
	result := make([]networkDnsClaim, 0, len(claims))
	for _, claim := range claims {
		result = append(result, claim)
	}
	slices.SortFunc(result, func(left, right networkDnsClaim) int { return strings.Compare(left.name(), right.name()) })
	return result, nil
}

func (manager Manager) networkDnsNamespace() string {
	if namespace := manager.runtime.platformSettings.Namespace; namespace != "" {
		return namespace
	}
	return platform.ManagementNamespace
}

func networkDnsPublicHost(host, domain string) bool {
	base := strings.TrimPrefix(host, "*.")
	if domain == "" || len(validation.IsDNS1123Subdomain(base)) != 0 || len(validation.IsDNS1123Subdomain(domain)) != 0 {
		return false
	}
	if base != domain && !strings.HasSuffix(base, "."+domain) {
		return false
	}
	return !strings.Contains(base, "_acme-challenge") && !networkDnsIsIp(base)
}

// Management identities occupy individual names directly below the configured
// wildcard. Reserved cluster-ID labels cannot become management identities,
// because DNS01 authorizations also authorize wildcard children of that name.
func networkDnsManagementHost(host, domain string) bool {
	if !networkDnsPublicHost(host, domain) {
		return false
	}
	label := strings.TrimSuffix(host, "."+domain)
	return label != host && label != "" && !strings.ContainsAny(label, ".*") && !(len(label) == 32 && strings.Trim(label, "0123456789abcdef") == "")
}

func networkDnsIsIp(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

func (manager Manager) reconcileNetworkDns(ctx context.Context) error {
	configured, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return err
	}
	if configured.Network.Cloudflare == nil {
		return nil
	}
	claims, err := manager.networkDnsClaims(ctx)
	if err != nil {
		return err
	}
	client, err := networkDnsClient(Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath})
	if err != nil {
		return err
	}
	var existing unstructured.UnstructuredList
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "externaldns.k8s.io", Version: "v1alpha1", Kind: "DNSEndpointList"})
	if err := client.List(ctx, &existing, ctrlclient.InNamespace(manager.networkDnsNamespace())); err != nil {
		return fmt.Errorf("read managed DNS endpoints: %w", err)
	}
	wanted := map[string]bool{}
	owners := map[string]string{}
	conflicts := map[string]bool{}
	for _, claim := range claims {
		wanted[claim.name()] = true
		if source, exists := owners[claim.host]; exists && source != claim.source {
			conflicts[claim.host] = true
		}
		owners[claim.host] = claim.source
	}
	settings := configured.Network.Cloudflare
	mode := settings.ExposureMode()
	if mode != "tunnel" && mode != "direct" {
		return errors.New("management DNS exposure must be tunnel or direct")
	}
	var tunnelTarget string
	var tunnelErr error
	if mode == "tunnel" {
		tunnelTarget, tunnelErr = platform.ReadManagementTunnel(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: configured})
	}
	var failures []error
	failedSources := map[string]bool{}
	observations := map[string]networkDnsObservation{}
	for _, claim := range claims {
		if conflicts[claim.host] {
			failures = append(failures, fmt.Errorf("DNS hostname %s is claimed by multiple scopes", claim.host))
			continue
		}
		if networkDnsEndpointConflict(claim, existing.Items) {
			failures = append(failures, fmt.Errorf("DNS hostname %s has an endpoint owned by another source", claim.host))
			continue
		}
		observation, observed := observations[claim.source]
		if !observed {
			observation = manager.networkDnsObserve(ctx, claim)
			observations[claim.source] = observation
		}
		if observation.err != nil {
			failedSources[claim.source] = true
			failures = append(failures, fmt.Errorf("read DNS cluster inventory: %w", observation.err))
			continue
		}
		claim.tunnel = observation.tunnel
		var endpoints []any
		management := claim.management || claim.pocketID != ""
		if (management || claim.tunnel) && mode == "tunnel" {
			if tunnelErr != nil {
				failures = append(failures, fmt.Errorf("read management tunnel for %s: %w", claim.source, tunnelErr))
				continue
			}
			// A pending tunnel must retain its last record, never silently
			// switch to an unrelated public Gateway address.
			if tunnelTarget == "" {
				continue
			}
			endpoints, err = networkDnsTunnelEndpoints(claim, observation.routes, observation.gateways, tunnelTarget)
		} else if management && settings.DirectTarget != "" {
			endpoints, err = networkDnsRouteEndpoints(claim, observation.routes, observation.gateways, settings.DirectTarget)
		} else {
			endpoints, err = networkDnsLiveEndpoints(claim, observation.routes, observation.gateways)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("read DNS source %s: %w", claim.source, err))
			continue
		}
		// Pending HTTPRoute or temporary loss of its addresses is not a request
		// to delete the last known working public record.
		if len(endpoints) == 0 {
			continue
		}
		networkDnsProxyEndpoints(endpoints)
		if err := networkDnsPublish(ctx, client, manager.networkDnsNamespace(), claim, endpoints); err != nil {
			failures = append(failures, err)
		}
	}
	// Re-read the accepted inventory before destructive cleanup. An application
	// changed while remote reads were in flight must wait for the next pass.
	current, err := manager.networkDnsClaims(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	if !reflect.DeepEqual(claims, current) {
		return errors.Join(append(failures, errors.New("DNS inventory changed during reconciliation"))...)
	}
	for _, endpoint := range existing.Items {
		if wanted[endpoint.GetName()] || failedSources[endpoint.GetAnnotations()["borq.run/source"]] || !networkDnsOwned(endpoint) {
			continue
		}
		uid, revision := endpoint.GetUID(), endpoint.GetResourceVersion()
		// Preconditions prevent deleting a replacement created after the list.
		err := client.Delete(ctx, &endpoint, &ctrlclient.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}})
		if err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove obsolete DNS endpoint: %w", err))
		}
	}
	return errors.Join(failures...)
}

func networkDnsClient(cluster Cluster) (ctrlclient.Client, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return nil, err
	}
	config := connection.RestConfig()
	config.Timeout = 10 * time.Second
	return ctrlclient.New(&config, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
}

func (manager Manager) networkDnsObserve(ctx context.Context, claim networkDnsClaim) networkDnsObservation {
	tunnel := false
	if claim.spaceID != "" {
		if err := manager.validateApplicationRoutes(ctx, claim.space, claim.space.Configuration); err != nil {
			return networkDnsObservation{err: fmt.Errorf("validate accepted DNS routes: %w", err)}
		}
		local, err := manager.applicationOnManagementCluster(ctx, claim.cluster)
		if err != nil {
			return networkDnsObservation{err: fmt.Errorf("read DNS cluster identity: %w", err)}
		}
		settings, err := manager.GetPlatformSettings(ctx)
		if err != nil {
			return networkDnsObservation{err: err}
		}
		tunnel = local && settings.Network.Cloudflare != nil && settings.Network.Cloudflare.ExposureMode() == "tunnel"
	}
	client, err := networkDnsClient(claim.cluster)
	if err != nil {
		return networkDnsObservation{err: err}
	}
	var routes gatewayv1.HTTPRouteList
	if err := client.List(ctx, &routes, ctrlclient.InNamespace(claim.namespace)); err != nil {
		return networkDnsObservation{err: err}
	}
	var gateways gatewayv1.GatewayList
	err = client.List(ctx, &gateways, ctrlclient.InNamespace(claim.namespace))
	return networkDnsObservation{routes: routes.Items, gateways: gateways.Items, tunnel: tunnel, err: err}
}

func networkDnsLiveEndpoints(claim networkDnsClaim, routes []gatewayv1.HTTPRoute, gateways []gatewayv1.Gateway) ([]any, error) {
	return networkDnsRouteEndpoints(claim, routes, gateways, "")
}

func networkDnsRouteEndpoints(claim networkDnsClaim, routes []gatewayv1.HTTPRoute, gateways []gatewayv1.Gateway, publicTarget string) ([]any, error) {
	var endpoints []any
	for _, accepted := range claim.routes {
		index := slices.IndexFunc(routes, func(route gatewayv1.HTTPRoute) bool { return route.Name == accepted.name })
		if index < 0 {
			return nil, errors.New("accepted HTTPRoute is not present yet")
		}
		route := routes[index]
		if route.DeletionTimestamp != nil {
			return nil, nil
		}
		if route.Labels["app.kubernetes.io/managed-by"] != "borq" ||
			(claim.spaceID != "" && (route.Labels["borq.run/space-id"] != claim.spaceID || route.Labels["app.kubernetes.io/name"] != accepted.application)) ||
			(claim.pocketID != "" && route.Labels["borq.io/pocket-id"] != claim.pocketID) ||
			(claim.management && (route.Labels["borq.run/network"] != "true" || route.Labels["borq.run/management-cluster"] != strings.TrimPrefix(claim.source, "management/"))) {
			return nil, errors.New("live HTTPRoute is not owned by the accepted application")
		}
		if !slices.Contains(route.Spec.Hostnames, gatewayv1.Hostname(claim.host)) {
			return nil, errors.New("live HTTPRoute no longer serves the accepted public hostname")
		}
		if len(route.Spec.ParentRefs) != 1 {
			return nil, errors.New("HTTPRoute must bind its managed Gateway")
		}
		parent := route.Spec.ParentRefs[0]
		if string(parent.Name) != route.Name || parent.Namespace != nil && string(*parent.Namespace) != claim.namespace || parent.Kind != nil && *parent.Kind != "Gateway" || parent.Group != nil && *parent.Group != gatewayv1.GroupName {
			return nil, errors.New("HTTPRoute must bind its managed Gateway")
		}
		gatewayIndex := slices.IndexFunc(gateways, func(gateway gatewayv1.Gateway) bool { return gateway.Name == route.Name })
		if gatewayIndex < 0 {
			return nil, errors.New("HTTPRoute Gateway is not present yet")
		}
		gateway := gateways[gatewayIndex]
		if gateway.Spec.GatewayClassName != "borq-traefik" || gateway.Labels["app.kubernetes.io/managed-by"] != "borq" || gateway.DeletionTimestamp != nil ||
			(claim.spaceID != "" && (gateway.Labels["borq.run/space-id"] != claim.spaceID || gateway.Labels["app.kubernetes.io/name"] != accepted.application)) ||
			(claim.pocketID != "" && gateway.Labels["borq.io/pocket-id"] != claim.pocketID) ||
			(claim.management && (gateway.Labels["borq.run/network"] != "true" || gateway.Labels["borq.run/management-cluster"] != strings.TrimPrefix(claim.source, "management/"))) {
			return nil, errors.New("HTTPRoute Gateway is not owned by Borq")
		}
		if claim.tunnel && !applicationTunnelRoute(route, gateway) {
			return nil, errors.New("application tunnel route does not match its managed HTTP origin")
		}
		acceptedParent := slices.ContainsFunc(route.Status.Parents, func(status gatewayv1.RouteParentStatus) bool {
			return status.ControllerName == "traefik.io/gateway-controller" && string(status.ParentRef.Name) == route.Name &&
				networkDnsCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted), route.Generation) &&
				networkDnsCondition(status.Conditions, string(gatewayv1.RouteConditionResolvedRefs), route.Generation)
		})
		if !acceptedParent || !networkDnsCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), gateway.Generation) {
			return nil, nil
		}
		var addresses []networkDnsAddress
		for _, address := range gateway.Status.Addresses {
			if address.Type == nil || *address.Type == gatewayv1.IPAddressType {
				addresses = append(addresses, networkDnsAddress{IP: address.Value})
			} else if *address.Type == gatewayv1.HostnameAddressType {
				addresses = append(addresses, networkDnsAddress{Hostname: address.Value})
			}
		}
		if publicTarget != "" {
			address := networkDnsAddress{Hostname: publicTarget}
			if networkDnsIsIp(publicTarget) {
				address = networkDnsAddress{IP: publicTarget}
			}
			addresses = []networkDnsAddress{address}
		}
		current, err := networkDnsTargets(claim.host, addresses)
		if err != nil || len(current) == 0 {
			return nil, err
		}
		if endpoints != nil && !reflect.DeepEqual(endpoints, current) {
			return nil, errors.New("HTTPRoutes sharing a hostname report different public targets")
		}
		endpoints = current
	}
	return endpoints, nil
}

func networkDnsCondition(conditions []metav1.Condition, kind string, generation int64) bool {
	condition := meta.FindStatusCondition(conditions, kind)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

type networkDnsAddress struct{ IP, Hostname string }

func networkDnsTargets(host string, addresses []networkDnsAddress) ([]any, error) {
	targets := map[string][]string{}
	for _, address := range addresses {
		if address.IP != "" {
			ip, err := netip.ParseAddr(address.IP)
			if err != nil || !networkDnsPublicAddress(ip) {
				return nil, errors.New("Gateway has a non-public load balancer address")
			}
			kind := "AAAA"
			if ip.Unmap().Is4() {
				kind = "A"
			}
			targets[kind] = append(targets[kind], ip.Unmap().String())
		}
		if address.Hostname != "" {
			hostname := strings.ToLower(strings.TrimSuffix(address.Hostname, "."))
			if hostname == host || !strings.Contains(hostname, ".") || len(validation.IsDNS1123Subdomain(hostname)) != 0 || networkDnsIsIp(hostname) ||
				strings.HasSuffix(hostname, ".local") || strings.HasSuffix(hostname, ".internal") || strings.HasSuffix(hostname, ".ts.net") || strings.HasSuffix(hostname, ".cluster.local") {
				return nil, errors.New("Gateway has an invalid public load balancer hostname")
			}
			targets["CNAME"] = append(targets["CNAME"], hostname)
		}
	}
	result := []any{}
	for _, kind := range []string{"A", "AAAA", "CNAME"} {
		values := targets[kind]
		slices.Sort(values)
		values = slices.Compact(values)
		if len(values) == 0 {
			continue
		}
		if kind == "CNAME" && (len(targets) != 1 || len(values) != 1) {
			return nil, errors.New("Gateway mixes CNAME and address records or reports multiple CNAME targets")
		}
		encoded := make([]any, len(values))
		for index, value := range values {
			encoded[index] = value
		}
		result = append(result, map[string]any{"dnsName": host, "recordType": kind, "recordTTL": int64(300), "targets": encoded})
	}
	return result, nil
}

func networkDnsPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	// These special-use ranges are not globally reachable even though netip
	// classifies them as unicast. They include tailnet, documentation and test IPs.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001:2::/48"} {
		if netip.MustParsePrefix(cidr).Contains(address) {
			return false
		}
	}
	return address.Is4() || netip.MustParsePrefix("2000::/3").Contains(address)
}

func networkDnsOwned(endpoint unstructured.Unstructured) bool {
	labels, annotations := endpoint.GetLabels(), endpoint.GetAnnotations()
	source, host := annotations["borq.run/source"], annotations["borq.run/hostname"]
	return labels["app.kubernetes.io/managed-by"] == "borq" && labels["borq.run/network-dns"] == "true" &&
		source != "" && host != "" && labels["borq.run/source-id"] == stableId(source) && endpoint.GetName() == (networkDnsClaim{source: source, host: host}).name()
}

func networkDnsEndpointConflict(claim networkDnsClaim, existing []unstructured.Unstructured) bool {
	for _, object := range existing {
		if object.GetName() == claim.name() && networkDnsOwned(object) {
			continue
		}
		if object.GetName() == claim.name() {
			return true
		}
		endpoints, _, _ := unstructured.NestedSlice(object.Object, "spec", "endpoints")
		for _, value := range endpoints {
			endpoint, ok := value.(map[string]any)
			if ok && strings.EqualFold(strings.TrimSuffix(fmt.Sprint(endpoint["dnsName"]), "."), claim.host) {
				return true
			}
		}
	}
	return false
}

func networkDnsPublish(ctx context.Context, client ctrlclient.Client, namespace string, claim networkDnsClaim, endpoints []any) error {
	desired := unstructured.Unstructured{Object: map[string]any{"apiVersion": "externaldns.k8s.io/v1alpha1", "kind": "DNSEndpoint", "spec": map[string]any{"endpoints": endpoints}}}
	desired.SetName(claim.name())
	desired.SetNamespace(namespace)
	desired.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "borq", "borq.run/network-dns": "true", "borq.run/source-id": stableId(claim.source)})
	desired.SetAnnotations(map[string]string{"borq.run/source": claim.source, "borq.run/hostname": claim.host})
	live := desired.DeepCopy()
	err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(&desired), live)
	if apierrors.IsNotFound(err) {
		return client.Create(ctx, &desired)
	}
	if err != nil {
		return err
	}
	if !networkDnsOwned(*live) {
		return errors.New("refusing to replace a DNS endpoint owned by another source")
	}
	if reflect.DeepEqual(live.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	live.Object["spec"] = desired.Object["spec"]
	return client.Update(ctx, live)
}

// Tunnel publication retains route ownership and controller acceptance checks,
// while supplying the public target for a private Gateway without a load balancer.
func networkDnsTunnelEndpoints(claim networkDnsClaim, routes []gatewayv1.HTTPRoute, gateways []gatewayv1.Gateway, target string) ([]any, error) {
	endpoints, err := networkDnsRouteEndpoints(claim, routes, gateways, target)
	if err != nil {
		return nil, err
	}
	networkDnsProxyEndpoints(endpoints)
	return endpoints, nil
}

func networkDnsProxyEndpoints(endpoints []any) {
	for _, endpoint := range endpoints {
		endpoint.(map[string]any)["providerSpecific"] = []any{map[string]any{"name": "cloudflare/proxied", "value": "true"}}
	}
}
