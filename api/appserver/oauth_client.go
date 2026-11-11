package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

type oauthClientMetadata struct {
	ID            string          `json:"client_id"`
	Name          string          `json:"client_name"`
	RedirectURIs  []string        `json:"redirect_uris"`
	AuthMethod    string          `json:"token_endpoint_auth_method"`
	GrantTypes    []string        `json:"grant_types"`
	ResponseTypes []string        `json:"response_types"`
	Secret        json.RawMessage `json:"client_secret,omitempty"`
	SecretExpires json.RawMessage `json:"client_secret_expires_at,omitempty"`
}

func newOAuthHttpClient() http.Client {
	return http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// The transport shares its connection pool and TLS state across requests.
		Transport: &http.Transport{
			DialContext:            oauthPublicDial,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  5 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
			MaxConnsPerHost:        4,
			IdleConnTimeout:        30 * time.Second,
		},
	}
}

func oauthPublicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid client metadata host")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("could not resolve client metadata host")
	}
	for _, address := range addresses {
		if !oauthPublicAddress(address) {
			return nil, errors.New("client metadata must use public addresses")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	// Dial the checked addresses directly so DNS rebinding cannot change the target.
	for _, address := range addresses {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return connection, nil
		}
	}
	return nil, errors.New("could not connect to client metadata host")
}

func oauthPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, reserved := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		if netip.MustParsePrefix(reserved).Contains(address) {
			return false
		}
	}
	return true
}

func oauthClientURL(value string) bool {
	address, err := url.Parse(value)
	if err != nil || len(value) > 2048 || address.Scheme != "https" || address.Hostname() == "" ||
		address.User != nil || address.Opaque != "" || address.Fragment != "" || strings.Contains(value, "#") || address.Path == "" {
		return false
	}
	for _, part := range strings.Split(address.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func oauthRedirectURL(value string) bool {
	address, err := url.Parse(value)
	if err != nil || len(value) > 2048 || address.Hostname() == "" || address.User != nil || address.Opaque != "" || strings.Contains(value, "#") {
		return false
	}
	if address.Scheme == "https" {
		return true
	}
	return address.Scheme == "http" && (address.Hostname() == "localhost" || net.ParseIP(address.Hostname()).IsLoopback())
}

func fetchOAuthClient(ctx context.Context, client http.Client, id string) (oauthClientMetadata, error) {
	if !oauthClientURL(id) {
		return oauthClientMetadata{}, errors.New("client_id must be an HTTPS metadata document URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return oauthClientMetadata{}, errors.New("invalid client metadata URL")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return oauthClientMetadata{}, errors.New("could not fetch client metadata")
	}
	defer response.Body.Close()
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || response.StatusCode != http.StatusOK ||
		(contentType != "application/json" && !(strings.HasPrefix(contentType, "application/") && strings.HasSuffix(contentType, "+json"))) {
		return oauthClientMetadata{}, errors.New("client metadata must be a successful JSON response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return oauthClientMetadata{}, errors.New("client metadata exceeds the response limit")
	}
	var metadata oauthClientMetadata
	if json.Unmarshal(body, &metadata) != nil || metadata.ID != id || strings.TrimSpace(metadata.Name) == "" || len(metadata.Name) > 200 ||
		len(metadata.RedirectURIs) == 0 || len(metadata.RedirectURIs) > 64 || len(metadata.Secret) > 0 || len(metadata.SecretExpires) > 0 ||
		(metadata.AuthMethod != "" && metadata.AuthMethod != "none") ||
		(len(metadata.GrantTypes) > 0 && !slices.Contains(metadata.GrantTypes, "authorization_code")) ||
		(len(metadata.ResponseTypes) > 0 && !slices.Contains(metadata.ResponseTypes, "code")) {
		return oauthClientMetadata{}, errors.New("invalid or unsupported client metadata")
	}
	for _, redirect := range metadata.RedirectURIs {
		if !oauthRedirectURL(redirect) {
			return oauthClientMetadata{}, errors.New("invalid client redirect URI")
		}
	}
	return metadata, nil
}
