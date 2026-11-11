package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	defaultClusterName         = "management"
	defaultNamespace           = "borq-mgmt"
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
	borqHealthPath             = "/api/readyz"
)

// Request selects the existing Kubernetes cluster and minimal Borq server installation.
type Request struct {
	// ApiUrl is the Borq API base address used by the CLI after installation.
	ApiUrl              string
	advertisedApiUrl    string
	localRuntime        string
	localClusterName    string
	inferredClusterName bool
	bootstrapProxy      bool
	adminToken          string
	probeClient         *http.Client // Shares Kubernetes authentication across bootstrap probes.
	// Mode selects development or persistent management components; an empty value is inferred from the local runtime.
	Mode string
	// ClusterName optionally overrides the kubeconfig cluster name for a new installation.
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
	// Timeout bounds server installation and API readiness.
	Timeout time.Duration
	// BorqImage selects a server image reference or a tag in the default Borq repository.
	BorqImage string
	// ServerImageArchivePath optionally imports an offline server image into local kind or k3d.
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
	// Namespace is the selected Borq Service namespace used by private port forwarding.
	Namespace string
	// AdminToken authenticates bootstrap follow-up requests and is never serialized.
	AdminToken string `json:"-"`
	// HttpClient authenticates bootstrap requests through Kubernetes when direct access was not selected.
	HttpClient *http.Client `json:"-"` // HTTP clients share their transport and connection pool.
	// KubernetesProxy means ApiUrl is an authenticated bootstrap endpoint, not a browser address.
	KubernetesProxy bool
	// ManagementURL is the controller-assigned private HTTPS origin when networking is enabled.
	ManagementURL string
	// ApiUrl is the bootstrap API endpoint; KubernetesProxy endpoints require HttpClient.
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

// Execute installs Borq and waits for its private API; server reconciliation continues independently.
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
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: request.KubeconfigPath}, log,
		kubernetes.WithFieldOwner(defaultFieldOwner))
	if err != nil {
		return Result{}, err
	}
	request, err = restoreManagementClusterName(timeoutContext, connection, request)
	if err != nil {
		return Result{}, err
	}
	log.Info("starting_management_bootstrap", logger.WithFields(
		logger.String("cluster_name", request.ClusterName), logger.String("component_mode", request.Mode),
		logger.Bool("existing_cluster", reused)))
	if request.ServerImageArchivePath != "" {
		log.Info("importing_management_server_image")
		if err := importServerImage(timeoutContext, request); err != nil {
			return Result{}, fmt.Errorf("import bundled management server: %w", err)
		}
	}
	request, err = prepareAdminAuthentication(timeoutContext, request, connection)
	if err != nil {
		return Result{}, err
	}
	request, err = connectBootstrapApi(request, connection)
	if err != nil {
		return Result{}, err
	}
	if err := runManagementBootstrapWorkflow(timeoutContext, log, request, connection); err != nil {
		return Result{}, err
	}
	log.Info("management_bootstrap_completed", logger.WithFields(logger.String("cluster_name", request.ClusterName),
		logger.String("probe_url", request.ProbeURL), logger.Bool("reused", reused)))
	result := Result{AdminToken: request.adminToken, ApiUrl: request.ApiUrl, ClusterName: request.ClusterName, ProbeURL: request.ProbeURL,
		KubeconfigPath: request.KubeconfigPath, Namespace: request.Namespace, Reused: reused, KubernetesProxy: request.bootstrapProxy, HttpClient: request.probeClient}
	return result, nil
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
		if request.localRuntime == "native" || request.localRuntime == "external" || request.localRuntime == "kind" || (request.localRuntime == "" && request.Mode == "prod") {
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
	if request.localRuntime != "" && request.localRuntime != "external" && request.localRuntime != "docker" && request.localRuntime != "native" && request.localRuntime != "kind" {
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
		return errors.New("management resources must use the borq-mgmt namespace")
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

// The shared client pointer preserves authenticated Kubernetes transport state.
func probeBorq(ctx context.Context, probeURL string, client *http.Client) error {
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
