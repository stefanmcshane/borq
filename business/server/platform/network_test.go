package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func networkTestSettings() NetworkSettings {
	return NetworkSettings{Tailscale: &TailscaleSettings{}, Cloudflare: &CloudflareSettings{WildcardDomain: "example.com", ZoneID: strings.Repeat("a", 32), AcmeEmail: "admin@example.com"}}
}

// TestNetworkConfiguration owns safe input validation and disabled compatibility.
func TestNetworkConfiguration(t *testing.T) {
	is := is.New(t)
	disabled, err := NormalizeNetworkSettings(NetworkSettings{}, "")
	is.NoErr(err)
	is.Equal(disabled, NetworkSettings{})
	input := networkTestSettings()
	input.Cloudflare.ZoneID = strings.ToUpper(input.Cloudflare.ZoneID)
	normalized, err := NormalizeNetworkSettings(input, "home")
	is.NoErr(err)
	is.Equal(normalized.Tailscale.ManagementHostname, "borq-home")
	is.Equal(normalized.Cloudflare.ZoneID, strings.Repeat("a", 32))
	is.Equal(normalized.Tailscale.OperatorTag, "tag:borq-operator")
	is.Equal(normalized.Tailscale.ManagementOperatorTag, "tag:borq-management-operator")
	is.Equal(normalized.Tailscale.ProxyTag, "tag:borq")
	is.Equal(normalized.Tailscale.AdminTag, "tag:borq-admin")
	is.Equal(normalized.Tailscale.BrokerTag, "")
	is.Equal(input.Tailscale.ManagementHostname, "")
	is.Equal(input.Cloudflare.ZoneID, strings.Repeat("A", 32))
	private, err := NormalizeNetworkSettings(NetworkSettings{Tailscale: &TailscaleSettings{}}, "private")
	is.NoErr(err)
	is.Equal(private.Tailscale.ManagementHostname, "borq-private")
	is.True(private.Cloudflare == nil)
	public, err := NormalizeNetworkSettings(NetworkSettings{Cloudflare: input.Cloudflare}, "public")
	is.NoErr(err)
	is.True(public.Tailscale == nil && public.Cloudflare != nil)
	normalized.ManagementURL = "https://admin.test.ts.net"
	encoded, err := json.Marshal(normalized)
	is.NoErr(err)
	is.True(strings.Contains(string(encoded), `"managementUrl":"https://admin.test.ts.net"`))
	is.True(!strings.Contains(string(encoded), "brokerUrl"))
	is.True(strings.Contains(string(encoded), `"tailscale":`) && strings.Contains(string(encoded), `"cloudflare":`))
	for name, change := range map[string]func(*NetworkSettings){
		"ip zone":          func(settings *NetworkSettings) { settings.Cloudflare.WildcardDomain = "127.0.0.1" },
		"wildcard zone":    func(settings *NetworkSettings) { settings.Cloudflare.WildcardDomain = "**.example.com" },
		"invalid zone ID":  func(settings *NetworkSettings) { settings.Cloudflare.ZoneID = "secret-bearing-do-not-print" },
		"display email":    func(settings *NetworkSettings) { settings.Cloudflare.AcmeEmail = "Admin <admin@example.com>" },
		"device name":      func(settings *NetworkSettings) { settings.Tailscale.ManagementHostname = "device.tail.ts.net" },
		"tag":              func(settings *NetworkSettings) { settings.Tailscale.ProxyTag = "autogroup:member" },
		"shared admin tag": func(settings *NetworkSettings) { settings.Tailscale.AdminTag = settings.Tailscale.ProxyTag },
		"shared operator tag": func(settings *NetworkSettings) {
			settings.Tailscale.ManagementOperatorTag = settings.Tailscale.OperatorTag
		},
		"insecure origin": func(settings *NetworkSettings) { settings.ManagementURL = "http://device.tail.ts.net" },
		"credential origin": func(settings *NetworkSettings) {
			settings.ManagementURL = "https://secret-bearing-do-not-print@device.tail.ts.net"
		},
		"wrong origin": func(settings *NetworkSettings) { settings.ManagementURL = "https://api.example.com" },
		"URL path":     func(settings *NetworkSettings) { settings.ManagementURL = "https://device.tail.ts.net/other" },
	} {
		t.Run(name, func(t *testing.T) {
			settings, err := NormalizeNetworkSettings(normalized, "home")
			if err != nil {
				t.Fatal(err)
			}
			change(&settings)
			err = ValidateNetworkSettings(settings)
			if err == nil || strings.Contains(err.Error(), "secret-bearing") {
				t.Fatalf("invalid settings did not reach safe validation: %v", err)
			}
		})
	}
}

// TestNetworkCatalog owns controller scope and credentials staying out of Helm values.
func TestNetworkCatalog(t *testing.T) {
	is := is.New(t)
	settings, err := normalizeSettings(Settings{Network: networkTestSettings()})
	is.NoErr(err)
	management := managementCatalog(settings, false)
	target := networkTargetCatalog(settings, false)
	is.True(strings.Contains(componentNames(management), "external-dns"))
	is.True(strings.Contains(componentNames(management), "tailscale-operator"))
	is.True(!strings.Contains(componentNames(target), "external-dns"))
	is.True(strings.Contains(componentNames(target), "tailscale-operator"))
	is.True(!strings.Contains(componentNames(managementCatalog(Settings{Profile: "dev"}, false)), "tailscale"))
	private := settings
	private.Network.Cloudflare = nil
	is.True(strings.Contains(componentNames(managementCatalog(private, false)), "tailscale-operator"))
	is.True(!strings.Contains(componentNames(managementCatalog(private, false)), "external-dns"))
	operator, err := renderComponentValues(tailscaleComponent(), valuesInput{Settings: settings})
	is.NoErr(err)
	is.Equal(operator["oauthSecretVolume"].(map[string]any)["secret"].(map[string]any)["secretName"], "operator-oauth")
	is.Equal(operator["operatorConfig"].(map[string]any)["defaultTags"], []any{"tag:borq-management-operator"})
	proxyClass := networkProxyClass()
	is.Equal(operator["proxyConfig"].(map[string]any)["defaultProxyClass"], proxyClass.GetName())
	is.Equal(proxyClass.GetNamespace(), "")
	is.NoErr(checkNetworkOwner(proxyClass))
	for container, memory := range map[string]string{"tailscaleContainer": "64Mi", "tailscaleInitContainer": "16Mi"} {
		request, found, err := unstructured.NestedString(proxyClass.Object, "spec", "statefulSet", "pod", container, "resources", "requests", "memory")
		is.NoErr(err)
		is.True(found)
		is.Equal(request, memory)
	}
	targetOperator, err := renderComponentValues(tailscaleComponent(), valuesInput{Settings: settings, ClusterID: "target"})
	is.NoErr(err)
	is.Equal(targetOperator["operatorConfig"].(map[string]any)["defaultTags"], []any{"tag:borq-operator"})
	_, hasOAuth := operator["oauth"]
	is.True(!hasOAuth)
	dns, err := renderComponentValues(externalDnsComponent(), valuesInput{Settings: settings, NetworkOwner: "borq-physical-uid"})
	is.NoErr(err)
	is.Equal(dns["sources"], []any{"crd"})
	is.Equal(dns["namespaced"], true)
	is.Equal(dns["sourceNamespace"], ManagementNamespace)
	is.Equal(dns["txtOwnerId"], "borq-physical-uid")
	is.Equal(dns["domainFilters"], []any{"example.com"})
	is.Equal(dns["extraArgs"].(map[string]any)["zone-id-filter"], settings.Network.Cloudflare.ZoneID)
	is.Equal(dns["extraArgs"].(map[string]any)["txt-wildcard-replacement"], "_wildcard")
	env := dns["env"].([]any)[0].(map[string]any)
	is.Equal(env["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)["key"], "cloudflareApiToken")
	is.True(ValidateComponentOverrides(map[string]ComponentOverride{"external-dns": {Values: map[string]any{"env": "credential"}}}) != nil)
	is.True(ValidateComponentOverrides(map[string]ComponentOverride{"tailscale-operator": {}}) != nil)
	encoded, _ := json.Marshal(TargetInput{TailscaleClientID: "do-not-serialize", TailscaleClientSecret: "do-not-serialize"})
	is.True(!strings.Contains(string(encoded), "do-not-serialize"))
	ingress := managementNetworkIngress(settings)
	is.Equal(ingress.Spec.TLS[0].Hosts, []string{"borq-management"})
	is.Equal(ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number, int32(80))
	is.Equal(len(ingress.Status.LoadBalancer.Ingress), 0)
	is.Equal(ingress.Annotations["tailscale.com/tags"], "tag:borq-admin")

}

// TestNetworkProxyClassOwnership rejects a foreign class before Helm can select
// it as the operator default, without issuing any Kubernetes mutations.
func TestNetworkProxyClassOwnership(t *testing.T) {
	is := is.New(t)
	foreign := networkProxyClass()
	foreign.SetLabels(nil)
	version := metav1.GroupVersionForDiscovery{GroupVersion: "tailscale.com/v1alpha1", Version: "v1alpha1"}
	group := metav1.APIGroup{Name: "tailscale.com", Versions: []metav1.GroupVersionForDiscovery{version}, PreferredVersion: version}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("unexpected mutation before ownership check: %s %s", request.Method, request.URL.Path)
			http.Error(writer, "unexpected mutation", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/api":
			writeOwnershipResponse(writer, http.StatusOK, metav1.APIVersions{Versions: []string{"v1"}})
		case "/apis":
			writeOwnershipResponse(writer, http.StatusOK, metav1.APIGroupList{Groups: []metav1.APIGroup{group}})
		case "/apis/tailscale.com":
			writeOwnershipResponse(writer, http.StatusOK, group)
		case "/apis/tailscale.com/v1alpha1":
			writeOwnershipResponse(writer, http.StatusOK, metav1.APIResourceList{GroupVersion: version.GroupVersion, APIResources: []metav1.APIResource{{Name: "proxyclasses", Kind: "ProxyClass"}}})
		case "/apis/tailscale.com/v1alpha1/proxyclasses/borq":
			writeOwnershipResponse(writer, http.StatusOK, foreign)
		default:
			t.Errorf("unexpected request before ownership check: %s", request.URL.Path)
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	connection, path := ownershipTestConnection(t, server.URL)
	input := componentRun{connection: connection, kubeconfigPath: path, scope: "target"}
	_, err := input.reconcile(context.Background(), tailscaleComponent())
	is.True(err != nil && strings.Contains(err.Error(), "not owned"))
}

// TestNetworkWildcardDomain treats starred and bare public suffixes identically,
// without changing the caller's input or accepting interior wildcard labels.
func TestNetworkWildcardDomain(t *testing.T) {
	assert := is.New(t)
	var expected NetworkSettings
	for index, value := range []string{"prod-test.borq.io", "*.prod-test.borq.io", " *.PROD-TEST.BORQ.IO. "} {
		input := networkTestSettings()
		input.Cloudflare.WildcardDomain = value
		normalized, err := NormalizeNetworkSettings(input, "production")
		assert.NoErr(err)
		assert.Equal(normalized.Cloudflare.WildcardDomain, "prod-test.borq.io")
		assert.Equal(input.Cloudflare.WildcardDomain, value)
		if index == 0 {
			expected = normalized
		} else {
			assert.Equal(normalized, expected)
		}
	}
	for _, value := range []string{"**.example.com", "foo.*.example.com", "*example.com", "https://example.com"} {
		input := networkTestSettings()
		input.Cloudflare.WildcardDomain = value
		_, err := NormalizeNetworkSettings(input, "production")
		assert.True(err != nil)
	}
}

func TestNetworkExposureConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, exposure, target, want string
		tunnel                       *CloudflareTunnelSettings
		invalid                      bool
	}{
		{name: "legacy direct", want: "direct"},
		{name: "legacy tunnel", tunnel: &CloudflareTunnelSettings{Enabled: true}, want: "tunnel"},
		{name: "explicit tunnel", exposure: "tunnel", want: "tunnel"},
		{name: "explicit direct", exposure: "direct", target: "PUBLIC.EXAMPLE.NET.", want: "direct"},
		{name: "static ipv4", exposure: "direct", target: "8.8.8.8", want: "direct"},
		{name: "static ipv6", exposure: "direct", target: "2606:4700:4700::1111", want: "direct"},
		{name: "unknown mode", exposure: "other", invalid: true},
		{name: "contradictory direct", exposure: "direct", tunnel: &CloudflareTunnelSettings{Enabled: true}, invalid: true},
		{name: "contradictory tunnel", exposure: "tunnel", tunnel: &CloudflareTunnelSettings{Enabled: false}, invalid: true},
		{name: "tunnel with direct target", exposure: "tunnel", target: "8.8.8.8", invalid: true},
		{name: "private address", target: "10.0.0.1", invalid: true},
		{name: "tailnet address", target: "100.100.0.1", invalid: true},
		{name: "documentation address", target: "192.0.2.1", invalid: true},
		{name: "private ipv6", target: "fd00::1", invalid: true},
		{name: "link local ipv6", target: "fe80::1", invalid: true},
		{name: "documentation ipv6", target: "2001:db8::1", invalid: true},
		{name: "internal hostname", target: "traefik.borq-system.svc.cluster.local", invalid: true},
		{name: "tailnet hostname", target: "borq.test.ts.net", invalid: true},
		{name: "tunnel target", target: "11111111-1111-4111-8111-111111111111.cfargotunnel.com", invalid: true},
		{name: "target url", target: "https://public.example.net", invalid: true},
		{name: "target port", target: "public.example.net:443", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			input := networkTestSettings()
			input.Cloudflare.Exposure, input.Cloudflare.DirectTarget, input.Cloudflare.Tunnel = test.exposure, test.target, test.tunnel
			normalized, err := NormalizeNetworkSettings(input, "production")
			if test.invalid {
				assert.True(err != nil)
				return
			}
			assert.NoErr(err)
			assert.Equal(normalized.Cloudflare.ExposureMode(), test.want)
			assert.Equal(normalized.Cloudflare.Exposure, test.want)
			assert.Equal(input.Cloudflare.Exposure, test.exposure)
			if test.want == "tunnel" {
				assert.True(normalized.Cloudflare.Tunnel != nil && normalized.Cloudflare.Tunnel.Enabled)
			}
			if test.name == "explicit direct" {
				assert.Equal(normalized.Cloudflare.DirectTarget, "public.example.net")
			}
			again, err := NormalizeNetworkSettings(normalized, "production")
			assert.NoErr(err)
			assert.Equal(again, normalized)
		})
	}
}
