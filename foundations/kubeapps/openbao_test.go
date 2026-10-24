package kubeapps

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// TestOpenBaoStoreUsesTargetClusterProxy verifies the store's credentials are read
// from the target cluster and service requests retain Kubernetes authentication.
func TestOpenBaoStoreUsesTargetClusterProxy(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(request.URL.Path, "/api/v1/namespaces/borq-system/services/http:openbao:8200/proxy/v1/secret/data/test")
		assert.Equal(request.Header.Get("Authorization"), "Bearer cluster-credential")
		assert.Equal(request.Header.Get("X-Vault-Token"), "openbao-credential")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, fakeClient := testClient(openBaoStoreManifest(), openBaoTokenManifest())
	client.config = rest.Config{Host: server.URL, BearerToken: "cluster-credential", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	access, err := client.OpenBaoStore(context.Background(), "borq-openbao")
	assert.NoErr(err)
	assert.Equal(access.Mount, "secret")
	assert.Equal(access.Token, "openbao-credential")
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, access.Address+"/v1/secret/data/test", nil)
	assert.NoErr(err)
	request.Header.Set("X-Vault-Token", access.Token)
	response, err := access.HttpClient.Do(request)
	assert.NoErr(err)
	assert.NoErr(response.Body.Close())
	assert.Equal(requests, 1)
	for _, action := range fakeClient.Actions() {
		assert.Equal(action.GetVerb(), "get")
	}
}

// TestOpenBaoStoreRejectsRedirects ensures a proxy response cannot forward the
// OpenBao credential to an unrelated HTTP endpoint.
func TestOpenBaoStoreRejectsRedirects(t *testing.T) {
	assert := is.New(t)
	forwarded := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, _ := testClient(openBaoStoreManifest(), openBaoTokenManifest())
	client.config = rest.Config{Host: server.URL}
	access, err := client.OpenBaoStore(context.Background(), "borq-openbao")
	assert.NoErr(err)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, access.Address+"/v1/secret/data/test", nil)
	assert.NoErr(err)
	request.Header.Set("X-Vault-Token", access.Token)
	response, err := access.HttpClient.Do(request)
	assert.True(err != nil)
	if response != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		assert.NoErr(response.Body.Close())
	}
	assert.True(!forwarded)
}

// TestOpenBaoStoreRejectsUnusableStores catches incorrect engines, service
// locations and token references without printing credentials in diagnostics.
func TestOpenBaoStoreRejectsUnusableStores(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "different namespace", change: func(provider map[string]any) { provider["server"] = "http://openbao.default.svc:8200" }},
		{name: "wrong port", change: func(provider map[string]any) { provider["server"] = "http://openbao.borq-system.svc:80" }},
		{name: "server credentials", change: func(provider map[string]any) { provider["server"] = "http://user:pass@openbao.borq-system.svc:8200" }},
		{name: "server path", change: func(provider map[string]any) { provider["server"] = "http://openbao.borq-system.svc:8200/path" }},
		{name: "KV v1", change: func(provider map[string]any) { provider["version"] = "v1" }},
		{name: "missing mount", change: func(provider map[string]any) { delete(provider, "path") }},
		{name: "relative mount", change: func(provider map[string]any) { provider["path"] = "../secret" }},
		{name: "missing token key", change: func(provider map[string]any) {
			delete(valueAt(provider, "auth", "tokenSecretRef").(map[string]any), "key")
		}},
		{name: "foreign token namespace", change: func(provider map[string]any) {
			valueAt(provider, "auth", "tokenSecretRef").(map[string]any)["namespace"] = "default"
		}},
		{name: "enterprise namespace", change: func(provider map[string]any) { provider["namespace"] = "tenant" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			store := openBaoStoreManifest()
			provider := valueAt(store, "spec", "provider", "vault").(map[string]any)
			test.change(provider)
			client, _ := testClient(store, openBaoTokenManifest())
			client.config = rest.Config{Host: "http://127.0.0.1:1"}
			_, err := client.OpenBaoStore(context.Background(), "borq-openbao")
			assert.True(err != nil)
			assert.True(!strings.Contains(err.Error(), "openbao-credential"))
		})
	}
	assert := is.New(t)
	client, _ := testClient()
	_, err := client.OpenBaoStore(context.Background(), "borq-openbao")
	assert.True(err != nil)
	client.config = rest.Config{Host: "http://127.0.0.1:1"}
	_, err = client.OpenBaoStore(context.Background(), "")
	assert.True(err != nil)
	secret := openBaoTokenManifest()
	secret["data"].(map[string]any)["token"] = "invalid base64"
	client, _ = testClient(openBaoStoreManifest(), secret)
	client.config = rest.Config{Host: "http://127.0.0.1:1"}
	_, err = client.OpenBaoStore(context.Background(), "borq-openbao")
	assert.True(err != nil)
}

// TestOpenBaoStoreSupportsClusterDnsName confirms the qualified service address
// still resolves through the selected cluster rather than the server's DNS.
func TestOpenBaoStoreSupportsClusterDnsName(t *testing.T) {
	assert := is.New(t)
	store := openBaoStoreManifest()
	provider := valueAt(store, "spec", "provider", "vault").(map[string]any)
	provider["server"] = "https://openbao.borq-system.svc.cluster.local:8200/"
	store["spec"].(map[string]any)["provider"] = map[string]any{"openbao": provider}
	client, _ := testClient(store, openBaoTokenManifest())
	client.config = rest.Config{Host: "https://cluster.example.test/prefix/"}
	access, err := client.OpenBaoStore(context.Background(), "borq-openbao")
	assert.NoErr(err)
	assert.Equal(access.Address, "https://cluster.example.test/prefix/api/v1/namespaces/borq-system/services/https:openbao:8200/proxy")
}

// TestOpenBaoStoreUsesPublicManagementVault keeps Kubernetes credentials out of
// cross-cluster vault requests and supports the managed borq token namespace.
func TestOpenBaoStoreUsesPublicManagementVault(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Header.Get("Authorization"), "")
		assert.Equal(request.Header.Get("X-Vault-Token"), "openbao-credential")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store, token := openBaoStoreManifest(), openBaoTokenManifest()
	provider := valueAt(store, "spec", "provider", "vault").(map[string]any)
	provider["server"] = server.URL
	valueAt(provider, "auth", "tokenSecretRef").(map[string]any)["namespace"] = "borq"
	token["metadata"].(map[string]any)["namespace"] = "borq"
	client, _ := testClient(store, token)
	client.config = rest.Config{Host: "https://target.example.test", BearerToken: "must-not-leak"}
	access, err := client.OpenBaoStore(t.Context(), "borq-openbao")
	assert.NoErr(err)
	assert.Equal(access.Address, server.URL)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, access.Address+"/v1/secret/data/test", nil)
	assert.NoErr(err)
	request.Header.Set("X-Vault-Token", access.Token)
	response, err := access.HttpClient.Do(request)
	assert.NoErr(err)
	assert.NoErr(response.Body.Close())
}

func openBaoStoreManifest() map[string]any {
	return map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ClusterSecretStore", "metadata": map[string]any{"name": "borq-openbao"},
		"spec": map[string]any{"provider": map[string]any{"vault": map[string]any{
			"server": "http://openbao.borq-system.svc:8200", "path": "secret", "version": "v2",
			"auth": map[string]any{"tokenSecretRef": map[string]any{"name": "openbao-token", "key": "token", "namespace": "borq-system"}},
		}}},
	}
}

func openBaoTokenManifest() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "openbao-token", "namespace": "borq-system"},
		"data": map[string]any{"token": base64.StdEncoding.EncodeToString([]byte("openbao-credential"))},
	}
}

// TestOpenBaoStoreMissingClassification distinguishes an absent store from
// missing token credentials, authorization failures, or transport failures.
func TestOpenBaoStoreMissingClassification(t *testing.T) {
	assert := is.New(t)
	client, _ := testClient()
	client.config = rest.Config{Host: "http://127.0.0.1:1"}
	_, err := client.OpenBaoStore(t.Context(), "borq-openbao")
	assert.True(errors.Is(err, ErrSecretStoreNotFound))
	client, _ = testClient(openBaoStoreManifest())
	client.config = rest.Config{Host: "http://127.0.0.1:1"}
	_, err = client.OpenBaoStore(t.Context(), "borq-openbao")
	assert.True(err != nil)
	assert.True(!errors.Is(err, ErrSecretStoreNotFound))
	for _, failure := range []error{
		apierrors.NewForbidden(schema.GroupResource{Group: "external-secrets.io", Resource: "clustersecretstores"}, "borq-openbao", errors.New("access denied")),
		errors.New("connection interrupted"),
	} {
		client, fakeClient := testClient()
		client.config = rest.Config{Host: "http://127.0.0.1:1"}
		fakeClient.PrependReactor("get", "clustersecretstores", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, failure })
		_, err := client.OpenBaoStore(t.Context(), "borq-openbao")
		assert.True(err != nil)
		assert.True(!errors.Is(err, ErrSecretStoreNotFound))
	}
}
