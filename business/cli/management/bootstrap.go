package management

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/distribution/reference"
	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

const (
	defaultClusterName         = "management"
	defaultNamespace           = "borq"
	defaultHostPort            = 7833
	defaultNodePort            = 31833
	defaultTelemetryHostPort   = 7834
	defaultTelemetryNodePort   = 31834
	defaultOpenbaoHostPort     = 8200
	defaultOpenbaoNodePort     = 30200
	defaultTimeout             = 15 * time.Minute
	defaultFieldOwner          = "borq-management-bootstrap"
	borqImageRepository        = "ghcr.io/stefanmcshane/borq"
	borqImage                  = borqImageRepository + ":latest"
	borqName                   = "borq"
	borqServiceAccountName     = "borq"
	borqRoleName               = "borq"
	borqRoleBindingName        = "borq"
	borqDataMountPath          = "/var/lib/borq"
	borqContainerPort          = 7833
	borqTelemetryContainerPort = 7834
	borqHealthPath             = "/api/healthz"
)

// Request selects a management installation and its public service addresses.
type Request struct {
	// ApiUrl is the Borq API base address used by the CLI after installation.
	ApiUrl           string
	advertisedApiUrl string
	localRuntime     string
	// Mode selects development or persistent management components; an empty value is inferred from the local runtime.
	Mode string
	// ClusterName identifies the existing management installation.
	ClusterName string
	// HostPort is the local Docker API port exposed automatically during bootstrap.
	HostPort int32
	// Namespace contains all Borq-managed management components.
	Namespace string
	// NodePort publishes the Borq server from the management cluster.
	NodePort int32
	// TelemetryHostPort is the already-published development ingestion port.
	TelemetryHostPort int32
	// TelemetryNodePort exposes the ingestion-only listener to deployment targets.
	TelemetryNodePort int32
	// OpenbaoHostPort is the already-published development vault port.
	OpenbaoHostPort int32
	// ProbeURL optionally overrides the Borq health endpoint used after installation.
	ProbeURL string
	// Timeout bounds server installation and waiting for server-owned setup.
	Timeout time.Duration
	// BorqImage selects a server image reference or a tag in the default Borq repository.
	BorqImage string
	// ServerImageArchivePath optionally imports an offline server image into local k3d.
	ServerImageArchivePath string
	// KubeconfigPath selects existing external Kubernetes instead of local provisioning.
	KubeconfigPath string
	// OpenbaoUrl is the vault endpoint reachable from deployment target clusters.
	OpenbaoUrl string
	// TelemetryUrl is Borq's authenticated ingestion endpoint reachable from targets.
	TelemetryUrl string
	// LokiUrl optionally selects a full authenticated log ingestion endpoint.
	LokiUrl string
	// PrometheusUrl optionally selects a full authenticated metrics ingestion endpoint.
	PrometheusUrl string
	// S3Endpoint overrides the management object store address reachable from target clusters.
	S3Endpoint string
	// StorageClass selects persistent storage; borq-local uses k3s local-path.
	StorageClass string
}

// Result identifies the installed server and the private kubeconfig used for it.
type Result struct {
	// ApiUrl is the API base address clients can save and use after bootstrap.
	ApiUrl string
	// ClusterName identifies the management installation.
	ClusterName string
	// ProbeURL is the health endpoint that passed after installation.
	ProbeURL string
	// KubeconfigPath is the private cluster connection file.
	KubeconfigPath string
	// Reused indicates an existing cluster was selected.
	Reused bool
}

// DefaultRequest returns local management installation settings.
func DefaultRequest() Request { return Request{}.withDefaults() }

// Execute installs Borq on existing management Kubernetes and waits for component setup.
func Execute(ctx context.Context, log logger.Logger, request Request) (Result, error) {
	executable, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("locate Borq executable: %w", err)
	}
	request, err = resolveBootstrapFiles(request, executable)
	if err != nil {
		return Result{}, err
	}
	if err := request.validate(); err != nil {
		return Result{}, err
	}
	timeoutContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	reused := true // Management Kubernetes is created explicitly before bootstrap.
	log.Info("starting_management_bootstrap", logger.WithFields(
		logger.String("cluster_name", request.ClusterName), logger.String("component_mode", request.Mode),
		logger.Bool("existing_cluster", reused)))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: request.KubeconfigPath}, log,
		kubernetes.WithFieldOwner(defaultFieldOwner))
	if err != nil {
		return Result{}, err
	}
	request, err = resolveManagementAddresses(timeoutContext, request, connection)
	if err != nil {
		return Result{}, err
	}
	if request.ServerImageArchivePath != "" {
		log.Info("importing_management_server_image")
		if err := localcluster.ImportImage(timeoutContext, request.ClusterName, request.ServerImageArchivePath); err != nil {
			return Result{}, fmt.Errorf("import bundled management server: %w", err)
		}
	}
	if err := runManagementBootstrapWorkflow(timeoutContext, log, request, connection); err != nil {
		return Result{}, err
	}
	log.Info("management_bootstrap_completed", logger.WithFields(logger.String("cluster_name", request.ClusterName),
		logger.String("probe_url", request.ProbeURL), logger.Bool("reused", reused)))
	return Result{ApiUrl: request.ApiUrl, ClusterName: request.ClusterName, ProbeURL: request.ProbeURL,
		KubeconfigPath: request.KubeconfigPath, Reused: reused}, nil
}

func (request Request) withDefaults() Request {
	if request.Mode == "" {
		request.Mode = "prod"
	}
	if request.Mode == "production" {
		request.Mode = "prod"
	}
	if request.ClusterName == "" {
		request.ClusterName = defaultClusterName
	}
	if request.HostPort == 0 {
		request.HostPort = defaultHostPort
	}
	if request.Namespace == "" {
		request.Namespace = defaultNamespace
	}
	if request.NodePort == 0 {
		request.NodePort = defaultNodePort
	}
	if request.TelemetryHostPort == 0 {
		request.TelemetryHostPort = defaultTelemetryHostPort
	}
	if request.TelemetryNodePort == 0 {
		request.TelemetryNodePort = defaultTelemetryNodePort
	}
	if request.OpenbaoHostPort == 0 {
		request.OpenbaoHostPort = defaultOpenbaoHostPort
	}
	if request.ProbeURL == "" {
		probePort := request.HostPort
		if request.localRuntime == "native" || request.localRuntime == "external" || (request.localRuntime == "" && request.Mode == "prod") {
			probePort = request.NodePort
		}
		request.ProbeURL = probeURLForHostPort(probePort)
		if request.ApiUrl != "" {
			request.ProbeURL = strings.TrimRight(request.ApiUrl, "/") + borqHealthPath
		}
	}
	if request.Timeout == 0 {
		request.Timeout = defaultTimeout
	}
	if request.BorqImage == "" {
		request.BorqImage = borqImage
	} else if !strings.ContainsAny(request.BorqImage, "/:@") {
		request.BorqImage = borqImageRepository + ":" + request.BorqImage
	}
	if request.StorageClass == "" {
		request.StorageClass = "borq-local"
	}
	return request
}

func (request Request) validate() error {
	if request.localRuntime != "" && request.localRuntime != "external" && request.localRuntime != "docker" && request.localRuntime != "native" {
		return errors.New("management runtime must be docker or native")
	}
	if request.localRuntime == "docker" && request.Mode != "dev" {
		return errors.New("k3d management clusters require development mode")
	}
	if request.Mode != "dev" && request.Mode != "prod" {
		return errors.New("management component mode must be dev or prod")
	}
	if strings.TrimSpace(request.ClusterName) == "" {
		return errors.New("cluster name must not be empty")
	}
	if strings.ContainsAny(request.ClusterName, "/\\ ") {
		return errors.New("cluster name must not contain paths or spaces")
	}
	if request.Namespace != defaultNamespace {
		return errors.New("management resources must use the borq namespace")
	}
	if request.HostPort <= 0 || request.HostPort > 65535 {
		return errors.New("host port must be between 1 and 65535")
	}
	if request.OpenbaoHostPort <= 0 || request.OpenbaoHostPort > 65535 {
		return errors.New("OpenBao host port must be between 1 and 65535")
	}
	if request.NodePort < 30000 || request.NodePort > 32767 {
		return errors.New("node port must be between 30000 and 32767")
	}
	if request.TelemetryHostPort <= 0 || request.TelemetryHostPort > 65535 {
		return errors.New("telemetry host port must be between 1 and 65535")
	}
	if request.TelemetryNodePort < 30000 || request.TelemetryNodePort > 32767 {
		return errors.New("telemetry node port must be between 30000 and 32767")
	}
	if request.TelemetryHostPort == request.HostPort || request.TelemetryHostPort == request.OpenbaoHostPort || request.HostPort == request.OpenbaoHostPort {
		return errors.New("management host ports must be distinct")
	}
	if request.TelemetryNodePort == request.NodePort || request.TelemetryNodePort == defaultOpenbaoNodePort || request.NodePort == defaultOpenbaoNodePort {
		return errors.New("management node ports must be distinct")
	}
	if request.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	if strings.TrimSpace(request.ProbeURL) == "" {
		return errors.New("probe URL must not be empty")
	}
	if strings.TrimSpace(request.BorqImage) == "" {
		return errors.New("borq image must not be empty")
	}
	if _, err := reference.ParseNormalizedNamed(request.BorqImage); err != nil {
		return errors.New("server image must be a valid image reference or tag")
	}
	if strings.TrimSpace(request.KubeconfigPath) == "" {
		return errors.New("management kubeconfig path must not be empty")
	}
	if request.ServerImageArchivePath != "" && request.localRuntime == "native" {
		return errors.New("local server image archives require the Docker runtime")
	}
	for _, endpoint := range []string{request.ApiUrl, request.ProbeURL, request.OpenbaoUrl, request.TelemetryUrl, request.LokiUrl, request.PrometheusUrl, request.S3Endpoint} {
		if endpoint == "" {
			continue
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return errors.New("invalid management endpoint: expected an HTTP(S) URL without credentials, query, or fragment")
		}
	}
	if request.S3Endpoint != "" {
		endpoint, _ := url.Parse(request.S3Endpoint)
		if endpoint.Path != "" && endpoint.Path != "/" {
			return errors.New("management S3 endpoint must be an origin without a path")
		}
	}
	return nil
}

func resolveManagementAddresses(ctx context.Context, request Request, connection kubernetes.Connection) (Request, error) {
	if request.ApiUrl == "" {
		request.ApiUrl = strings.TrimSuffix(request.ProbeURL, borqHealthPath)
	}
	request.ApiUrl = strings.TrimRight(request.ApiUrl, "/")
	request.advertisedApiUrl = request.ApiUrl
	if request.OpenbaoUrl != "" && request.TelemetryUrl != "" && (request.Mode != "dev" || request.S3Endpoint != "") {
		return request, nil
	}
	address, err := url.Parse(request.ApiUrl)
	if err != nil {
		return Request{}, err
	}
	host := address.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && !ip.IsLoopback() && !ip.IsUnspecified() {
		return managementPortOverrides(request), nil
	}
	// A CLI loopback address is not usable by pods. Kubernetes already knows
	// the node's address, so discover it without Docker or other host tools.
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires a configuration pointer to create its client.
	client, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return Request{}, fmt.Errorf("create management discovery client: %w", err)
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Request{}, fmt.Errorf("discover management node address: %w", err)
	}
	request.advertisedApiUrl, err = managementNodeApiUrl(nodes.Items, request.NodePort)
	if err != nil {
		return Request{}, err
	}
	return managementPortOverrides(request), nil
}

func managementPortOverrides(request Request) Request {
	if request.TelemetryUrl != "" || request.TelemetryNodePort == defaultTelemetryNodePort {
		return request
	}
	address, _ := url.Parse(request.advertisedApiUrl)
	request.TelemetryUrl = "http://" + net.JoinHostPort(address.Hostname(), fmt.Sprint(request.TelemetryNodePort))
	return request
}

func managementNodeApiUrl(nodes []corev1.Node, nodePort int32) (string, error) {
	for _, kind := range []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP} {
		for _, node := range nodes {
			for _, address := range node.Status.Addresses {
				ip := net.ParseIP(address.Address)
				if address.Type == kind && ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
					return "http://" + net.JoinHostPort(address.Address, fmt.Sprint(nodePort)), nil
				}
			}
		}
	}
	return "", errors.New("management nodes have no usable IP address; provide a target-reachable --api-url")
}

func probeURLForHostPort(hostPort int32) string {
	return fmt.Sprintf("http://localhost:%d%s", hostPort, borqHealthPath)
}

func managementNamespace(namespace string) kubernetes.Namespace {
	return kubernetes.Namespace{
		Name: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":       "borq-management-bootstrap",
			"app.kubernetes.io/managed-by": "borq",
		},
	}
}

func appLabels(name string, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "borq",
	}
}

func selectorLabels(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": name}
}

func podLabels(labels map[string]string, name string) map[string]string {
	clonedLabels := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		clonedLabels[key] = value
	}
	clonedLabels["app.kubernetes.io/name"] = name
	return clonedLabels
}

func probeBorq(ctx context.Context, probeURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return fmt.Errorf("build probe request: %w", err)
		}

		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("probe %s did not succeed before timeout: %w", probeURL, ctx.Err())
		case <-ticker.C:
		}
	}
}
