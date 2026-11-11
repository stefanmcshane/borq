package platform

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// NetworkSettings contains public provider configuration; credentials remain Kubernetes Secrets.
type NetworkSettings struct {
	// Tailscale is optional: nil omits private networking, while an empty object enables its defaults.
	Tailscale *TailscaleSettings `json:"tailscale,omitempty"`
	// Cloudflare is optional: nil omits management public DNS and ACME.
	Cloudflare *CloudflareSettings `json:"cloudflare,omitempty"`
	// ManagementURL is the observed admin origin, supplied at runtime rather than exported as configuration.
	ManagementURL string `json:"managementUrl,omitempty"`
}

// TailscaleSettings configures private access without requiring public DNS.
type TailscaleSettings struct {
	// ClientID is a write-only OAuth input staged privately by management apply.
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is a write-only OAuth input staged privately by management apply.
	ClientSecret string `json:"clientSecret,omitempty"`
	// CredentialsRef reuses a management-owned staged Kubernetes Secret.
	CredentialsRef string `json:"credentialsRef,omitempty"`
	// ManagementHostname requests a short device name; the assigned tailnet domain is observed later.
	ManagementHostname string `json:"managementHostname,omitempty"`
	// ManagementOperatorTag identifies the management-only operator credential scope.
	ManagementOperatorTag string `json:"managementOperatorTag,omitempty"`
	// OperatorTag identifies target operator devices in tailnet policy.
	OperatorTag string `json:"operatorTag,omitempty"`
	// AdminTag restricts management API access independently of cluster proxies.
	AdminTag string `json:"adminTag,omitempty"`
	// BrokerTag is deprecated and ignored; accepted only when reading older bootstrap files.
	BrokerTag string `json:"brokerTag,omitempty"`
	// ProxyTag identifies workload proxy devices in tailnet policy.
	ProxyTag string `json:"proxyTag,omitempty"`
}

// CloudflareSettings configures management-owned public DNS and ACME issuance.
type CloudflareSettings struct {
	// ApiToken is a write-only provider credential staged privately by management apply.
	ApiToken string `json:"apiToken,omitempty"`
	// CredentialsRef reuses a management-owned staged Kubernetes Secret.
	CredentialsRef string `json:"credentialsRef,omitempty"`
	// Exposure selects tunnel or direct management ingress. Omission reads the legacy tunnel.enabled setting.
	Exposure string `json:"exposure,omitempty"`
	// DirectTarget optionally supplies a public static IP or load-balancer hostname; empty observes the Gateway address.
	DirectTarget string `json:"directTarget,omitempty"`
	// Tunnel enables an outbound management ingress connector. Nil preserves direct ingress.
	Tunnel *CloudflareTunnelSettings `json:"tunnel,omitempty"`
	// WildcardDomain restricts public routing, DNS records and certificate challenges.
	// NormalizeNetworkSettings accepts an optional *. prefix and stores the bare suffix.
	WildcardDomain string `json:"wildcardDomain,omitempty"`
	// ZoneID restricts management DNS writes to the selected Cloudflare zone.
	ZoneID string `json:"zoneId,omitempty"`
	// AcmeEmail registers the certificate issuer with Let's Encrypt.
	AcmeEmail string `json:"acmeEmail,omitempty"`
	// WebhookImage is deprecated and ignored; native Cloudflare DNS01 needs no Borq image.
	WebhookImage string `json:"webhookImage,omitempty"`
}

// CloudflareTunnelSettings configures management ingress through Cloudflare Tunnel.
// Credentials reuse the management Cloudflare API token and never enter this object.
type CloudflareTunnelSettings struct {
	// Enabled opts in to tunnel provisioning; false stops its connector without deleting the remote tunnel.
	Enabled bool `json:"enabled"`
	// Image overrides the pinned official cloudflared image.
	Image string `json:"image,omitempty"`
}

// ExposureMode resolves explicit routing intent while keeping older bootstrap files readable.
func (settings CloudflareSettings) ExposureMode() string {
	if settings.Exposure != "" {
		return settings.Exposure
	}
	if settings.Tunnel != nil && settings.Tunnel.Enabled {
		return "tunnel"
	}
	return "direct"
}

// NetworkState reports controller-assigned addresses without credentials.
type NetworkState struct {
	// ManagementURL is observed from the tailnet Ingress, never synthesized.
	ManagementURL string `json:"managementUrl,omitempty"`
}

// NormalizeNetworkSettings copies present providers before filling defaults and validating them.
func NormalizeNetworkSettings(settings NetworkSettings, clusterName string) (NetworkSettings, error) {
	if settings.Tailscale != nil {
		tailscale := *settings.Tailscale
		settings.Tailscale = &tailscale
		if tailscale.ManagementHostname == "" {
			tailscale.ManagementHostname = "borq-" + clusterName
		}
		if tailscale.ManagementOperatorTag == "" {
			tailscale.ManagementOperatorTag = "tag:borq-management-operator"
		}
		if tailscale.OperatorTag == "" {
			tailscale.OperatorTag = "tag:borq-operator"
		}
		if tailscale.AdminTag == "" {
			tailscale.AdminTag = "tag:borq-admin"
		}
		tailscale.BrokerTag = ""
		if tailscale.ProxyTag == "" {
			tailscale.ProxyTag = "tag:borq"
		}
	}
	if settings.Cloudflare != nil {
		if clusterName != "" && len(validation.IsDNS1123Label(clusterName)) != 0 {
			return NetworkSettings{}, fmt.Errorf("management cluster name must be a DNS label for public routing")
		}
		cloudflare := *settings.Cloudflare
		settings.Cloudflare = &cloudflare
		cloudflare.WebhookImage = ""
		cloudflare.Exposure = cloudflare.ExposureMode()
		cloudflare.DirectTarget = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cloudflare.DirectTarget)), ".")
		if cloudflare.Exposure == "tunnel" && cloudflare.Tunnel == nil {
			cloudflare.Tunnel = &CloudflareTunnelSettings{Enabled: true}
		}
		cloudflare.ZoneID = strings.ToLower(strings.TrimSpace(cloudflare.ZoneID))
		if cloudflare.Tunnel != nil {
			tunnel := *cloudflare.Tunnel
			cloudflare.Tunnel = &tunnel
			if tunnel.Image == "" {
				tunnel.Image = "cloudflare/cloudflared:2026.10.7@sha256:4955524aeaa379e154023d5a027c250f7b35041a2e7e950cf13f144bc9b9c5ba"
			}
		}
		cloudflare.WildcardDomain = strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cloudflare.WildcardDomain)), "."), "*.")
	}
	if err := ValidateNetworkSettings(settings); err != nil {
		return NetworkSettings{}, err
	}
	return settings, nil
}

// ValidateNetworkSettings rejects unsafe origins, zones and device names before installation.
func ValidateNetworkSettings(settings NetworkSettings) error {
	if settings.Cloudflare != nil {
		cloudflare := *settings.Cloudflare
		cloudflare.WildcardDomain = strings.TrimPrefix(cloudflare.WildcardDomain, "*.")
		mode := cloudflare.ExposureMode()
		if mode != "tunnel" && mode != "direct" {
			return fmt.Errorf("network Cloudflare exposure must be tunnel or direct")
		}
		if cloudflare.Tunnel != nil && (mode == "tunnel") != cloudflare.Tunnel.Enabled {
			return fmt.Errorf("network Cloudflare exposure conflicts with tunnel.enabled")
		}
		if cloudflare.DirectTarget != "" {
			if mode != "direct" {
				return fmt.Errorf("network Cloudflare directTarget requires direct exposure")
			}
			if err := validateCloudflareDirectTarget(cloudflare.DirectTarget); err != nil {
				return err
			}
		}
		if cloudflare.Tunnel != nil && cloudflare.Tunnel.Enabled && (strings.TrimSpace(cloudflare.Tunnel.Image) == "" || strings.ContainsAny(cloudflare.Tunnel.Image, " \t\r\n")) {
			return fmt.Errorf("network Cloudflare tunnel image is required")
		}
		if len(validation.IsDNS1123Subdomain(cloudflare.WildcardDomain)) != 0 || !strings.Contains(cloudflare.WildcardDomain, ".") || net.ParseIP(cloudflare.WildcardDomain) != nil || strings.HasSuffix(cloudflare.WildcardDomain, ".localhost") {
			return fmt.Errorf("network domain must be a public DNS zone name")
		}
		if !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(cloudflare.ZoneID) {
			return fmt.Errorf("network Cloudflare zone ID must contain 32 hexadecimal characters")
		}
		address, err := mail.ParseAddress(cloudflare.AcmeEmail)
		if err != nil || address.Address != cloudflare.AcmeEmail || address.Name != "" {
			return fmt.Errorf("network ACME email must be a plain email address")
		}
	}
	if settings.Tailscale != nil {
		tailscale := *settings.Tailscale
		if tailscale.ManagementHostname != "" && (len(validation.IsDNS1123Label(tailscale.ManagementHostname)) != 0 || len(tailscale.ManagementHostname) > 57) {
			return fmt.Errorf("network management hostname must be a DNS label")
		}
		for _, tag := range []string{tailscale.OperatorTag, tailscale.ProxyTag, tailscale.AdminTag, tailscale.ManagementOperatorTag} {
			if tag != "" && !regexp.MustCompile(`^tag:[a-z][a-z0-9-]{0,62}$`).MatchString(tag) {
				return fmt.Errorf("network Tailscale tags must use tag:name")
			}
		}
		for _, trusted := range []string{tailscale.AdminTag, tailscale.ManagementOperatorTag} {
			if trusted != "" && (trusted == tailscale.OperatorTag || trusted == tailscale.ProxyTag) {
				return fmt.Errorf("management Tailscale tags must differ from target operator and proxy tags")
			}
		}
		if tailscale.AdminTag != "" && tailscale.AdminTag == tailscale.ManagementOperatorTag {
			return fmt.Errorf("admin Tailscale tag must differ from the operator tag")
		}
	}
	for _, origin := range []string{settings.ManagementURL} {
		if origin == "" {
			continue
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Port() != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasSuffix(parsed.Hostname(), ".ts.net") || len(validation.IsDNS1123Subdomain(parsed.Hostname())) != 0 {
			return fmt.Errorf("network address must be an assigned tailnet HTTPS origin")
		}
	}
	return nil
}

func validateCloudflareDirectTarget(target string) error {
	invalid := fmt.Errorf("network Cloudflare directTarget must be a public IP or load-balancer hostname, without a scheme, port or tunnel target")
	if address, err := netip.ParseAddr(target); err == nil {
		address = address.Unmap()
		if !address.IsGlobalUnicast() || address.IsPrivate() {
			return invalid
		}
		for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001:2::/48"} {
			if netip.MustParsePrefix(cidr).Contains(address) {
				return invalid
			}
		}
		if !address.Is4() && !netip.MustParsePrefix("2000::/3").Contains(address) {
			return invalid
		}
		return nil
	}
	if len(validation.IsDNS1123Subdomain(target)) != 0 || !strings.Contains(target, ".") {
		return invalid
	}
	for _, suffix := range []string{".local", ".internal", ".ts.net", ".localhost", ".cfargotunnel.com"} {
		if strings.HasSuffix(target, suffix) {
			return invalid
		}
	}
	return nil
}
