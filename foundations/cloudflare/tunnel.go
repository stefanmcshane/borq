package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

// Tunnel identifies an account-owned connector without serializing its run token.
type Tunnel struct {
	// ID is the Cloudflare tunnel UUID.
	ID string `json:"id"`
	// AccountID scopes subsequent tunnel API calls.
	AccountID string `json:"accountId"`
	// Name is the stable installation-specific tunnel name.
	Name string `json:"name"`
}

type tunnelRecord struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Source    string  `json:"config_src"`
	DeletedAt *string `json:"deleted_at"`
}

// EnsureTunnel finds or creates this installation's remotely managed tunnel.
// A stored UUID is checked against the derived account and installation name.
// The caller must persist that UUID before requesting its connector token.
func (client Client) EnsureTunnel(ctx context.Context, name, storedID string) (Tunnel, error) {
	if !strings.HasPrefix(name, "borq-") || len(name) > 100 || strings.ContainsAny(name, "/\\?& \t\r\n") {
		return Tunnel{}, errors.New("invalid managed Cloudflare tunnel name")
	}
	var zoneResponse response
	if err := client.exchange(ctx, http.MethodGet, "", nil, &zoneResponse); err != nil {
		return Tunnel{}, err
	}
	var zone struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if json.Unmarshal(zoneResponse.Result, &zone) != nil || zone.ID != client.zoneID || !validID(zone.Account.ID) {
		return Tunnel{}, errors.New("Cloudflare zone has no valid account")
	}
	base := "/accounts/" + zone.Account.ID + "/cfd_tunnel"
	var record tunnelRecord
	if storedID != "" {
		if !validTunnelID(storedID) {
			return Tunnel{}, errors.New("invalid stored Cloudflare tunnel ID")
		}
		var result response
		if err := client.exchangePath(ctx, http.MethodGet, base+"/"+storedID, nil, &result); err != nil {
			return Tunnel{}, err
		}
		if json.Unmarshal(result.Result, &record) != nil {
			return Tunnel{}, errors.New("invalid Cloudflare tunnel response")
		}
	} else {
		var result response
		query := url.Values{"name": {name}, "is_deleted": {"false"}, "per_page": {"100"}}
		if err := client.exchangePath(ctx, http.MethodGet, base+"?"+query.Encode(), nil, &result); err != nil {
			return Tunnel{}, err
		}
		if result.ResultInfo.TotalPages > 1 {
			return Tunnel{}, errors.New("Cloudflare tunnel inventory is ambiguous; multiple result pages returned")
		}
		var records []tunnelRecord
		if json.Unmarshal(result.Result, &records) != nil || records == nil {
			return Tunnel{}, errors.New("invalid Cloudflare tunnel inventory")
		}
		for _, entry := range records {
			if entry.Name == name && entry.DeletedAt == nil {
				if record.ID != "" {
					return Tunnel{}, errors.New("multiple Cloudflare tunnels have this installation name")
				}
				record = entry
			}
		}
		if record.ID == "" {
			if err := client.exchangePath(ctx, http.MethodPost, base, struct {
				Name   string `json:"name"`
				Source string `json:"config_src"`
			}{name, "cloudflare"}, &result); err != nil {
				return Tunnel{}, err
			}
			if json.Unmarshal(result.Result, &record) != nil {
				return Tunnel{}, errors.New("invalid created Cloudflare tunnel")
			}
		}
	}
	if !validTunnelID(record.ID) || record.Name != name || record.Source != "cloudflare" || record.DeletedAt != nil || (storedID != "" && record.ID != storedID) {
		return Tunnel{}, errors.New("Cloudflare tunnel identity or configuration source does not match this installation")
	}
	return Tunnel{ID: record.ID, AccountID: zone.Account.ID, Name: name}, nil
}

type tunnelConfiguration struct {
	Config struct {
		Ingress []tunnelRoute `json:"ingress"`
	} `json:"config"`
}
type tunnelRoute struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// ConfigureTunnel restricts the connector to management hostnames and forwards
// their original Host to Traefik over cluster-local HTTP. Cloudflare terminates
// public TLS and encrypts transport to cloudflared; the final local hop uses HTTP.
func (client Client) ConfigureTunnel(ctx context.Context, tunnel Tunnel, domain string) error {
	if !validID(tunnel.AccountID) || !validTunnelID(tunnel.ID) || len(strings.Split(domain, ".")) < 2 || canonicalName(domain) != domain || strings.ContainsAny(domain, "/* :\t\r\n") {
		return errors.New("invalid Cloudflare tunnel route")
	}
	desired := tunnelConfiguration{}
	desired.Config.Ingress = []tunnelRoute{{Hostname: "*." + domain, Service: "http://borq-traefik.borq-system.svc.cluster.local:80"}, {Service: "http_status:404"}}
	path := "/accounts/" + tunnel.AccountID + "/cfd_tunnel/" + tunnel.ID + "/configurations"
	var result response
	if err := client.exchangePath(ctx, http.MethodGet, path, nil, &result); err != nil {
		return err
	}
	var current tunnelConfiguration
	if json.Unmarshal(result.Result, &current) != nil {
		return errors.New("invalid Cloudflare tunnel configuration")
	}
	// Compare the full config, including unknown rules: this tunnel is dedicated
	// to Borq, so manually added routes must not escape its management boundary.
	var currentObject map[string]any
	if json.Unmarshal(result.Result, &currentObject) != nil {
		return errors.New("invalid Cloudflare tunnel configuration")
	}
	raw, _ := json.Marshal(desired.Config)
	var desiredConfig any
	_ = json.Unmarshal(raw, &desiredConfig)
	if reflect.DeepEqual(currentObject["config"], desiredConfig) {
		return nil
	}
	return client.exchangePath(ctx, http.MethodPut, path, desired, &result)
}

// TunnelToken retrieves a tunnel-specific run credential; API errors never expose it.
func (client Client) TunnelToken(ctx context.Context, tunnel Tunnel) (string, error) {
	if !validID(tunnel.AccountID) || !validTunnelID(tunnel.ID) {
		return "", errors.New("invalid Cloudflare tunnel identity")
	}
	var result response
	if err := client.exchangePath(ctx, http.MethodGet, "/accounts/"+tunnel.AccountID+"/cfd_tunnel/"+tunnel.ID+"/token", nil, &result); err != nil {
		return "", err
	}
	var token string
	if json.Unmarshal(result.Result, &token) != nil || !validToken(token) {
		return "", errors.New("Cloudflare returned an invalid tunnel token")
	}
	return token, nil
}
func validTunnelID(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' && validID(strings.ReplaceAll(value, "-", ""))
}
