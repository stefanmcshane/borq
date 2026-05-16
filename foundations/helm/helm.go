package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	discoverymemory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/storage/driver"
)

const defaultActionTimeout = 5 * time.Minute

// ReleaseManager installs and upgrades Helm releases through the Helm SDK.
type ReleaseManager struct {
	kubeconfigPath string
	restConfig     rest.Config
	tempDir        string
}

// NewReleaseManagerInput describes the shared Helm SDK setup for a bootstrap run.
type NewReleaseManagerInput struct {
	// Connection provides the Kubernetes REST config shared with borq foundations.
	Connection kubernetes.Connection
	// KubeconfigPath is the explicit kubeconfig path used by the bootstrap command.
	KubeconfigPath string
}

// InstallOrUpgradeInput describes one Helm release reconciliation run.
type InstallOrUpgradeInput struct {
	// Namespace is the release namespace.
	Namespace string
	// ReleaseName is the Helm release name.
	ReleaseName string
	// ChartRef is either a chart name in an HTTP repository or a full OCI chart reference.
	ChartRef string
	// RepositoryUrl is the HTTP repository URL used for non-OCI charts.
	RepositoryUrl string
	// Version pins the chart version when set.
	Version string
	// Values contains the chart values passed to Helm.
	Values map[string]any
	// Timeout bounds the Helm install or upgrade action.
	Timeout time.Duration
}

// NewReleaseManager creates the shared Helm CLI environment for a bootstrap run.
func NewReleaseManager(input NewReleaseManagerInput) (ReleaseManager, error) {
	kubeconfigPath := strings.TrimSpace(input.KubeconfigPath)
	if kubeconfigPath == "" {
		return ReleaseManager{}, errors.New("helm kubeconfig path must not be empty")
	}

	tempDir, err := os.MkdirTemp("", "borq-helm-*")
	if err != nil {
		return ReleaseManager{}, fmt.Errorf("create temporary helm home: %w", err)
	}

	manager := ReleaseManager{
		kubeconfigPath: kubeconfigPath,
		restConfig:     input.Connection.RestConfig(),
		tempDir:        tempDir,
	}
	if err := manager.ensureCliLayout(); err != nil {
		_ = os.RemoveAll(tempDir)
		return ReleaseManager{}, err
	}

	return manager, nil
}

// Close removes the temporary Helm CLI state created for the manager.
func (manager ReleaseManager) Close() error {
	if strings.TrimSpace(manager.tempDir) == "" {
		return nil
	}

	return os.RemoveAll(manager.tempDir)
}

// InstallOrUpgrade installs the release when absent and upgrades it when it already exists.
func (manager ReleaseManager) InstallOrUpgrade(ctx context.Context, input InstallOrUpgradeInput) error {
	kube.ManagedFieldsManager = "helm-controller"

	if err := validateInstallOrUpgradeInput(input); err != nil {
		return err
	}

	settings := manager.newEnvSettings(strings.TrimSpace(input.Namespace))
	restClientGetter := manager.restClientGetter(strings.TrimSpace(input.Namespace))

	actionConfig := action.NewConfiguration()
	if err := actionConfig.Init(restClientGetter, input.Namespace, "secrets"); err != nil {
		return fmt.Errorf("initialize helm action configuration: %w", err)
	}

	registryClient, err := registry.NewClient(
		registry.ClientOptWriter(io.Discard),
		registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return fmt.Errorf("create helm registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	chartPath, chart, err := locateChart(settings, registryClient, input)
	if err != nil {
		return err
	}

	_, err = actionConfig.Releases.Last(input.ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return manager.install(ctx, actionConfig, registryClient, chart, chartPath, input)
	}
	if err != nil {
		return fmt.Errorf("lookup helm release %q: %w", input.ReleaseName, err)
	}

	return manager.upgrade(ctx, actionConfig, registryClient, chart, chartPath, input)
}

func (manager ReleaseManager) install(
	ctx context.Context,
	actionConfig *action.Configuration,
	registryClient *registry.Client,
	chart *chartv2.Chart,
	chartPath string,
	input InstallOrUpgradeInput,
) error {
	install := action.NewInstall(actionConfig)
	install.ReleaseName = input.ReleaseName
	install.Namespace = input.Namespace
	install.CreateNamespace = true
	install.Timeout = timeoutForInput(input.Timeout)
	install.WaitStrategy = kube.StatusWatcherStrategy
	install.ChartPathOptions.RepoURL = strings.TrimSpace(input.RepositoryUrl)
	install.ChartPathOptions.Version = strings.TrimSpace(input.Version)
	install.SetRegistryClient(registryClient)

	if _, err := install.RunWithContext(ctx, chart, valuesOrEmpty(input.Values)); err != nil {
		return fmt.Errorf("install helm release %q from %q: %w", input.ReleaseName, chartPath, err)
	}

	return nil
}

func (manager ReleaseManager) upgrade(
	ctx context.Context,
	actionConfig *action.Configuration,
	registryClient *registry.Client,
	chart *chartv2.Chart,
	chartPath string,
	input InstallOrUpgradeInput,
) error {
	upgrade := action.NewUpgrade(actionConfig)
	upgrade.Namespace = input.Namespace
	upgrade.Timeout = timeoutForInput(input.Timeout)
	upgrade.WaitStrategy = kube.StatusWatcherStrategy
	upgrade.ChartPathOptions.RepoURL = strings.TrimSpace(input.RepositoryUrl)
	upgrade.ChartPathOptions.Version = strings.TrimSpace(input.Version)
	upgrade.SetRegistryClient(registryClient)

	if _, err := upgrade.RunWithContext(ctx, input.ReleaseName, chart, valuesOrEmpty(input.Values)); err != nil {
		return fmt.Errorf("upgrade helm release %q from %q: %w", input.ReleaseName, chartPath, err)
	}

	return nil
}

func locateChart(
	settings *cli.EnvSettings,
	registryClient *registry.Client,
	input InstallOrUpgradeInput,
) (string, *chartv2.Chart, error) {
	chartLocator := action.NewInstall(action.NewConfiguration())
	chartLocator.ChartPathOptions.RepoURL = strings.TrimSpace(input.RepositoryUrl)
	chartLocator.ChartPathOptions.Version = strings.TrimSpace(input.Version)
	chartLocator.SetRegistryClient(registryClient)

	chartPath, err := chartLocator.LocateChart(strings.TrimSpace(input.ChartRef), settings)
	if err != nil {
		return "", nil, fmt.Errorf("locate helm chart %q: %w", input.ChartRef, err)
	}

	chart, err := loader.Load(chartPath)
	if err != nil {
		return "", nil, fmt.Errorf("load helm chart %q: %w", chartPath, err)
	}

	return chartPath, chart, nil
}

func validateInstallOrUpgradeInput(input InstallOrUpgradeInput) error {
	if strings.TrimSpace(input.Namespace) == "" {
		return errors.New("helm namespace must not be empty")
	}
	if strings.TrimSpace(input.ReleaseName) == "" {
		return errors.New("helm release name must not be empty")
	}
	if strings.TrimSpace(input.ChartRef) == "" {
		return errors.New("helm chart ref must not be empty")
	}

	return nil
}

func (manager ReleaseManager) newEnvSettings(namespace string) *cli.EnvSettings {
	settings := cli.New()
	settings.KubeConfig = manager.kubeconfigPath
	settings.SetNamespace(namespace)
	settings.RepositoryCache = filepath.Join(manager.tempDir, "repository-cache")
	settings.RepositoryConfig = filepath.Join(manager.tempDir, "repository", "repositories.yaml")
	settings.RegistryConfig = filepath.Join(manager.tempDir, "registry", "config.json")
	settings.ContentCache = filepath.Join(manager.tempDir, "content-cache")

	return settings
}

func (manager ReleaseManager) ensureCliLayout() error {
	repositoryCache := filepath.Join(manager.tempDir, "repository-cache")
	repositoryConfig := filepath.Join(manager.tempDir, "repository", "repositories.yaml")
	registryConfig := filepath.Join(manager.tempDir, "registry", "config.json")
	contentCache := filepath.Join(manager.tempDir, "content-cache")

	if err := os.MkdirAll(repositoryCache, 0o755); err != nil {
		return fmt.Errorf("create helm repository cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(repositoryConfig), 0o755); err != nil {
		return fmt.Errorf("create helm repository config directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(registryConfig), 0o755); err != nil {
		return fmt.Errorf("create helm registry config directory: %w", err)
	}
	if err := os.MkdirAll(contentCache, 0o755); err != nil {
		return fmt.Errorf("create helm content cache: %w", err)
	}
	if err := os.WriteFile(repositoryConfig, []byte{}, 0o600); err != nil {
		return fmt.Errorf("create helm repository config: %w", err)
	}
	if err := os.WriteFile(registryConfig, []byte("{}\n"), 0o600); err != nil {
		return fmt.Errorf("create helm registry config: %w", err)
	}

	return nil
}

func valuesOrEmpty(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}

	return values
}

func timeoutForInput(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultActionTimeout
	}

	return timeout
}

func (manager ReleaseManager) restClientGetter(namespace string) helmRestClientGetter {
	return helmRestClientGetter{
		restConfig:     manager.restConfig,
		kubeconfigPath: manager.kubeconfigPath,
		namespace:      namespace,
	}
}

type helmRestClientGetter struct {
	restConfig     rest.Config
	kubeconfigPath string
	namespace      string
}

var _ genericclioptions.RESTClientGetter = helmRestClientGetter{}

func (getter helmRestClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(&getter.restConfig), nil
}

func (getter helmRestClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	restConfig, err := getter.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return discoverymemory.NewMemCacheClient(discoveryClient), nil
}

func (getter helmRestClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := getter.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	return restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient), nil
}

func (getter helmRestClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if strings.TrimSpace(getter.kubeconfigPath) != "" {
		loadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: getter.kubeconfigPath}
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{
			ClusterDefaults: clientcmd.ClusterDefaults,
			Context: clientcmdapi.Context{
				Namespace: getter.namespace,
			},
		},
	)
}
