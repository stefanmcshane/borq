package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestBootstrapProxyAccess owns authentication scope, redirect rejection and
// health probing through the kubeconfig transport, without simulating readiness.
func TestBootstrapProxyAccess(t *testing.T) {
	assert := is.New(t)
	var contacts, redirected atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	prefix := "/api/v1/namespaces/borq-mgmt/services/http:borq:80/proxy"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacts.Add(1)
		assert.Equal(r.Header.Get("Authorization"), "Bearer bootstrap-test-token")
		assert.Equal(r.Header.Get("X-Borq-Admin-Token"), strings.Repeat("a", 64))
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
			return
		}
		assert.Equal(r.URL.Path, prefix+borqHealthPath)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "kubeconfig")
	data := strings.Replace(string(bootstrapTestKubeconfig(t)), "https://127.0.0.1:6443", server.URL+"\n      insecure-skip-tls-verify: true", 1)
	assert.NoErr(os.WriteFile(file, []byte(data), 0600))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: file}, logger.Logger{})
	assert.NoErr(err)
	request, err := connectBootstrapApi(Request{Namespace: "borq-mgmt", bootstrapProxy: true, adminToken: strings.Repeat("a", 64), advertisedApiUrl: "http://node:31833"}, connection)
	assert.NoErr(err)
	assert.Equal(request.advertisedApiUrl, "http://node:31833")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	assert.NoErr(probeBorq(ctx, request.ProbeURL, request.probeClient))
	assert.Equal(contacts.Load(), int64(1))
	response, err := request.probeClient.Get(request.ApiUrl + "/api/redirect")
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusTemporaryRedirect)
	assert.NoErr(response.Body.Close())
	for _, endpoint := range []string{other.URL + prefix + borqHealthPath, server.URL + "/api/v1/secrets", request.ApiUrl + "/api/../../secrets", request.ApiUrl + "/api/%2e%2e/%2e%2e/secrets"} {
		_, err := request.probeClient.Get(endpoint)
		assert.True(err != nil)
	}
	assert.Equal(contacts.Load(), int64(2))
	assert.Equal(redirected.Load(), int64(0))
	direct, err := connectBootstrapApi(Request{ApiUrl: other.URL, ProbeURL: other.URL + borqHealthPath}, connection)
	assert.NoErr(err)
	assert.Equal(direct.ApiUrl, other.URL)
	assert.True(direct.probeClient == nil)
}

// TestPrivateServerRejectsRemoteHttp prevents an explicit kubeconfig from
// downgrading the confidentiality of the separately saved Borq administrator token.
func TestPrivateServerRejectsRemoteHttp(t *testing.T) {
	assert := is.New(t)
	var contacts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { contacts.Add(1) }))
	defer server.Close()
	// 0.0.0.0 can address this local listener but is not a loopback identity.
	endpoint := strings.Replace(server.URL, "127.0.0.1", "0.0.0.0", 1)
	file := filepath.Join(t.TempDir(), "kubeconfig")
	data := strings.Replace(string(bootstrapTestKubeconfig(t)), "https://127.0.0.1:6443", endpoint, 1)
	assert.NoErr(os.WriteFile(file, []byte(data), 0600))
	_, err := ConnectPrivateServer(PrivateServerInput{KubeconfigPath: file, Namespace: "borq-mgmt", AdminToken: strings.Repeat("s", 64)})
	assert.True(err != nil && strings.Contains(err.Error(), "require HTTPS"))
	assert.Equal(contacts.Load(), int64(0))
}
