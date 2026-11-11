package appserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
)

// TestMcpOAuthClientMetadata guards the fetched document's identity, public
// client authentication, exact callbacks, size, JSON format and redirect policy.
func TestMcpOAuthClientMetadata(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		metadata := oauthClientMetadata{ID: "https://" + request.Host + request.URL.Path, Name: "MCP client", RedirectURIs: []string{"http://127.0.0.1:8000/callback"}, AuthMethod: "none"}
		switch request.URL.Path {
		case "/mismatch":
			metadata.ID += "/other"
		case "/confidential":
			metadata.AuthMethod = "private_key_jwt"
		case "/secret":
			metadata.Secret = json.RawMessage(`""`)
		case "/expiry":
			metadata.SecretExpires = json.RawMessage(`0`)
		case "/unsafe-callback":
			metadata.RedirectURIs = []string{"http://remote.example/callback"}
		case "/fragment":
			metadata.RedirectURIs = []string{"https://remote.example/callback#fragment"}
		case "/missing-name":
			metadata.Name = ""
		case "/oversized":
			_, _ = writer.Write([]byte(strings.Repeat(" ", 65<<10)))
			return
		case "/redirect":
			http.Redirect(writer, request, "/valid", http.StatusFound)
			return
		case "/not-json":
			writer.Header().Set("Content-Type", "text/plain")
		}
		_ = json.NewEncoder(writer).Encode(metadata)
	}))
	defer server.Close()
	client := newOAuthHttpClient()
	client.Transport = server.Client().Transport
	t.Cleanup(client.CloseIdleConnections)
	for _, path := range []string{"/valid", "/mismatch", "/confidential", "/secret", "/expiry", "/unsafe-callback", "/fragment", "/missing-name", "/oversized", "/redirect", "/not-json"} {
		t.Run(path, func(t *testing.T) {
			assert := is.New(t)
			before := hits.Load()
			metadata, err := fetchOAuthClient(t.Context(), client, server.URL+path)
			if path == "/valid" {
				assert.NoErr(err)
				assert.Equal(metadata.Name, "MCP client")
			} else {
				assert.True(err != nil)
			}
			assert.Equal(hits.Load(), before+1)
		})
	}
}

// TestMcpOAuthMetadataNetworkBoundary runs the production transport to prove
// private HTTPS targets never receive a request, including mapped IPv6 targets.
func TestMcpOAuthMetadataNetworkBoundary(t *testing.T) {
	assert := is.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := newOAuthHttpClient()
	// Trust the fixture certificate so TLS rejection cannot mask a missing SSRF guard.
	client.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(client.CloseIdleConnections)
	_, err := fetchOAuthClient(t.Context(), client, server.URL+"/client.json")
	assert.True(err != nil)
	assert.Equal(hits.Load(), int32(0))
	for _, address := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.100.100.100", "198.18.0.1", "192.0.2.1", "0.0.0.1", "224.0.0.1", "fc00::1", "fe80::1", "fec0::1", "64:ff9b::a00:1", "2001:db8::1", "2002:7f00:1::1", "3fff::1"} {
		assert.True(!oauthPublicAddress(netip.MustParseAddr(address)))
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		assert.True(oauthPublicAddress(netip.MustParseAddr(address)))
	}
	for _, id := range []string{"http://client.example/client.json", "https://client.example", "https://user:password@client.example/client.json", "https://client.example/client.json#fragment", "https://client.example/a/../client.json", "https://client.example/a/%2e%2e/client.json"} {
		_, err := fetchOAuthClient(t.Context(), client, id)
		assert.True(err != nil)
	}
}
