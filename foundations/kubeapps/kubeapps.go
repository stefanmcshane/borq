// Package kubeapps applies rendered application manifests and observes Kubernetes rollouts.
package kubeapps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const fieldManager = "borq-apps"
const targetLabel = "borq.run/target-id"

// ErrOwnershipConflict has package scope so callers can classify ownership conflicts with errors.Is.
var ErrOwnershipConflict = errors.New("Kubernetes ownership conflict")

// NewInput selects explicit Kubernetes credentials for a registered cluster.
type NewInput struct {
	// KubeconfigPath selects a kubeconfig file when InCluster is false.
	KubeconfigPath string
	// Context selects a context in the kubeconfig; empty selects its current context.
	Context string
	// InCluster uses the service account mounted in the Borq server's pod.
	InCluster bool
}

// Client reads and applies rendered resources without exposing Kubernetes SDK types.
type Client struct {
	dynamic dynamic.Interface
	mapper  meta.RESTMapper
	// Keep target credentials private so service proxy requests use the registered cluster.
	config rest.Config
	// Discovery clients are pointers because the Kubernetes SDK requires shared transport state.
	discovery *discovery.DiscoveryClient
}

// ResourceDiff compares a live resource with the result of applying its planned configuration.
type ResourceDiff struct {
	// ApiVersion identifies the Kubernetes resource API.
	ApiVersion string `json:"apiVersion"`
	// Kind identifies the Kubernetes resource kind.
	Kind string `json:"kind"`
	// Namespace scopes the resource.
	Namespace string `json:"namespace"`
	// Name identifies the resource within its namespace.
	Name string `json:"name"`
	// Action is create, update, unchanged, or delete.
	Action string `json:"action"`
	// Live contains normalized live configuration, or nil for a missing resource.
	Live map[string]any `json:"live"`
	// Desired contains normalized predicted configuration after apply.
	Desired map[string]any `json:"desired"`
	// Fingerprint identifies live configuration while excluding changing runtime status.
	Fingerprint string `json:"fingerprint"`
}

// RolloutStatus reports the aggregate state of the resources in an application rollout.
type RolloutStatus struct {
	// State is running, succeeded, or failed.
	State string `json:"state"`
	// Message summarizes the outstanding or failed resource.
	Message string `json:"message"`
	// Resources contains an observation for every planned resource.
	Resources []ResourceStatus `json:"resources"`
}

// ResourceStatus describes the rollout of one Kubernetes resource.
type ResourceStatus struct {
	// ApiVersion identifies the resource API.
	ApiVersion string `json:"apiVersion"`
	// Kind identifies the resource kind.
	Kind string `json:"kind"`
	// Namespace scopes the resource.
	Namespace string `json:"namespace"`
	// Name identifies the resource.
	Name string `json:"name"`
	// State is running, succeeded, or failed.
	State string `json:"state"`
	// Message explains progress or failure.
	Message string `json:"message"`
}

// New constructs a client without contacting the cluster; Validate performs an explicit connection check.
func New(input NewInput) (Client, error) {
	if input.InCluster && (input.KubeconfigPath != "" || input.Context != "") {
		return Client{}, fmt.Errorf("in-cluster credentials cannot be combined with kubeconfig or context")
	}
	config, err := connectionConfig(input)
	if err != nil {
		return Client{}, fmt.Errorf("load cluster credentials: %w", err)
	}
	config.Timeout = 30 * time.Second
	// A batch previews and applies several resources per app before polling rollout status.
	config.QPS = 20
	config.Burst = 50
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return Client{}, fmt.Errorf("create Kubernetes resource client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return Client{}, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	return Client{
		dynamic:   dynamicClient,
		mapper:    restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient)),
		discovery: discoveryClient,
		config:    *rest.CopyConfig(config),
	}, nil
}

// Validate checks cluster connectivity and the credentials' ability to discover resources.
func (client Client) Validate(ctx context.Context) error {
	if client.discovery == nil {
		return fmt.Errorf("Kubernetes discovery client is not configured")
	}
	_, err := client.discovery.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("connect to Kubernetes cluster: %w", err)
	}
	return nil
}

// The Kubernetes SDK returns pointer configs and mutates their transport defaults.
func connectionConfig(input NewInput) (*rest.Config, error) {
	if input.InCluster {
		return rest.InClusterConfig()
	}
	if input.KubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig path is required unless in-cluster credentials are selected")
	}
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	loading.ExplicitPath = input.KubeconfigPath
	overrides := clientcmd.ConfigOverrides{CurrentContext: input.Context}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &overrides).ClientConfig()
}

func (client Client) resource(manifest map[string]any) (dynamic.ResourceInterface, error) {
	version, _ := manifest["apiVersion"].(string)
	kind, _ := manifest["kind"].(string)
	groupVersion, err := schema.ParseGroupVersion(version)
	if err != nil {
		return nil, fmt.Errorf("parse resource API version: %w", err)
	}
	mapping, err := client.mapper.RESTMapping(schema.GroupKind{Group: groupVersion.Group, Kind: kind}, groupVersion.Version)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", kind, err)
	}
	namespace := textAt(manifest, "metadata", "namespace")
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return client.dynamic.Resource(mapping.Resource).Namespace(namespace), nil
	}
	return client.dynamic.Resource(mapping.Resource), nil
}

func validateManifest(manifest map[string]any, targetId string) error {
	if targetId == "" {
		return fmt.Errorf("target ID is required")
	}
	version := textAt(manifest, "apiVersion")
	kind := textAt(manifest, "kind")
	allowed := version == "apps/v1" && kind == "Deployment" || version == "v1" && kind == "Service" || version == "networking.k8s.io/v1" && kind == "Ingress" || version == "gateway.networking.k8s.io/v1" && (kind == "HTTPRoute" || kind == "Gateway") || version == "cert-manager.io/v1" && kind == "Certificate" || version == "batch/v1" && kind == "Job" || version == "external-secrets.io/v1" && kind == "ExternalSecret"
	if version == "v1" && (kind == "PersistentVolumeClaim" || kind == "ConfigMap" || kind == "Secret") && textAt(manifest, "metadata", "labels", "borq.run/phase") == "sqlite" {
		allowed = true
	}
	if textAt(manifest, "metadata", "labels", "borq.run/phase") == "sqlite" && (version == "v1" && kind == "ServiceAccount" || version == "rbac.authorization.k8s.io/v1" && (kind == "Role" || kind == "RoleBinding") || version == "batch/v1" && kind == "CronJob") {
		allowed = true
	}
	if !allowed {
		return fmt.Errorf("unsupported application resource %s %s", version, kind)
	}
	if textAt(manifest, "metadata", "name") == "" || textAt(manifest, "metadata", "namespace") == "" {
		return fmt.Errorf("%s requires a name and namespace", kind)
	}
	if textAt(manifest, "metadata", "labels", targetLabel) != targetId {
		return fmt.Errorf("%s %s must carry target label %q", kind, textAt(manifest, "metadata", "name"), targetId)
	}
	if kind == "Job" && textAt(manifest, "metadata", "labels", "borq.run/phase") != "predeploy" {
		return fmt.Errorf("Job %s must carry the predeploy phase label", textAt(manifest, "metadata", "name"))
	}
	return nil
}

func checkOwnership(live map[string]any, targetId string) error {
	if textAt(live, "metadata", "labels", targetLabel) == targetId {
		return nil
	}
	return fmt.Errorf("%w: refuse to adopt %s %s/%s: resource is not owned by target %q", ErrOwnershipConflict, textAt(live, "kind"), textAt(live, "metadata", "namespace"), textAt(live, "metadata", "name"), targetId)
}

func namespaceResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
}

func (client Client) namespaceExists(ctx context.Context, namespace string) (bool, error) {
	_, err := client.dynamic.Resource(namespaceResource()).Get(ctx, namespace, metav1.GetOptions{})
	return err == nil, err
}
