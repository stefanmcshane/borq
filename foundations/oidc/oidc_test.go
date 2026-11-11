package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	provider "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/matryer/is"
)

// TestOidcCodeFlow verifies real signed tokens and rejects invalid identities at the protocol boundary.
func TestOidcCodeFlow(t *testing.T) {
	assert := is.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	assert.NoErr(err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	assert.NoErr(err)
	claims := map[string]any{}
	providerError := false
	logoutEndpoint, rawIDToken := "", ""
	callbackURL := "https://borq.test/api/auth/oidc/callback"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(writer http.ResponseWriter, request *http.Request) {
		metadata := map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
		if logoutEndpoint != "" {
			metadata["end_session_endpoint"] = logoutEndpoint
		}
		_ = json.NewEncoder(writer).Encode(metadata)
	})
	mux.HandleFunc("GET /keys", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		if providerError {
			http.Error(writer, "private-provider-token", http.StatusBadRequest)
			return
		}
		assert.NoErr(request.ParseForm())
		id, secret, ok := request.BasicAuth()
		assert.True(ok)
		assert.Equal(id, "borq-client")
		assert.Equal(secret, "private-client-secret")
		assert.Equal(request.Form.Get("code_verifier"), strings.Repeat("v", 43))
		assert.Equal(request.Form.Get("redirect_uri"), callbackURL)
		payload, err := json.Marshal(claims)
		assert.NoErr(err)
		signed, err := signer.Sign(payload)
		assert.NoErr(err)
		raw, err := signed.CompactSerialize()
		assert.NoErr(err)
		rawIDToken = raw
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "private-access-token", "token_type": "Bearer", "id_token": raw})
	})
	client, err := New(t.Context(), Config{Issuer: server.URL, ClientID: "borq-client", ClientSecret: "private-client-secret", RedirectURL: "https://borq.test/api/auth/oidc/callback"})
	assert.NoErr(err)
	verifier := strings.Repeat("v", 43)
	location, err := url.Parse(client.AuthorizationURL("browser-state", "test-nonce", verifier))
	assert.NoErr(err)
	challenge := sha256.Sum256([]byte(verifier))
	assert.Equal(location.Query().Get("code_challenge"), base64.RawURLEncoding.EncodeToString(challenge[:]))
	assert.Equal(location.Query().Get("code_challenge_method"), "S256")
	assert.Equal(location.Query().Get("state"), "browser-state")
	assert.Equal(location.Query().Get("nonce"), "test-nonce")
	assert.Equal(location.Query().Get("response_type"), "code")
	for _, test := range []struct {
		name, field string
		value       any
		valid       bool
	}{
		{name: "valid", valid: true},
		{name: "wrong issuer", field: "iss", value: "https://other.test"},
		{name: "wrong audience", field: "aud", value: "another-client"},
		{name: "expired", field: "exp", value: time.Now().Add(-time.Minute).Unix()},
		{name: "wrong nonce", field: "nonce", value: "other-attempt"},
		{name: "missing subject", field: "sub", value: ""},
		{name: "wrong authorized party", field: "azp", value: "another-client"},
		{name: "multiple audiences without authorized party", field: "aud", value: []string{"borq-client", "another-client"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			claims = map[string]any{"iss": server.URL, "aud": "borq-client", "sub": "account-123", "nonce": "test-nonce", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "email": "person@example.test", "email_verified": true}
			if test.field != "" {
				claims[test.field] = test.value
			}
			authentication, err := client.Exchange(t.Context(), "one-time-code", "test-nonce", verifier)
			if !test.valid {
				assert.True(err != nil)
				assert.Equal(authentication, Authentication{})
				return
			}
			assert.NoErr(err)
			identity := authentication.Identity
			assert.Equal(identity.Subject, "account-123")
			assert.Equal(identity.Issuer, server.URL)
			assert.Equal(identity.Email, "person@example.test")
			assert.True(identity.EmailVerified)
			assert.Equal(authentication.LogoutURL, "")
		})
	}
	// Synthetic Microsoft tokens are still signed and verified; only their key
	// discovery uses this local fixture, rather than a live Microsoft tenant.
	microsoftIssuer := "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	for _, test := range []struct {
		name, issuer, email, tenant string
		domainClaim                 any
		standardClaim               any
		verified                    bool
	}{
		{"Microsoft verified domain", microsoftIssuer, "person@example.test", "11111111-2222-3333-4444-555555555555", true, nil, true},
		{"Microsoft missing domain proof", microsoftIssuer, "person@example.test", "11111111-2222-3333-4444-555555555555", nil, nil, false},
		{"Microsoft unverified domain", microsoftIssuer, "person@example.test", "11111111-2222-3333-4444-555555555555", false, nil, false},
		{"Microsoft wrong tenant", microsoftIssuer, "person@example.test", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true, nil, false},
		{"Microsoft explicit email denial", microsoftIssuer, "person@example.test", "11111111-2222-3333-4444-555555555555", true, false, false},
		{"Microsoft no email", microsoftIssuer, "", "11111111-2222-3333-4444-555555555555", true, nil, false},
		{"Microsoft string assertion", microsoftIssuer, "person@example.test", "11111111-2222-3333-4444-555555555555", "true", nil, false},
		{"Microsoft shared authority", "https://login.microsoftonline.com/common/v2.0", "person@example.test", "11111111-2222-3333-4444-555555555555", true, nil, false},
		{"Microsoft lookalike host", "https://login.microsoftonline.com.attacker.test/11111111-2222-3333-4444-555555555555/v2.0", "person@example.test", "11111111-2222-3333-4444-555555555555", true, nil, false},
		{"unrelated issuer claim", server.URL, "person@example.test", "11111111-2222-3333-4444-555555555555", true, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			claims = map[string]any{"iss": test.issuer, "aud": "borq-client", "sub": "account-123", "nonce": "test-nonce", "exp": time.Now().Add(time.Minute).Unix(), "email": test.email, "tid": test.tenant}
			if test.domainClaim != nil {
				claims["xms_edov"] = test.domainClaim
			}
			if test.standardClaim != nil {
				claims["email_verified"] = test.standardClaim
			}
			configured := client
			configured.issuer = test.issuer
			configured.verifier = provider.NewVerifier(test.issuer, provider.NewRemoteKeySet(t.Context(), server.URL+"/keys"), &provider.Config{ClientID: "borq-client"})
			authentication, err := configured.Exchange(t.Context(), "one-time-code", "test-nonce", verifier)
			assert.NoErr(err)
			assert.Equal(authentication.Identity.EmailVerified, test.verified)
		})
	}
	claims = map[string]any{"iss": server.URL, "aud": "borq-client", "sub": "account-123", "nonce": "test-nonce", "exp": time.Now().Add(time.Minute).Unix()}
	logoutEndpoint = server.URL + "/end-session?tenant=fixed&client_id=other&id_token_hint=unverified&post_logout_redirect_uri=https%3A%2F%2Fwrong.test"
	callbackURL = "https://borq.test:8443/custom/organization/callback"
	logoutClient, err := New(t.Context(), Config{Issuer: server.URL, ClientID: "borq-client", ClientSecret: "private-client-secret", RedirectURL: callbackURL})
	assert.NoErr(err)
	authentication, err := logoutClient.Exchange(t.Context(), "one-time-code", "test-nonce", verifier)
	assert.NoErr(err)
	logout, err := url.Parse(authentication.LogoutURL)
	assert.NoErr(err)
	assert.Equal(logout.Scheme+"://"+logout.Host+logout.Path, server.URL+"/end-session")
	assert.Equal(logout.Query().Get("tenant"), "fixed")
	assert.Equal(logout.Query().Get("client_id"), "borq-client")
	assert.Equal(logout.Query().Get("id_token_hint"), rawIDToken)
	assert.Equal(logout.Query().Get("post_logout_redirect_uri"), "https://borq.test:8443/login")
	encoded, err := json.Marshal(authentication)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), rawIDToken))
	assert.True(!strings.Contains(string(encoded), "LogoutURL"))
	assert.True(!strings.Contains(string(encoded), "private-"))
	callbackURL = "https://borq.test/api/auth/oidc/callback"
	providerError = true
	_, err = client.Exchange(t.Context(), "one-time-code", "test-nonce", verifier)
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), "private-"))
	providerError = false
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	assert.NoErr(err)
	signer, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: otherKey}, nil)
	assert.NoErr(err)
	claims = map[string]any{"iss": server.URL, "aud": "borq-client", "sub": "account-123", "nonce": "test-nonce", "exp": time.Now().Add(time.Minute).Unix()}
	_, err = client.Exchange(t.Context(), "one-time-code", "test-nonce", verifier)
	assert.True(err != nil)
}

// TestOidcDiscoveryLogoutEndpoints rejects unsafe provider logout destinations before retaining an ID token hint.
func TestOidcDiscoveryLogoutEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"/relative",
		"http://identity.example.test/logout",
		"javascript:alert(1)",
		"https://user:private-password@identity.example.test/logout",
		"https://identity.example.test/logout#fragment",
		"https://identity.example.test/logout#",
		"https://identity.example.test/logout?fixed=private-value%zz",
		"https://identity.example.test/logout?fixed=private value",
	} {
		t.Run(endpoint, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				issuer := "http://" + request.Host
				_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "end_session_endpoint": endpoint})
			}))
			t.Cleanup(server.Close)
			_, err := New(t.Context(), Config{Issuer: server.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://borq.test/callback"})
			assert.True(err != nil)
			assert.True(!strings.Contains(err.Error(), "private"))
		})
	}
}

// TestOidcResponseIssuer enforces advertised issuer identification and accepts providers using distinct callbacks instead.
func TestOidcResponseIssuer(t *testing.T) {
	for _, supported := range []bool{true, false} {
		assert := is.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			issuer := "http://" + request.Host
			_ = json.NewEncoder(writer).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/auth?fixed=1", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "authorization_response_iss_parameter_supported": supported})
		}))
		client, err := New(t.Context(), Config{Issuer: server.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://borq.test/callback"})
		assert.NoErr(err)
		assert.NoErr(client.CheckResponseIssuer(server.URL))
		assert.True(client.CheckResponseIssuer("https://other.test") != nil)
		assert.Equal(client.CheckResponseIssuer("") != nil, supported)
		server.Close()
	}
}

// TestOidcDiscoveryRejectsMismatch prevents a configured issuer from delegating its identity to another provider.
func TestOidcDiscoveryRejectsMismatch(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": "https://another.test", "authorization_endpoint": "https://another.test/auth", "token_endpoint": "https://another.test/token", "jwks_uri": "https://another.test/keys"})
	}))
	defer server.Close()
	_, err := New(t.Context(), Config{Issuer: server.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://borq.test/api/auth/oidc/callback"})
	assert.True(err != nil)
}

// TestOidcUrls requires TLS in deployed environments and rejects ambiguous URL components.
func TestOidcUrls(t *testing.T) {
	for _, value := range []string{"https://id.example.test/tenant/v2.0", "http://127.0.0.1:9000", "http://[::1]:9000", "http://localhost:7833/api/auth/oidc/callback"} {
		is.New(t).NoErr(ValidateURL(value))
	}
	for _, value := range []string{"", "/relative", "http://id.example.test", "ftp://id.example.test", "https://user:password@id.example.test", "https://id.example.test?x=1", "https://id.example.test#fragment", "https://id.example.test/unsafe path", "https://id.example.test?", "https://id.example.test#"} {
		is.New(t).True(ValidateURL(value) != nil)
	}
}
