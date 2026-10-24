package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
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
	// Upgrade permits an existing release to change; installs are always allowed.
	Upgrade bool
	// RecoverPending permits recovery of a Borq-owned operation whose process
	// ended. Callers must establish exclusive ownership before enabling it.
	RecoverPending bool
	// SkipWait lets a caller initialize sealed services before their readiness gate.
	SkipWait bool
	// Owner identifies the caller's release and prevents adoption of unrelated releases.
	Owner string
	// ResourceLabels are applied to rendered resources and included in definition comparisons.
	ResourceLabels map[string]string
	// PodLabels are applied to native Pod templates matching PodSelector.
	PodLabels map[string]string
	// PodSelector narrows the Pod templates receiving collection labels; empty selects all.
	PodSelector map[string]string
}

// ReleaseState reports persisted Helm history without exposing rendered secrets.
type ReleaseState struct {
	// Name identifies the release in its namespace.
	Name string
	// Chart identifies the installed upstream chart.
	Chart string
	// Version is the installed chart version.
	Version string
	// Revision is Helm's monotonically increasing release revision.
	Revision int
	// Status is Helm's persisted release status.
	Status string
	// Changed indicates this call created a new release revision.
	Changed bool
	// UpgradeAvailable indicates the desired pin or values differ from installation.
	UpgradeAvailable bool
}

// ReleaseInspection combines safe release state with private user-supplied Helm
// values for a caller that needs to calculate its own redacted configuration diff.
type ReleaseInspection struct {
	// State describes the installed revision without rendered manifests.
	State ReleaseState
	// Values contains the stored user overrides and may contain credentials.
	// It is deliberately excluded from JSON; callers must redact before output.
	Values map[string]any `json:"-"`
	// DefinitionChanged compares chart provenance, version and user values,
	// independently of whether a matching release is currently failed.
	DefinitionChanged bool
}

// NewReleaseManager creates the shared Helm CLI environment for a bootstrap run.
func NewReleaseManager(input NewReleaseManagerInput) (ReleaseManager, error) {
	kubeconfigPath := strings.TrimSpace(input.KubeconfigPath)
	if kubeconfigPath == "" && strings.TrimSpace(input.Connection.RestConfig().Host) == "" {
		return ReleaseManager{}, errors.New("helm requires a Kubernetes connection or kubeconfig path")
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
	input.Upgrade = true
	_, err := manager.Reconcile(ctx, input)
	return err
}

// Reconcile installs a missing release and upgrades only when explicitly enabled.
// Equal chart pins and values return the existing revision without chart downloads.
func (manager ReleaseManager) Reconcile(ctx context.Context, input InstallOrUpgradeInput) (ReleaseState, error) {
	ctx, span := otel.Tracer("borq/helm").Start(ctx, "helm.reconcile", trace.WithAttributes(attribute.String("release", input.ReleaseName), attribute.String("namespace", input.Namespace), attribute.String("chart", input.ChartRef), attribute.String("version", input.Version)))
	defer span.End()
	if err := ctx.Err(); err != nil {
		return ReleaseState{}, err
	}

	if err := validateInstallOrUpgradeInput(input); err != nil {
		return ReleaseState{}, err
	}

	settings := manager.newEnvSettings(strings.TrimSpace(input.Namespace))
	restClientGetter := manager.restClientGetter(strings.TrimSpace(input.Namespace))

	actionConfig := action.NewConfiguration()
	if err := actionConfig.Init(restClientGetter, input.Namespace, "secrets"); err != nil {
		return ReleaseState{}, fmt.Errorf("initialize helm action configuration: %w", err)
	}

	stored, err := actionConfig.Releases.Last(input.ReleaseName)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return ReleaseState{}, fmt.Errorf("lookup helm release %q: %w", input.ReleaseName, err)
	}
	var live *releasev1.Release // Helm retains release pointers in its storage API.
	if err == nil {
		live, err = releaseRecord(stored)
		if err != nil {
			return ReleaseState{}, err
		}
		state := releaseState(live)
		if input.Owner != "" && live.Labels["borq.run/owner"] != input.Owner {
			return state, fmt.Errorf("Helm release %q belongs to another owner", input.ReleaseName)
		}
		if live.Info != nil && live.Info.Status.IsPending() {
			if !input.RecoverPending {
				return state, fmt.Errorf("Helm release %q has a pending operation; its owning process must finish or recover it", input.ReleaseName)
			}
			if live.Labels["borq.run/definition"] == "" {
				return state, fmt.Errorf("cannot recover pending Helm release %q without Borq ownership", input.ReleaseName)
			}
			if err := ctx.Err(); err != nil {
				return state, err
			}
			// Keep the interrupted revision in history. The following upgrade
			// creates a new revision, so a failed retry remains inspectable.
			live.Info.Status = common.StatusFailed
			live.Info.Description = "Borq recovered an operation interrupted by its previous server process"
			if err := actionConfig.Releases.Update(live); err != nil {
				return state, fmt.Errorf("recover interrupted Helm release %q: %w", input.ReleaseName, err)
			}
			state = releaseState(live)
		}
		state.UpgradeAvailable = !sameRelease(live, input)
		if state.Status == "deployed" && (!state.UpgradeAvailable || !input.Upgrade) {
			return state, nil
		}
	}

	registryClient, err := registry.NewClient(
		registry.ClientOptWriter(io.Discard),
		registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return ReleaseState{}, fmt.Errorf("create helm registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	chartPath, chart, err := locateChart(settings, registryClient, input)
	if err != nil {
		failed, readErr := actionConfig.Releases.Last(input.ReleaseName)
		if readErr == nil {
			if record, recordErr := releaseRecord(failed); recordErr == nil {
				return releaseState(record), err
			}
		}
		return ReleaseState{}, err
	}
	if live == nil {
		err = manager.install(ctx, actionConfig, registryClient, chart, chartPath, input)
	} else {
		// Helm does not upgrade CRDs from a chart's crds/ directory. Borq
		// deliberately updates them before the controller during explicit upgrades.
		values := valuesOrEmpty(input.Values)
		if len(values) == 0 {
			values = live.Config
			if live.Info.Status != common.StatusDeployed {
				deployed, err := actionConfig.Releases.Deployed(input.ReleaseName)
				if err != nil && !errors.Is(err, driver.ErrNoDeployedReleases) {
					return ReleaseState{}, fmt.Errorf("lookup deployed Helm release: %w", err)
				}
				if err == nil {
					current, err := releaseRecord(deployed)
					if err != nil {
						return ReleaseState{}, err
					}
					values = current.Config
				}
			}
		}
		// Dependency processing mutates imported values, so Helm must receive
		// its original chart and evaluate dependency conditions only once.
		crdChart, loadErr := loader.Load(chartPath)
		if loadErr != nil {
			return ReleaseState{}, fmt.Errorf("load Helm chart for CRD upgrades: %w", loadErr)
		}
		if err := chartutil.ProcessDependencies(crdChart, values); err != nil {
			return ReleaseState{}, fmt.Errorf("process Helm chart dependencies: %w", err)
		}
		if err := manager.upgradeChartCrds(ctx, crdChart); err != nil {
			return ReleaseState{}, err
		}
		err = manager.upgrade(ctx, actionConfig, registryClient, chart, chartPath, input)
	}
	if err != nil {
		failed, readErr := actionConfig.Releases.Last(input.ReleaseName)
		if readErr == nil {
			if record, recordErr := releaseRecord(failed); recordErr == nil {
				return releaseState(record), err
			}
		}
		return ReleaseState{}, err
	}
	stored, err = actionConfig.Releases.Last(input.ReleaseName)
	if err != nil {
		return ReleaseState{}, fmt.Errorf("read reconciled helm release %q: %w", input.ReleaseName, err)
	}
	live, err = releaseRecord(stored)
	if err != nil {
		return ReleaseState{}, err
	}
	state := releaseState(live)
	state.Changed = true
	return state, nil
}

func (manager ReleaseManager) upgradeChartCrds(ctx context.Context, chart *chartv2.Chart) error {
	definitions := chart.CRDObjects()
	if len(definitions) == 0 {
		return nil
	}
	client, err := apiextensionsclient.NewForConfig(&manager.restConfig)
	if err != nil {
		return fmt.Errorf("create Helm CRD client: %w", err)
	}
	for _, definition := range definitions {
		crds, err := kubernetes.ParseCRDs(definition.File.Data)
		if err != nil {
			return fmt.Errorf("parse chart CRDs: %w", err)
		}
		for _, crd := range crds {
			encoded, err := json.Marshal(crd)
			if err != nil {
				return err
			}
			force := true // Kubernetes requires a pointer to distinguish explicit force.
			if _, err := client.ApiextensionsV1().CustomResourceDefinitions().Patch(ctx, crd.Name, types.ApplyPatchType, encoded, metav1.PatchOptions{FieldManager: "borq-platform-crds", Force: &force}); err != nil {
				return fmt.Errorf("upgrade chart CRD %s: %w", crd.Name, err)
			}
		}
	}
	return nil
}

// Inspect reads current Helm state and compares catalog pins and values without
// downloading charts, modifying Kubernetes resources, or creating revisions.
func (manager ReleaseManager) Inspect(ctx context.Context, input InstallOrUpgradeInput) (ReleaseState, error) {
	inspection, err := manager.InspectWithValues(ctx, input)
	return inspection.State, err
}

// InspectWithValues reads persisted Helm user overrides without downloading a
// chart or modifying release history. Values are private and require redaction.
func (manager ReleaseManager) InspectWithValues(ctx context.Context, input InstallOrUpgradeInput) (ReleaseInspection, error) {
	if err := ctx.Err(); err != nil {
		return ReleaseInspection{}, err
	}
	if err := validateInstallOrUpgradeInput(input); err != nil {
		return ReleaseInspection{}, err
	}
	actionConfig := action.NewConfiguration()
	if err := actionConfig.Init(manager.restClientGetter(input.Namespace), input.Namespace, "secrets"); err != nil {
		return ReleaseInspection{}, fmt.Errorf("initialize helm read configuration: %w", err)
	}
	stored, err := actionConfig.Releases.Last(input.ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return ReleaseInspection{State: ReleaseState{Name: input.ReleaseName, Status: "absent"}, Values: map[string]any{}}, nil
	}
	if err != nil {
		return ReleaseInspection{}, fmt.Errorf("read helm release %q: %w", input.ReleaseName, err)
	}
	live, err := releaseRecord(stored)
	if err != nil {
		return ReleaseInspection{}, err
	}
	state := releaseState(live)
	state.UpgradeAvailable = !sameRelease(live, input)
	return ReleaseInspection{State: state, Values: valuesOrEmpty(live.Config), DefinitionChanged: !sameReleaseDefinition(live, input)}, nil
}

func releaseRecord(stored any) (*releasev1.Release, error) {
	switch value := stored.(type) {
	case *releasev1.Release:
		return value, nil
	case releasev1.Release:
		return &value, nil // Helm's SDK requires the concrete release pointer.
	default:
		return nil, fmt.Errorf("unsupported Helm release record %T", stored)
	}
}

func releaseState(live *releasev1.Release) ReleaseState {
	state := ReleaseState{Name: live.Name, Revision: live.Version}
	if live.Chart != nil && live.Chart.Metadata != nil {
		state.Chart = live.Chart.Metadata.Name
		state.Version = live.Chart.Metadata.Version
	}
	if live.Info != nil {
		state.Status = live.Info.Status.String()
	}
	return state
}

func sameRelease(live *releasev1.Release, input InstallOrUpgradeInput) bool {
	return live.Info != nil && live.Info.Status == common.StatusDeployed && sameReleaseDefinition(live, input)
}

func sameReleaseDefinition(live *releasev1.Release, input InstallOrUpgradeInput) bool {
	if live.Chart == nil || live.Chart.Metadata == nil {
		return false
	}
	if strings.TrimPrefix(live.Chart.Metadata.Version, "v") != strings.TrimPrefix(input.Version, "v") {
		return false
	}
	if live.Labels["borq.run/definition"] != definitionHash(input) {
		return false
	}
	stored, _ := json.Marshal(valuesOrEmpty(live.Config))
	desired, _ := json.Marshal(valuesOrEmpty(input.Values))
	return string(stored) == string(desired)
}

func definitionHash(input InstallOrUpgradeInput) string {
	definition, _ := json.Marshal(struct{ Chart, Repository, Version string }{input.ChartRef, input.RepositoryUrl, input.Version})
	if input.Owner != "" || len(input.ResourceLabels)+len(input.PodLabels)+len(input.PodSelector) > 0 {
		definition, _ = json.Marshal(struct {
			Chart, Repository, Version, Owner      string
			ResourceLabels, PodLabels, PodSelector map[string]string
		}{input.ChartRef, input.RepositoryUrl, input.Version, input.Owner, input.ResourceLabels, input.PodLabels, input.PodSelector})
	}
	digest := sha256.Sum256(definition)
	return hex.EncodeToString(digest[:])[:32]
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
	// Poll live API state: Helm's watcher can retain stale InProgress CRDs
	// after they are Established, preventing post-install hooks from running.
	install.WaitStrategy = kube.LegacyStrategy
	install.WaitForJobs = true
	if input.SkipWait {
		install.WaitStrategy = kube.HookOnlyStrategy
		install.WaitForJobs = false
	}
	install.ChartPathOptions.RepoURL = strings.TrimSpace(input.RepositoryUrl)
	install.ChartPathOptions.Version = strings.TrimSpace(input.Version)
	install.SetRegistryClient(registryClient)
	install.Labels = map[string]string{"borq.run/definition": definitionHash(input)}
	if input.Owner != "" {
		install.Labels["borq.run/owner"] = input.Owner
	}
	install.PostRenderer = metadataRenderer{input: input}

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
	// Use the same live-state readiness and Job gates for installations and upgrades.
	upgrade.WaitStrategy = kube.LegacyStrategy
	upgrade.WaitForJobs = true
	if input.SkipWait {
		upgrade.WaitStrategy = kube.HookOnlyStrategy
		upgrade.WaitForJobs = false
	}
	upgrade.ChartPathOptions.RepoURL = strings.TrimSpace(input.RepositoryUrl)
	upgrade.ChartPathOptions.Version = strings.TrimSpace(input.Version)
	upgrade.SetRegistryClient(registryClient)
	upgrade.Labels = map[string]string{"borq.run/definition": definitionHash(input)}
	if input.Owner != "" {
		upgrade.Labels["borq.run/owner"] = input.Owner
	}
	upgrade.PostRenderer = metadataRenderer{input: input}

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
	if input.Version != "" && strings.TrimPrefix(chart.Metadata.Version, "v") != strings.TrimPrefix(input.Version, "v") {
		return "", nil, fmt.Errorf("chart version %q does not match requested version %q", chart.Metadata.Version, input.Version)
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
		restConfig: manager.restConfig,
		namespace:  namespace,
	}
}

type helmRestClientGetter struct {
	restConfig rest.Config
	namespace  string
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
	// Helm's builders must use the already selected connection, including its
	// kubeconfig context or in-cluster token, rather than ambient CLI settings.
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"borq": {
			Server: getter.restConfig.Host, CertificateAuthority: getter.restConfig.CAFile,
			CertificateAuthorityData: getter.restConfig.CAData, InsecureSkipTLSVerify: getter.restConfig.Insecure,
			TLSServerName: getter.restConfig.ServerName,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"borq": {
			Token: getter.restConfig.BearerToken, TokenFile: getter.restConfig.BearerTokenFile,
			ClientCertificate: getter.restConfig.CertFile, ClientCertificateData: getter.restConfig.CertData,
			ClientKey: getter.restConfig.KeyFile, ClientKeyData: getter.restConfig.KeyData,
			Username: getter.restConfig.Username, Password: getter.restConfig.Password,
			Exec: getter.restConfig.ExecProvider, AuthProvider: getter.restConfig.AuthProvider,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"borq": {Cluster: "borq", AuthInfo: "borq", Namespace: getter.namespace}},
		CurrentContext: "borq",
	}
	return clientcmd.NewDefaultClientConfig(config, &clientcmd.ConfigOverrides{})
}
