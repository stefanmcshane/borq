package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// DnsCleanupInput confines deletion to one installation's registry and known routing targets.
type DnsCleanupInput struct {
	// OwnerID, Namespace and Domain scope the ExternalDNS ownership receipts.
	OwnerID, Namespace, Domain string
	// Targets contains independently observed installation destinations.
	Targets []string
	// Confirm permits deletion only after complete ownership preflight.
	Confirm bool
}

// DnsCleanupRecord identifies a public record selected for retirement, without credentials.
type DnsCleanupRecord struct {
	// ID, Type and Name identify the public record without its private API credential.
	ID, Type, Name string
}

// CleanupExternalDns plans, or removes, records proven by ExternalDNS's TXT registry.
// The data record is removed before its ownership receipt, allowing safe retries.
func (client Client) CleanupExternalDns(ctx context.Context, input DnsCleanupInput) ([]DnsCleanupRecord, error) {
	if !strings.HasPrefix(input.OwnerID, "borq-") || input.Namespace == "" || input.Domain == "" || strings.ContainsAny(input.Namespace, "/\\ \r\n") {
		return nil, errors.New("DNS cleanup requires an installation owner, namespace and domain")
	}
	if err := client.ValidateZone(ctx, input.Domain); err != nil {
		return nil, err
	}
	records, err := client.list(ctx, url.Values{"name.startswith": {"_borq."}})
	if err != nil {
		return nil, err
	}
	type claim struct {
		receipt dnsRecord
		targets []dnsRecord
	}
	var claims []claim
	var result []DnsCleanupRecord
	registryCounts := map[string]int{}
	for _, record := range records {
		registryCounts[canonicalName(record.Name)]++
	}
	for _, receipt := range records {
		if receipt.Type != "TXT" {
			return nil, errors.New("ExternalDNS registry returned a non-TXT receipt")
		}
		content := receipt.Content
		if strings.HasPrefix(content, "\"") {
			content, err = strconv.Unquote(content)
			if err != nil {
				return nil, errors.New("invalid ExternalDNS cleanup receipt")
			}
		}
		labels := map[string]string{}
		for _, label := range strings.Split(content, ",") {
			key, value, ok := strings.Cut(label, "=")
			if ok {
				if _, duplicate := labels[key]; duplicate {
					return nil, errors.New("ambiguous ExternalDNS cleanup receipt")
				}
				labels[key] = value
			}
		}
		if labels["heritage"] != "external-dns" || labels["external-dns/owner"] != input.OwnerID || !strings.HasPrefix(labels["external-dns/resource"], "crd/"+input.Namespace+"/") {
			continue
		}
		if registryCounts[canonicalName(receipt.Name)] != 1 {
			return nil, errors.New("competing ExternalDNS ownership receipts prevent safe cleanup")
		}
		kind, name, ok := strings.Cut(strings.TrimPrefix(canonicalName(receipt.Name), "_borq."), "-")
		name = strings.Replace(name, "_wildcard.", "*.", 1)
		if !ok || (kind != "a" && kind != "aaaa" && kind != "cname") || (name != input.Domain && !strings.HasSuffix(name, "."+input.Domain)) || !validID(receipt.ID) {
			return nil, errors.New("owned ExternalDNS cleanup receipt has an invalid name or identity")
		}
		targets, err := client.list(ctx, url.Values{"name": {name}, "type": {strings.ToUpper(kind)}})
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			if !validID(target.ID) || canonicalName(target.Name) != name || strings.ToLower(target.Type) != kind || !slices.ContainsFunc(input.Targets, func(expected string) bool { return canonicalName(expected) == canonicalName(target.Content) }) {
				return nil, fmt.Errorf("DNS target for %s is no longer owned by this installation", name)
			}
			result = append(result, DnsCleanupRecord{ID: target.ID, Type: target.Type, Name: target.Name})
		}
		result = append(result, DnsCleanupRecord{ID: receipt.ID, Type: "TXT", Name: receipt.Name})
		claims = append(claims, claim{receipt: receipt, targets: targets})
	}
	if !input.Confirm {
		return result, nil
	}
	// Finish all ownership and target checks before the first provider mutation.
	for _, claim := range claims {
		var current response
		if err := client.exchange(ctx, http.MethodGet, "/dns_records/"+claim.receipt.ID, nil, &current); err != nil {
			return nil, err
		}
		var receipt dnsRecord
		if json.Unmarshal(current.Result, &receipt) != nil || receipt != claim.receipt {
			return nil, errors.New("ExternalDNS ownership changed during cleanup")
		}
		for _, target := range claim.targets {
			if err := client.exchange(ctx, http.MethodGet, "/dns_records/"+target.ID, nil, &current); err != nil {
				return nil, err
			}
			var existing dnsRecord
			if json.Unmarshal(current.Result, &existing) != nil || existing != target {
				return nil, errors.New("DNS target changed during cleanup")
			}
			if err := client.exchange(ctx, http.MethodDelete, "/dns_records/"+target.ID, nil, &current); err != nil {
				return nil, err
			}
		}
		if err := client.exchange(ctx, http.MethodDelete, "/dns_records/"+claim.receipt.ID, nil, &current); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// CleanupTunnel verifies the stored installation identity before retiring its connector.
// A deleted tunnel is a successful retry; tunnel creation is never attempted.
func (client Client) CleanupTunnel(ctx context.Context, tunnel Tunnel, owner, namespace, domain string, confirm bool) error {
	digest := sha256.Sum256([]byte(owner + "/" + namespace + "/" + client.zoneID))
	if tunnel.Name != "borq-"+hex.EncodeToString(digest[:16]) || !validID(tunnel.AccountID) || !validTunnelID(tunnel.ID) {
		return errors.New("stored Cloudflare tunnel does not belong to this installation")
	}
	var result response
	if err := client.exchange(ctx, http.MethodGet, "", nil, &result); err != nil {
		return err
	}
	var zone struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if json.Unmarshal(result.Result, &zone) != nil || zone.ID != client.zoneID || zone.Account.ID != tunnel.AccountID {
		return errors.New("Cloudflare tunnel account does not match its zone")
	}
	base := "/accounts/" + tunnel.AccountID + "/cfd_tunnel/" + tunnel.ID
	if err := client.exchangePath(ctx, http.MethodGet, base, nil, &result); err != nil {
		var status responseError
		if errors.As(err, &status) && status.status == http.StatusNotFound {
			return nil
		}
		return err
	}
	var current tunnelRecord
	if json.Unmarshal(result.Result, &current) != nil || current.ID != tunnel.ID || current.Name != tunnel.Name || current.Source != "cloudflare" {
		return errors.New("Cloudflare tunnel identity changed")
	}
	if current.DeletedAt != nil {
		return nil
	}
	if err := client.exchangePath(ctx, http.MethodGet, base+"/configurations", nil, &result); err != nil {
		return err
	}
	var envelope struct {
		Config json.RawMessage `json:"config"`
	}
	if json.Unmarshal(result.Result, &envelope) != nil {
		return errors.New("Cloudflare tunnel configuration is invalid")
	}
	var configurationFields map[string]json.RawMessage
	if json.Unmarshal(envelope.Config, &configurationFields) != nil {
		return errors.New("Cloudflare tunnel configuration is invalid")
	}
	if raw, exists := configurationFields["warp-routing"]; exists {
		var warp map[string]any
		if json.Unmarshal(raw, &warp) != nil || len(warp) != 1 || warp["enabled"] != false {
			return errors.New("Cloudflare tunnel has private-network routing; preserve it for manual review")
		}
		delete(configurationFields, "warp-routing")
	}
	if raw, exists := configurationFields["originRequest"]; exists {
		var origin map[string]any
		if json.Unmarshal(raw, &origin) != nil || len(origin) != 0 {
			return errors.New("Cloudflare tunnel has custom origin routing; preserve it for manual review")
		}
		delete(configurationFields, "originRequest")
	}
	routing, err := json.Marshal(configurationFields)
	if err != nil {
		return err
	}
	var configuration tunnelConfiguration
	decoder := json.NewDecoder(bytes.NewReader(routing))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&configuration.Config) != nil {
		return errors.New("Cloudflare tunnel has unsupported configuration; preserve it for manual review")
	}
	for _, route := range configuration.Config.Ingress {
		if route.Hostname == "" && route.Service == "http_status:404" {
			continue
		}
		if route.Hostname != "*."+domain || route.Service != "http://borq-traefik.borq-system.svc.cluster.local:80" {
			return errors.New("Cloudflare tunnel contains routes outside this installation")
		}
	}
	if !confirm {
		return nil
	}
	if err := client.exchangePath(ctx, http.MethodDelete, base+"/connections", nil, &result); err != nil {
		return err
	}
	return client.exchangePath(ctx, http.MethodDelete, base, nil, &result)
}
