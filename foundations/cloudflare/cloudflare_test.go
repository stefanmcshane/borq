package cloudflare

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestCloudflareCredentialValidation rejects malformed IDs and header injection.
func TestCloudflareCredentialValidation(t *testing.T) {
	assert := is.New(t)
	_, err := New(Config{ZoneID: "../zones/other", ApiToken: "token"})
	assert.True(err != nil)
	_, err = New(Config{ZoneID: strings.Repeat("b", 32), ApiToken: "private\r\nheader"})
	assert.True(err != nil && !strings.Contains(err.Error(), "private"))
}

// TestZoneValidation checks real provider identity and both read permissions before any DNS mutation.
func TestZoneValidation(t *testing.T) {
	for _, test := range []struct {
		name, domain, zone string
		denyDNS            bool
		message            string
		requests           int
	}{
		{name: "zone apex", domain: "Example.com.", zone: "example.com", requests: 2},
		{name: "domain inside zone", domain: "apps.example.com", zone: "example.com", requests: 2},
		{name: "sibling zone rejected", domain: "notexample.com", zone: "example.com", message: "outside", requests: 1},
		{name: "child zone cannot authorize parent", domain: "example.com", zone: "child.example.com", message: "outside", requests: 1},
		{name: "DNS permission required", domain: "apps.example.com", zone: "example.com", denyDNS: true, message: "HTTP 403", requests: 2},
		{name: "invalid domain before request", domain: "../example.com", zone: "example.com", message: "valid Cloudflare domain", requests: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				assert.Equal(request.Method, http.MethodGet)
				base := "/client/v4/zones/" + strings.Repeat("b", 32)
				if request.URL.Path == base {
					assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": map[string]string{"id": strings.Repeat("b", 32), "name": test.zone}}))
					return
				}
				assert.Equal(request.URL.Path, base+"/dns_records")
				if test.denyDNS {
					writer.WriteHeader(http.StatusForbidden)
					_, _ = writer.Write([]byte("private-provider-diagnostic"))
					return
				}
				assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": []dnsRecord{}, "result_info": map[string]int{"page": 1, "per_page": 100, "count": 0, "total_pages": 0}}))
			}))
			defer server.Close()
			err := testClient(t, server.URL).ValidateZone(t.Context(), test.domain)
			if test.message == "" {
				assert.NoErr(err)
			} else {
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), test.message))
				assert.True(!strings.Contains(err.Error(), "private-provider-diagnostic"))
			}
			assert.Equal(requests, test.requests)
		})
	}
}

type localTransport struct{ destination *url.URL }

func (transport localTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.cloudflare.com" {
		return nil, errors.New("unexpected Cloudflare origin")
	}
	copy := request.Clone(request.Context())
	address := *request.URL
	address.Scheme = transport.destination.Scheme
	address.Host = transport.destination.Host
	copy.URL = &address
	return http.DefaultTransport.RoundTrip(copy)
}
func testClient(t *testing.T, address string) Client {
	t.Helper()
	assert := is.New(t)
	destination, err := url.Parse(address)
	assert.NoErr(err)
	client, err := New(Config{ZoneID: strings.Repeat("b", 32), ApiToken: "test-private-token", HttpClient: &http.Client{Transport: localTransport{destination: destination}}})
	assert.NoErr(err)
	return client
}
