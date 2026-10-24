package apps

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"go.opentelemetry.io/otel"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ClusterKubeconfig contains credentials deliberately requested for one cluster.
// It must never be included in registration or list responses.
type ClusterKubeconfig struct {
	// Kubeconfig is a selected-context-only document with file credentials embedded.
	Kubeconfig string `json:"kubeconfig"`
}

// GetClusterKubeconfig exports the stored connection without contacting its API.
func (manager Manager) GetClusterKubeconfig(ctx context.Context, organization, name string) (ClusterKubeconfig, error) {
	ctx, span := otel.Tracer("borq/apps").Start(ctx, "apps.get_cluster_kubeconfig")
	defer span.End()
	if err := validateNames(organization, name); err != nil {
		return ClusterKubeconfig{}, err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return ClusterKubeconfig{}, err
	}
	configuration, err := exportClusterConfiguration(cluster)
	if err != nil {
		// Parser and file errors can contain credentials or private file names.
		return ClusterKubeconfig{}, fmt.Errorf("cluster kubeconfig cannot be exported as a self-contained document")
	}
	encoded, err := clientcmd.Write(configuration)
	if err != nil {
		return ClusterKubeconfig{}, fmt.Errorf("cluster kubeconfig could not be encoded")
	}
	return ClusterKubeconfig{Kubeconfig: string(encoded)}, nil
}

func exportClusterConfiguration(cluster Cluster) (clientcmdapi.Config, error) {
	var configuration clientcmdapi.Config
	if cluster.InCluster || (cluster.KubeconfigPath == "" && cluster.Kubeconfig == "") {
		connection, err := kubernetes.NewConnection(kubernetes.Config{InCluster: true}, logger.Logger{})
		if err != nil {
			return configuration, err
		}
		configuration = clusterConfigurationFromRest(connection.RestConfig())
	} else {
		// client-go exposes kubeconfig parsing and flattening through pointers.
		var loaded *clientcmdapi.Config
		var err error
		if cluster.Kubeconfig != "" {
			loaded, err = clientcmd.Load([]byte(cluster.Kubeconfig))
		} else {
			loaded, err = clientcmd.LoadFromFile(cluster.KubeconfigPath)
		}
		if err != nil {
			return configuration, err
		}
		configuration = *loaded
		if cluster.Context != "" {
			configuration.CurrentContext = cluster.Context
		}
	}
	return flattenClusterConfiguration(configuration, cluster.ClientApiServer)
}

func flattenClusterConfiguration(configuration clientcmdapi.Config, clientApiServer string) (clientcmdapi.Config, error) {
	if err := clientcmdapi.MinifyConfig(&configuration); err != nil {
		return clientcmdapi.Config{}, err
	}
	for _, auth := range configuration.AuthInfos {
		if auth.Exec != nil || auth.AuthProvider != nil {
			return clientcmdapi.Config{}, fmt.Errorf("external credential plugins are not self-contained")
		}
		if auth.TokenFile != "" {
			path := auth.TokenFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(auth.LocationOfOrigin), path)
			}
			token, err := os.ReadFile(path)
			if err != nil {
				return clientcmdapi.Config{}, err
			}
			auth.Token, auth.TokenFile = strings.TrimSpace(string(token)), ""
		}
		auth.Extensions = nil
	}
	if err := clientcmdapi.FlattenConfig(&configuration); err != nil {
		return clientcmdapi.Config{}, err
	}
	if err := validateClientApiServer(clientApiServer); err != nil {
		return clientcmdapi.Config{}, err
	}
	for _, target := range configuration.Clusters {
		if clientApiServer != "" {
			target.Server = clientApiServer
		}
		target.Extensions = nil
	}
	for _, selected := range configuration.Contexts {
		selected.Extensions = nil
	}
	configuration.Extensions = nil
	configuration.Preferences = clientcmdapi.Preferences{}
	if err := clientcmd.Validate(configuration); err != nil {
		return clientcmdapi.Config{}, err
	}
	return configuration, nil
}

func clusterConfigurationFromRest(configuration rest.Config) clientcmdapi.Config {
	// The kubeconfig API represents each named stanza with shared pointers.
	return clientcmdapi.Config{
		CurrentContext: "borq",
		Clusters: map[string]*clientcmdapi.Cluster{"borq": {
			Server: configuration.Host, TLSServerName: configuration.ServerName, InsecureSkipTLSVerify: configuration.Insecure,
			CertificateAuthority: configuration.CAFile, CertificateAuthorityData: configuration.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"borq": {
			Token: configuration.BearerToken, TokenFile: configuration.BearerTokenFile,
			ClientCertificate: configuration.CertFile, ClientCertificateData: configuration.CertData,
			ClientKey: configuration.KeyFile, ClientKeyData: configuration.KeyData,
		}},
		Contexts: map[string]*clientcmdapi.Context{"borq": {Cluster: "borq", AuthInfo: "borq"}},
	}
}

func validateClientApiServer(address string) error {
	if address == "" {
		return nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%w: client API server must be an absolute HTTP(S) URL without credentials, a query, or a fragment", ErrInvalid)
	}
	return nil
}
