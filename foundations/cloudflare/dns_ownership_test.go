package cloudflare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// Replacing the cluster changes ExternalDNS's owner while the old public TXT
// records survive. Kubernetes publication alone cannot detect that conflict.
func TestExternalDnsOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		records []dnsRecord
		kind    string
		message string
	}{
		{name: "previous installation", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-production.example.com", Content: `"heritage=external-dns,external-dns/owner=borq-old-cluster,external-dns/resource=crd/borq-mgmt/old"`},
		}, message: `DNS hostname production.example.com has ExternalDNS owner "borq-old-cluster"; expected "borq-current-cluster"; DNS updates are blocked until ownership is explicitly transferred to this installation`},
		{name: "current installation", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-production.example.com", Content: `"heritage=external-dns,external-dns/owner=borq-current-cluster"`},
		}},
		{name: "unquoted IPv4 marker", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.a-production.example.com", Content: "heritage=external-dns,external-dns/owner=other"},
		}, kind: "A", message: `owner "other"`},
		{name: "IPv6 marker", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.aaaa-production.example.com", Content: "heritage=external-dns,external-dns/owner=other"},
		}, kind: "AAAA", message: `owner "other"`},
		{name: "wildcard marker", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-_wildcard.apps.example.com", Content: "heritage=external-dns,external-dns/owner=other"},
		}, message: "DNS hostname *.apps.example.com"},
		{name: "missing owner", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-production.example.com", Content: "heritage=external-dns"},
		}, message: `owner ""`},
		{name: "unrelated records", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-unclaimed.example.com", Content: "heritage=external-dns,external-dns/owner=other"},
			{Type: "TXT", Name: "_borq.a-production.example.com", Content: "heritage=external-dns,external-dns/owner=previous-record-type"},
			{Type: "TXT", Name: "_borq.cname-production.example.com", Content: "unrelated-verification-value"},
		}},
		{name: "initial publication", records: []dnsRecord{}},
		{name: "malformed ownership", records: []dnsRecord{
			{Type: "TXT", Name: "_borq.cname-production.example.com", Content: `"private-malformed-payload`},
		}, message: "invalid ExternalDNS ownership record"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/client/v4/zones/"+strings.Repeat("b", 32)+"/dns_records")
				assert.Equal(request.URL.Query().Get("type"), "TXT")
				assert.Equal(request.URL.Query().Get("name.startswith"), "_borq.")
				assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": test.records, "result_info": map[string]int{"page": 1, "per_page": 100, "count": len(test.records), "total_pages": 1}}))
			}))
			defer server.Close()
			kind := test.kind
			if kind == "" {
				kind = "CNAME"
			}
			err := testClient(t, server.URL).CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "borq-current-cluster", Records: []DnsOwnershipRecord{{Hostname: "production.example.com", RecordType: kind}, {Hostname: "*.apps.example.com", RecordType: "CNAME"}}})
			if test.message == "" {
				assert.NoErr(err)
				return
			}
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			assert.True(!strings.Contains(err.Error(), "test-private-token"))
			assert.True(!strings.Contains(err.Error(), "private-malformed-payload"))
		})
	}
}

func TestExternalDnsOwnershipPagination(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		requests++
		assert.Equal(request.URL.Query().Get("page"), []string{"1", "2"}[requests-1])
		records := []dnsRecord{{Type: "TXT", Name: "_borq.cname-unrelated.example.com", Content: "heritage=external-dns,external-dns/owner=other"}}
		if requests == 2 {
			records = []dnsRecord{
				{Type: "TXT", Name: "_borq.cname-production.example.com", Content: "heritage=external-dns,external-dns/owner=old-cluster"},
				{Type: "TXT", Name: "_borq.cname-id.example.com", Content: "heritage=external-dns,external-dns/owner=old-cluster"},
			}
		}
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": records, "result_info": map[string]int{"page": requests, "per_page": 100, "count": len(records), "total_pages": 2}}))
	}))
	defer server.Close()
	err := testClient(t, server.URL).CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "current", Records: []DnsOwnershipRecord{{Hostname: "production.example.com", RecordType: "CNAME"}, {Hostname: "id.example.com", RecordType: "CNAME"}}})
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "DNS hostname production.example.com"))
	assert.True(strings.Contains(err.Error(), "DNS hostname id.example.com"))
	assert.True(!strings.Contains(err.Error(), "unrelated.example.com"))
	assert.Equal(requests, 2)
}

func TestExternalDnsOwnershipReadFailure(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte("private-provider-diagnostic"))
	}))
	defer server.Close()
	err := testClient(t, server.URL).CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "current", Records: []DnsOwnershipRecord{{Hostname: "production.example.com", RecordType: "CNAME"}}})
	assert.True(err != nil)
	assert.Equal(err.Error(), "read ExternalDNS ownership records: Cloudflare DNS request failed (HTTP 403)")
}

func TestExternalDnsOwnershipInput(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("invalid or empty input must not contact Cloudflare")
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	assert.NoErr(client.CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "current"}))
	err := client.CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{Records: []DnsOwnershipRecord{{Hostname: "production.example.com", RecordType: "CNAME"}}})
	assert.True(err != nil && strings.Contains(err.Error(), "owner ID"))
	err = client.CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "current", Records: []DnsOwnershipRecord{{Hostname: "https://production.example.com", RecordType: "CNAME"}}})
	assert.True(err != nil && strings.Contains(err.Error(), "valid DNS hostnames"))
	err = client.CheckExternalDnsOwnership(t.Context(), DnsOwnershipInput{OwnerID: "current", Records: []DnsOwnershipRecord{{Hostname: "production.example.com", RecordType: "TXT"}}})
	assert.True(err != nil && strings.Contains(err.Error(), "A, AAAA or CNAME"))
}
