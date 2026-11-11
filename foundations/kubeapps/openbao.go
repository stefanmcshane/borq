package kubeapps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// ErrSecretStoreNotFound identifies an absent configured ClusterSecretStore.
// It is distinct from missing credentials or denied access to an existing store.
var ErrSecretStoreNotFound = errors.New("OpenBao ClusterSecretStore not found")

// OpenBaoAccess contains a target cluster's existing KV v2 store connection.
// Credentials are read from the store's token Secret and must not be persisted in plans.
type OpenBaoAccess struct {
	// Address reaches the management vault directly or through a local Service proxy.
	Address string
	// Mount selects the preinstalled KV v2 engine.
	Mount string
	// Token authorizes OpenBao reads and writes and must never appear in diagnostics.
	Token string
	// HttpClient authenticates requests to the registered cluster's service proxy.
	HttpClient http.Client
}

// OpenBaoStore reads a preinstalled ClusterSecretStore and its token Secret.
// Local Borq service names use the Kubernetes proxy; public management URLs use
// an independent HTTP transport that never carries Kubernetes credentials.
func (client Client) OpenBaoStore(ctx context.Context, storeName string) (OpenBaoAccess, error) {
	if storeName == "" {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao ClusterSecretStore name is required")
	}
	if client.dynamic == nil || client.config.Host == "" {
		return OpenBaoAccess{}, fmt.Errorf("registered Kubernetes credentials are required to access OpenBao")
	}
	resource := schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}
	store, err := client.dynamic.Resource(resource).Get(ctx, storeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return OpenBaoAccess{}, fmt.Errorf("%w: %s", ErrSecretStoreNotFound, storeName)
	}
	if err != nil {
		return OpenBaoAccess{}, fmt.Errorf("read OpenBao ClusterSecretStore %s: %w", storeName, err)
	}
	provider, err := openBaoProvider(store.Object)
	if err != nil {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao ClusterSecretStore %s: %w", storeName, err)
	}
	return client.openBaoAccess(ctx, provider)
}

func (client Client) openBaoAccess(ctx context.Context, provider map[string]any) (OpenBaoAccess, error) {
	service, err := url.Parse(textAt(provider, "server"))
	if err != nil || service == nil || (service.Scheme != "http" && service.Scheme != "https") || service.Hostname() == "" || service.User != nil || (service.Path != "" && service.Path != "/") || service.RawQuery != "" || service.Fragment != "" {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao server must be an HTTP or HTTPS origin")
	}
	serviceNamespace := ""
	for _, candidate := range []string{"borq", "borq-mgmt", "borq-system"} {
		if service.Hostname() == "openbao."+candidate+".svc" || service.Hostname() == "openbao."+candidate+".svc.cluster.local" {
			serviceNamespace = candidate
		}
	}
	if serviceNamespace != "" && service.Port() != "8200" || serviceNamespace == "" && (strings.HasSuffix(service.Hostname(), ".svc") || strings.HasSuffix(service.Hostname(), ".svc.cluster.local")) {
		return OpenBaoAccess{}, fmt.Errorf("local OpenBao must use the borq service on port 8200")
	}
	if textAt(provider, "version") != "v2" {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao store must use a KV v2 engine")
	}
	mount := strings.Trim(textAt(provider, "path"), "/")
	if mount == "" || strings.ContainsAny(mount, "?#\\") {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao KV v2 mount is required")
	}
	for _, part := range strings.Split(mount, "/") {
		if part == "" || part == "." || part == ".." {
			return OpenBaoAccess{}, fmt.Errorf("OpenBao KV v2 mount must contain valid path segments")
		}
	}
	if textAt(provider, "namespace") != "" {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao store namespaces are not supported")
	}
	name := textAt(provider, "auth", "tokenSecretRef", "name")
	key := textAt(provider, "auth", "tokenSecretRef", "key")
	namespace := textAt(provider, "auth", "tokenSecretRef", "namespace")
	if name == "" || key == "" || (namespace != "borq" && namespace != "borq-system") {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao tokenSecretRef requires a name and key in borq")
	}
	secretResource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secret, err := client.dynamic.Resource(secretResource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return OpenBaoAccess{}, fmt.Errorf("read OpenBao token Secret %s/%s: %w", namespace, name, err)
	}
	token, err := base64.StdEncoding.DecodeString(textAt(secret.Object, "data", key))
	if err != nil || len(token) == 0 {
		return OpenBaoAccess{}, fmt.Errorf("OpenBao token Secret does not contain a valid token at the configured key")
	}
	address := strings.TrimRight(service.String(), "/")
	httpClient := http.Client{}
	if serviceNamespace != "" {
		transport, err := rest.HTTPClientFor(&client.config)
		if err != nil {
			return OpenBaoAccess{}, fmt.Errorf("configure OpenBao service proxy transport: %w", err)
		}
		httpClient = *transport
		address = strings.TrimRight(client.config.Host, "/") + "/api/v1/namespaces/" + serviceNamespace + "/services/" + service.Scheme + ":openbao:8200/proxy"
	} else {
		// This transport deliberately excludes the registered cluster's bearer token and client certificate.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if bundle := textAt(provider, "caBundle"); bundle != "" {
			certificate, err := base64.StdEncoding.DecodeString(bundle)
			if err != nil {
				return OpenBaoAccess{}, fmt.Errorf("invalid OpenBao CA bundle")
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(certificate) {
				return OpenBaoAccess{}, fmt.Errorf("invalid OpenBao CA bundle")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
		httpClient.Transport = transport
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("OpenBao redirects are not allowed") }
	return OpenBaoAccess{Address: address, Mount: mount, Token: string(token), HttpClient: httpClient}, nil

}

func openBaoProvider(store map[string]any) (map[string]any, error) {
	vault, _ := valueAt(store, "spec", "provider", "vault").(map[string]any)
	openBao, _ := valueAt(store, "spec", "provider", "openbao").(map[string]any)
	if vault != nil && openBao != nil {
		return nil, fmt.Errorf("configure exactly one OpenBao provider")
	}
	if vault != nil {
		return vault, nil
	}
	if openBao != nil {
		return openBao, nil
	}
	return nil, fmt.Errorf("a Vault-compatible OpenBao provider is required")
}

// OpenBaoProvider copies the validated target provider for an ESO generator without exposing its token.
func (client Client) OpenBaoProvider(ctx context.Context, storeName string) (map[string]any, error) {
	if storeName == "" || client.dynamic == nil || client.config.Host == "" {
		return nil, fmt.Errorf("registered Kubernetes credentials and store name are required")
	}
	resource := schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}
	store, err := client.dynamic.Resource(resource).Get(ctx, storeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read validated OpenBao provider: %w", err)
	}
	provider, err := openBaoProvider(store.Object)
	if err != nil {
		return nil, err
	}
	if _, err := client.openBaoAccess(ctx, provider); err != nil {
		return nil, err
	}
	return copyMap(provider), nil
}
