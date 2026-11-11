// Package cloudflare validates DNS zones and manages Cloudflare Tunnel through its fixed API endpoint.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Config scopes credentials to one configured Cloudflare zone.
type Config struct {
	// ZoneID is the immutable Cloudflare zone identifier, not a domain name.
	ZoneID string
	// ApiToken must have DNS edit permission for the configured zone.
	ApiToken string `json:"-"`
	// HttpClient supplies an optional transport for explicit TLS or network configuration.
	// Only its transport is reused; deadlines and redirect refusal remain enforced.
	HttpClient *http.Client `json:"-"`
}

// Client owns management zone validation and tunnel provisioning.
type Client struct {
	zoneID     string
	token      string
	httpClient *http.Client
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	TTL     int    `json:"ttl,omitempty"`
}

type response struct {
	Success    bool            `json:"success"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		Count      int `json:"count"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type responseError struct{ status int }

func (err responseError) Error() string {
	return fmt.Sprintf("Cloudflare DNS request failed (HTTP %d)", err.status)
}

// New validates zone credentials and builds a client that never follows redirects.
func New(config Config) (Client, error) {
	if !validID(config.ZoneID) {
		return Client{}, errors.New("a valid Cloudflare zone ID is required")
	}
	if !validToken(config.ApiToken) {
		return Client{}, errors.New("a valid Cloudflare API token is required")
	}
	var transport http.RoundTripper
	if config.HttpClient != nil {
		transport = config.HttpClient.Transport
	}
	return Client{zoneID: config.ZoneID, token: config.ApiToken, httpClient: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// ValidateZone verifies zone ownership and DNS read permission before provisioning begins.
// A configured domain may be the zone apex or a subdomain within that zone.
func (client Client) ValidateZone(ctx context.Context, domain string) error {
	domain = canonicalName(domain)
	if len(validation.IsDNS1123Subdomain(domain)) != 0 {
		return errors.New("a valid Cloudflare domain is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result response
	if err := client.exchange(ctx, http.MethodGet, "", nil, &result); err != nil {
		return err
	}
	var zone struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result.Result, &zone); err != nil || zone.ID != client.zoneID {
		return errors.New("Cloudflare returned an unexpected zone")
	}
	name := canonicalName(zone.Name)
	if len(validation.IsDNS1123Subdomain(name)) != 0 || domain != name && !strings.HasSuffix(domain, "."+name) {
		return errors.New("the configured domain is outside the Cloudflare zone")
	}
	_, err := client.list(ctx, url.Values{"name": {"_acme-challenge." + domain}})
	return err
}

func (client Client) list(ctx context.Context, query url.Values) ([]dnsRecord, error) {
	var records []dnsRecord
	if query.Get("type") == "" {
		query.Set("type", "TXT")
	}
	query.Set("per_page", "100")
	for page := 1; page <= 100; page++ {
		query.Set("page", strconv.Itoa(page))
		var result response
		if err := client.exchange(ctx, http.MethodGet, "/dns_records?"+query.Encode(), nil, &result); err != nil {
			return nil, err
		}
		var entries []dnsRecord
		if err := json.Unmarshal(result.Result, &entries); err != nil || entries == nil {
			return nil, errors.New("Cloudflare returned an invalid DNS record list")
		}
		info := result.ResultInfo
		if info.Page != page || info.PerPage < 1 || info.Count != len(entries) || info.TotalPages < page && !(page == 1 && len(entries) == 0 && info.TotalPages == 0) {
			return nil, errors.New("Cloudflare returned invalid DNS pagination metadata")
		}
		records = append(records, entries...)
		if page >= info.TotalPages {
			return records, nil
		}
	}
	return nil, errors.New("Cloudflare DNS record listing exceeded the supported page limit")
}

func (client Client) exchange(ctx context.Context, method, suffix string, input any, output *response) error {
	return client.exchangePath(ctx, method, "/zones/"+client.zoneID+suffix, input, output)
}

func (client Client) exchangePath(ctx context.Context, method, path string, input any, output *response) error {
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return errors.New("Cloudflare DNS request could not be encoded")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("Cloudflare DNS request could not be prepared")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	result, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Cloudflare DNS request could not be completed")
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return responseError{status: result.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, output) != nil {
		return errors.New("Cloudflare returned an invalid or oversized DNS response")
	}
	if !output.Success {
		return errors.New("Cloudflare rejected the DNS request")
	}
	return nil
}

func canonicalName(value string) string { return strings.TrimSuffix(strings.ToLower(value), ".") }
func validID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && value == strings.ToLower(value)
}
func validToken(value string) bool {
	if len(value) == 0 || len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if character <= 32 || character >= 127 {
			return false
		}
	}
	return true
}
func statusIs(err error, status int) bool {
	var response responseError
	return errors.As(err, &response) && response.status == status
}
