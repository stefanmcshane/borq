package management

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

// Bootstrap must not require public ingress or a host port before it can finish
// configuring those facilities. The API server authenticates this service proxy
// and removes Kubernetes authentication headers before forwarding to Borq.
func connectBootstrapApi(request Request, connection kubernetes.Connection) (Request, error) {
	if !request.bootstrapProxy {
		return request, nil
	}
	access, err := connectPrivateServer(connection, request.Namespace, request.adminToken)
	if err != nil {
		return Request{}, err
	}
	request.ApiUrl = access.URL
	request.ProbeURL = access.URL + borqHealthPath
	request.probeClient = access.HttpClient
	return request, nil
}

// PrivateServerInput selects explicit Kubernetes access and an already saved Borq credential.
type PrivateServerInput struct {
	// KubeconfigPath prevents teardown from borrowing ambient Kubernetes credentials.
	KubeconfigPath string
	// Namespace identifies the installation's private Borq Service.
	Namespace string
	// AdminToken is supplied by CLI login, never loaded from a cluster Secret.
	AdminToken string `json:"-"`
}

// PrivateServerAccess confines both credentials to the selected Kubernetes Service proxy.
type PrivateServerAccess struct {
	// URL includes the private Service proxy prefix used by the generated API client.
	URL string
	// HttpClient carries Kubernetes authentication and the separate Borq admin header.
	HttpClient *http.Client `json:"-"` // net/http requires a client pointer for shared transport state.
}

// ConnectPrivateServer connects without relying on public routes or reading cluster Secrets.
func ConnectPrivateServer(input PrivateServerInput) (PrivateServerAccess, error) {
	if strings.TrimSpace(input.KubeconfigPath) == "" || strings.TrimSpace(input.AdminToken) == "" {
		return PrivateServerAccess{}, errors.New("private Borq access requires an explicit kubeconfig and a saved login token")
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return PrivateServerAccess{}, err
	}
	return connectPrivateServer(connection, input.Namespace, input.AdminToken)
}

func connectPrivateServer(connection kubernetes.Connection, namespace, adminToken string) (PrivateServerAccess, error) {
	if problems := validation.IsDNS1123Label(namespace); len(problems) != 0 {
		return PrivateServerAccess{}, errors.New("invalid management namespace")
	}
	configuration := connection.RestConfig()
	endpoint, err := url.Parse(configuration.Host)
	if err != nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return PrivateServerAccess{}, errors.New("invalid Kubernetes API address")
	}
	host := endpoint.Hostname()
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback())) {
		return PrivateServerAccess{}, errors.New("Borq administrator tokens require HTTPS for private Kubernetes access; HTTP is allowed only for localhost or loopback")
	}
	transport, err := rest.TransportFor(&configuration)
	if err != nil {
		return PrivateServerAccess{}, fmt.Errorf("configure private Kubernetes access: %w", err)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/namespaces/" + namespace + "/services/http:borq:80/proxy"
	return PrivateServerAccess{URL: endpoint.String(), HttpClient: &http.Client{
		Transport: bootstrapTransport{endpoint: *endpoint, transport: transport, adminToken: adminToken},
		Timeout:   15 * time.Minute,
		// Never send Kubernetes credentials to a redirect destination.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type bootstrapTransport struct {
	endpoint   url.URL
	transport  http.RoundTripper
	adminToken string
}

// RoundTrip confines Kubernetes credentials to the selected Borq service proxy.
// The HTTP interfaces require request and response pointers.
func (transport bootstrapTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	endpoint := request.URL
	if endpoint.Scheme != transport.endpoint.Scheme || endpoint.Host != transport.endpoint.Host || endpoint.User != nil ||
		!strings.HasPrefix(path.Clean(endpoint.Path), transport.endpoint.Path+"/api/") {
		return nil, errors.New("Borq request is outside the selected Kubernetes service proxy")
	}
	forwarded := request.Clone(request.Context())
	forwarded.Header.Set("X-Borq-Admin-Token", transport.adminToken)
	return transport.transport.RoundTrip(forwarded)
}
