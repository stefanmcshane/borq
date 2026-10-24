package cluster

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/cli/localconfig"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// LocalKubeconfigPath returns Borq's private credentials for a named local cluster.
func LocalKubeconfigPath(name string) (string, error) {
	if err := validateClusterName(name); err != nil {
		return "", err
	}
	directory, err := localconfig.Directory()
	if err != nil {
		return "", fmt.Errorf("find local cluster credentials directory: %w", err)
	}
	return filepath.Join(directory, "kubeconfigs", name), nil
}

// ImportImage imports a Docker image archive into a local k3d cluster without a Docker CLI.
func ImportImage(ctx context.Context, clusterName, archivePath string) error {
	if err := validateClusterName(clusterName); err != nil {
		return err
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("read bundled server image archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("server image archive must be a regular file")
	}
	environment, err := localDockerEnvironment(os.Environ())
	if err != nil {
		return err
	}
	_, err = runK3d(ctx, environment, "image", "import", "--cluster", clusterName, archivePath)
	if err != nil {
		return fmt.Errorf("import bundled server image: %w", err)
	}
	return nil
}

// ResolveLocalApiServer discovers a control-plane address reachable by Borq's pod
// while leaving the CLI's original kubeconfig and current context unchanged.
func ResolveLocalApiServer(ctx context.Context, kubeconfigPath, contextName string) (string, error) {
	configuration, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("read local cluster credentials: %w", err)
	}
	if contextName == "" {
		contextName = configuration.CurrentContext
	}
	selected := configuration.Contexts[contextName]
	if selected == nil {
		return "", fmt.Errorf("local kubeconfig context %q does not exist", contextName)
	}
	cluster := configuration.Clusters[selected.Cluster]
	if cluster == nil {
		return "", fmt.Errorf("local kubeconfig context %q has no cluster", contextName)
	}
	endpoint, err := url.Parse(cluster.Server)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", fmt.Errorf("local cluster API URL must use HTTP or HTTPS")
	}
	address := net.ParseIP(endpoint.Hostname())
	local := strings.EqualFold(endpoint.Hostname(), "localhost") || (address != nil && (address.IsLoopback() || address.IsUnspecified()))
	if !local {
		return cluster.Server, nil
	}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(*configuration, contextName, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return "", fmt.Errorf("configure local cluster discovery: %w", err)
	}
	restConfig.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("configure local cluster discovery: %w", err)
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("discover a server-reachable local API address: %w", err)
	}
	sort.Slice(nodes.Items, func(left, right int) bool { return nodes.Items[left].Name < nodes.Items[right].Name })
	for _, node := range nodes.Items {
		_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
		_, master := node.Labels["node-role.kubernetes.io/master"]
		if !controlPlane && !master && len(nodes.Items) != 1 {
			continue
		}
		for _, candidate := range node.Status.Addresses {
			ip := net.ParseIP(candidate.Address)
			if candidate.Type != corev1.NodeInternalIP || ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			endpoint.Host = net.JoinHostPort(candidate.Address, "6443")
			return endpoint.String(), nil
		}
	}
	return "", fmt.Errorf("local cluster has no control-plane InternalIP; supply --api-server explicitly")
}
