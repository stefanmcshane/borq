package appserver

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestOrganizationOidcBrowserFlow exercises saved API settings, real signed provider responses and browser-bound callbacks.
func TestOrganizationOidcBrowserFlow(t *testing.T) {
	assert := is.New(t)
	provider, kubeconfig, exchanges := oidcProviderFixture(t, true)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory, ManagementKubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer server.Close()
	call := func(method, address string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
		var encoded []byte
		if body != nil {
			var err error
			encoded, err = json.Marshal(body)
			assert.NoErr(err)
		}
		request := httptest.NewRequest(method, address, bytes.NewReader(encoded))
		if strings.HasPrefix(request.URL.Path, "/api/v1/") || strings.HasPrefix(request.URL.Path, "/api/auth/oidc/test/") {
			authorizeAdminTestRequest(t, directory, request)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		return response
	}
	base := "https://borq.example.test"
	settingsPath := base + "/api/v1/organizations/team/oidc"
	_, err = server.manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	input := map[string]any{"issuerUrl": provider.URL, "clientId": "borq-client", "clientSecret": "private-client-secret", "borqPublicUrl": base, "revision": 0}
	response := call(http.MethodPut, settingsPath, input, nil)
	assert.Equal(response.Code, http.StatusBadRequest)
	input["adminEmail"] = "owner@example.test"
	response = call(http.MethodPut, settingsPath, input, nil)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), "private-client-secret"))
	var publicFields map[string]json.RawMessage
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &publicFields))
	_, canonicalAddress := publicFields["borqPublicUrl"]
	_, legacyAddress := publicFields["baseUrl"]
	assert.True(canonicalAddress && !legacyAddress)
	legacyResponse := call(http.MethodPut, settingsPath, map[string]any{"adminEmail": "owner@example.test", "issuerUrl": provider.URL, "clientId": "borq-client", "clientSecret": "private-client-secret", "baseUrl": base, "revision": 0}, nil)
	assert.Equal(legacyResponse.Code, http.StatusBadRequest)
	assert.True(strings.Contains(legacyResponse.Body.String(), "OpenAPI schema"))
	var settings appapi.OrganizationOidc
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &settings))
	assert.True(settings.CallbackURL != "")
	input["revision"] = settings.Revision
	input["clientId"], input["clientSecret"] = "different-client", ""
	response = call(http.MethodPut, settingsPath, input, nil)
	assert.Equal(response.Code, http.StatusBadRequest)
	assert.True(strings.Contains(response.Body.String(), "client secret"))
	input["clientId"] = "borq-client"

	start := func() (string, *http.Cookie) {
		response := call(http.MethodPost, base+"/api/auth/oidc/test/team", nil, nil)
		assert.Equal(response.Code, http.StatusSeeOther)
		cookies := response.Result().Cookies()
		assert.Equal(len(cookies), 1)
		cookie := cookies[0]
		assert.True(cookie.HttpOnly && cookie.Secure)
		assert.Equal(cookie.SameSite, http.SameSiteLaxMode)
		assert.Equal(cookie.MaxAge, 300)
		client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		authorization, err := client.Get(response.Header().Get("Location"))
		assert.NoErr(err)
		defer authorization.Body.Close()
		assert.Equal(authorization.StatusCode, http.StatusFound)
		return authorization.Header.Get("Location"), cookie
	}

	for _, test := range []string{"missing cookie", "wrong cookie", "missing state", "duplicate state", "unknown state", "wrong callback", "wrong host", "missing issuer", "wrong issuer", "duplicate issuer", "provider denial"} {
		t.Run(test, func(t *testing.T) {
			assert := is.New(t)
			address, cookie := start()
			callback, err := url.Parse(address)
			assert.NoErr(err)
			query := callback.Query()
			switch test {
			case "missing cookie":
				cookie = nil
			case "wrong cookie":
				cookie.Value = "another-browser"
			case "missing state":
				query.Del("state")
			case "duplicate state":
				query.Add("state", query.Get("state"))
			case "unknown state":
				query.Set("state", strings.Repeat("x", 43))
			case "wrong callback":
				callback.Path = "/api/auth/oidc/callback/other-provider"
			case "wrong host":
				callback.Host = "other-borq.example.test"
			case "missing issuer":
				query.Del("iss")
			case "wrong issuer":
				query.Set("iss", "https://other-provider.example.test")
			case "duplicate issuer":
				query.Add("iss", provider.URL)
			case "provider denial":
				query.Set("error", "private-provider-error")
			}
			callback.RawQuery = query.Encode()
			before := exchanges.Load()
			response := call(http.MethodGet, callback.String(), nil, cookie)
			assert.Equal(response.Code, http.StatusBadRequest)
			assert.Equal(exchanges.Load(), before)
			assert.True(!strings.Contains(response.Body.String(), "private-provider-error"))
		})
	}

	address, cookie := start()
	response = call(http.MethodGet, address, nil, cookie)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "Sign-in test passed"))
	assert.True(strings.Contains(response.Body.String(), "account-123"))
	assert.True(!strings.Contains(response.Body.String(), "private-"))
	assert.True(!strings.Contains(response.Body.String(), "<script>"))
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.Equal(response.Header().Get("Referrer-Policy"), "no-referrer")
	assert.Equal(len(response.Result().Cookies()), 1)
	assert.Equal(response.Result().Cookies()[0].MaxAge, -1)
	assert.Equal(call(http.MethodGet, address, nil, cookie).Code, http.StatusBadRequest)
	response = call(http.MethodGet, settingsPath, nil, nil)
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &settings))
	assert.True(settings.LastTestedAt != "")
	// A settings change while at the provider must not mark the new settings as tested.
	address, cookie = start()
	input["revision"] = settings.Revision
	response = call(http.MethodPut, settingsPath, input, nil)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(call(http.MethodGet, address, nil, cookie).Code, http.StatusConflict)
	response = call(http.MethodGet, settingsPath, nil, nil)
	assert.NoErr(json.Unmarshal(response.Body.Bytes(), &settings))
	assert.Equal(settings.LastTestedAt, "")
	address, cookie = start()
	before := exchanges.Load()
	statuses := make(chan int, 2)
	for range 2 {
		go func() { statuses <- call(http.MethodGet, address, nil, cookie).Code }()
	}
	counts := map[int]int{}
	for range 2 {
		counts[<-statuses]++
	}
	assert.Equal(counts[http.StatusOK], 1)
	assert.Equal(counts[http.StatusBadRequest], 1)
	assert.Equal(exchanges.Load(), before+1)
	// A provider settings test does not grant an API credential or a browser session.
	unauthenticated := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, base+"/api/v1/organizations", nil))
	assert.Equal(unauthenticated.Code, http.StatusUnauthorized)
	request := httptest.NewRequest(http.MethodPost, base+"/api/auth/oidc/test/team", nil)
	request.Header.Set("Origin", "https://attacker.example.test")
	authorizeAdminTestRequest(t, directory, request)
	blocked := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(blocked, request)
	assert.Equal(blocked.Code, http.StatusForbidden)
}

// oidcProviderFixture supplies only the actual OIDC and vault HTTP protocols used by setup.
// It does not emulate Kubernetes controllers or manufacture lifecycle success.
func oidcProviderFixture(t *testing.T, emailVerified bool) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	assert := is.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	assert.NoErr(err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	assert.NoErr(err)
	var mutex sync.Mutex
	documents := map[string]map[string]string{}
	codes := map[string]url.Values{}
	logoutReturns := map[string]string{}
	// HTTP handlers and the test share the exchange count to prove pre-exchange rejection.
	exchanges := &atomic.Int32{}
	mux := http.NewServeMux()
	provider := httptest.NewServer(mux)
	t.Cleanup(provider.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"issuer": provider.URL, "authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token", "jwks_uri": provider.URL + "/keys", "end_session_endpoint": provider.URL + "/end-session", "id_token_signing_alg_values_supported": []string{"RS256"}, "authorization_response_iss_parameter_supported": true})
	})
	mux.HandleFunc("GET /authorize", func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		code := fmt.Sprintf("code-%d", len(codes))
		codes[code] = request.URL.Query()
		callback, err := url.Parse(request.URL.Query().Get("redirect_uri"))
		assert.NoErr(err)
		callback.RawQuery = url.Values{"code": {code}, "state": {request.URL.Query().Get("state")}, "iss": {provider.URL}}.Encode()
		http.Redirect(writer, request, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("GET /keys", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		exchanges.Add(1)
		mutex.Lock()
		defer mutex.Unlock()
		assert.NoErr(request.ParseForm())
		id, secret, ok := request.BasicAuth()
		original := codes[request.Form.Get("code")]
		challenge := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
		if !ok || id != "borq-client" || secret != "private-client-secret" || original == nil || original.Get("code_challenge_method") != "S256" || original.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || original.Get("redirect_uri") != request.Form.Get("redirect_uri") {
			http.Error(writer, "private-token-error", http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": provider.URL, "aud": id, "sub": "account-123", "name": "<script>untrusted</script>", "email": "owner@example.test", "nonce": original.Get("nonce"), "exp": time.Now().Add(time.Minute).Unix()}
		if emailVerified {
			claims["email_verified"] = true
		}
		payload, err := json.Marshal(claims)
		assert.NoErr(err)
		signed, err := signer.Sign(payload)
		assert.NoErr(err)
		raw, err := signed.CompactSerialize()
		assert.NoErr(err)
		callback, err := url.Parse(original.Get("redirect_uri"))
		assert.NoErr(err)
		logoutReturns[raw] = callback.Scheme + "://" + callback.Host + "/login"
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "private-access-token", "token_type": "Bearer", "id_token": raw})
		delete(codes, request.Form.Get("code"))
	})
	mux.HandleFunc("GET /end-session", func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		callback, found := logoutReturns[request.URL.Query().Get("id_token_hint")]
		assert.True(found)
		assert.Equal(request.URL.Query().Get("client_id"), "borq-client")
		assert.Equal(request.URL.Query().Get("post_logout_redirect_uri"), callback)
		http.Redirect(writer, request, callback, http.StatusFound)
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/secrets/borq-openbao-admin"):
			_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": "borq-openbao-admin", "namespace": "borq-mgmt"}, "data": map[string]string{"token": base64.StdEncoding.EncodeToString([]byte("admin"))}})
		case strings.HasSuffix(request.URL.Path, "/sys/seal-status"):
			_, _ = writer.Write([]byte(`{"initialized":true,"sealed":false}`))
		case strings.Contains(request.URL.Path, "/proxy/v1/secret/data/"):
			if request.Method == http.MethodPut {
				var body struct {
					Data struct {
						Values map[string]string `json:"values"`
					} `json:"data"`
				}
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				documents[request.URL.Path] = body.Data.Values
				_, _ = writer.Write([]byte(`{"data":{"version":1}}`))
				return
			}
			values, found := documents[request.URL.Path]
			if !found {
				http.NotFound(writer, request)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"values": values}, "metadata": map[string]int{"version": 1}}})
		default:
			http.NotFound(writer, request)
		}
	})
	config := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(config, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: vault\n  cluster:\n    server: %s\ncontexts:\n- name: vault\n  context:\n    cluster: vault\n    user: vault\ncurrent-context: vault\nusers:\n- name: vault\n  user: {}\n", provider.URL)), 0600))
	return provider, config, exchanges
}
