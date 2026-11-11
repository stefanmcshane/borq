package apps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/observability"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"sigs.k8s.io/yaml"
)

// Stable identities expose domain failures consistently across HTTP, CLI, and future MCP clients.
var (
	ErrTelemetryUnavailable = errors.New("application telemetry unavailable")
	ErrNotFound             = errors.New("not found")
	ErrConflict             = errors.New("conflict")
	ErrInvalid              = errors.New("invalid request")
	ErrComponentsNotReady   = errors.New("deployment target components are not ready")
)

// ClusterTailscale accepts an operator OAuth pair and retains only its private reference.
type ClusterTailscale struct {
	// ClientID is write-only and identifies this cluster's OAuth application.
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is write-only and is never persisted in SQLite or exports.
	ClientSecret string `json:"clientSecret,omitempty"`
	// CredentialsRef selects an immutable private credential revision bound to this cluster.
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// ClusterInput identifies a server-side connection; Kubernetes credentials are returned only
// through the explicit kubeconfig download endpoint.
type ClusterInput struct {
	// ID is derived by Borq and scopes installation state and cluster credentials.
	ID             string `json:"id"`
	Organization   string `json:"organization"`
	Name           string `json:"name"`
	KubeconfigPath string `json:"kubeconfigPath"`
	Context        string `json:"context"`
	InCluster      bool   `json:"inCluster"`
	// ClientApiServer preserves the API address usable by clients downloading credentials.
	ClientApiServer string `json:"clientApiServer,omitempty"`
	// Kubeconfig is accepted at registration and written to a private server file.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Reconcile distinguishes an omitted default from explicit registration-only mode.
	// The pointer is required for that distinction and is cleared before persistence.
	Reconcile *bool `json:"reconcile,omitempty"`
	// Tailscale configures this physical target; nil preserves an existing binding.
	Tailscale *ClusterTailscale `json:"tailscale,omitempty"`
	// Unaccepted management staging owns cleanup; durable tasks retain files for retries.
	tailscaleStagedRef    string
	tailscaleBaseRef      string
	tailscaleBaseCaptured bool
}

// Cluster is a registered connection, independent of the cluster hosting Borq.
type Cluster = ClusterInput

// SpaceInput creates an organization space bound to a registered cluster.
type SpaceInput struct {
	// Organization owns both the space and its cluster registration.
	Organization string `json:"organization"`
	// Name identifies the space within its organization.
	Name string `json:"name"`
	// Cluster is required and cannot change after the space is bound.
	Cluster string `json:"cluster"`
}

// Space keeps accepted desired state independent from the last successful rollout.
type Space struct {
	ID           string `json:"id"`
	Organization string `json:"organization"`
	Name         string `json:"name"`
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	// BackupStorage reports the space's independently scoped backup bucket provisioning.
	BackupStorage SpaceBackupStorage `json:"backupStorage"`
	// CertificateIssuer supplies HTTPS certificates for this space's ingresses.
	CertificateIssuer      string        `json:"certificateIssuer"`
	Revision               int           `json:"revision"`
	Configuration          Configuration `json:"configuration"`
	LastSuccessfulRevision int           `json:"lastSuccessfulRevision"`
}

// PlanInput applies a partial configuration to exactly one bound space.
type PlanInput struct {
	Organization string
	Space        string
	Patch        Patch
}

// Plan freezes both human-level configuration and the exact Kubernetes artifacts to be applied.
type Plan struct {
	ID                string                  `json:"id"`
	Space             Space                   `json:"space"`
	BaseRevision      int                     `json:"baseRevision"`
	Before            Configuration           `json:"before"`
	Configuration     Configuration           `json:"configuration"`
	ConfigurationDiff string                  `json:"configurationDiff"`
	Values            map[string]any          `json:"values"`
	RendererVersion   string                  `json:"rendererVersion"`
	Manifests         []map[string]any        `json:"manifests"`
	ResourceDiffs     []kubeapps.ResourceDiff `json:"resourceDiffs"`
	CreatedAt         time.Time               `json:"createdAt"`
}

// Operation remains queryable after a client disconnects or an application rollout fails.
type Operation struct {
	ID      string `json:"id"`
	PlanID  string `json:"planId"`
	SpaceID string `json:"spaceId"`
	Status  string `json:"status"`
	// Phase identifies environment synchronization, predeploy tasks, or application rollout.
	Phase     string                    `json:"phase"`
	Message   string                    `json:"message"`
	Resources []kubeapps.ResourceStatus `json:"resources"`
	StartedAt time.Time                 `json:"startedAt"`
	UpdatedAt time.Time                 `json:"updatedAt"`
}

type spaceRecord struct {
	Space Space `json:"space"`
	// EnvironmentGroupPrefix preserves existing default vault names when migrating legacy targets.
	EnvironmentGroupPrefix string `json:"environmentGroupPrefix,omitempty"`
	ActiveOperation        string `json:"activeOperation"`
	// LatestOperation retains unfinished predeploy requirements after terminal failures or cancellation.
	LatestOperation string                  `json:"latestOperation,omitempty"`
	Manifests       []map[string]any        `json:"manifests"`
	EnvSnapshots    []EnvSnapshot           `json:"envSnapshots"`
	EnvSelectors    map[string]EnvReference `json:"envSelectors"`
	Queue           []string                `json:"queue"`
	SqliteStorage   ManagementS3            `json:"sqliteStorage,omitempty"`
}

type planRecord struct {
	RetiredIngresses    []map[string]any        `json:"retiredIngresses,omitempty"`
	RetiredCertificates []map[string]any        `json:"retiredCertificates,omitempty"`
	DeleteApp           string                  `json:"deleteApp,omitempty"`
	DeleteManifests     []map[string]any        `json:"deleteManifests,omitempty"`
	DeleteBaseRevision  int64                   `json:"deleteBaseRevision,omitempty"`
	Registries          map[string]RegistryAuth `json:"registries,omitempty"`
	Plan                Plan                    `json:"plan"`
	Connection          Cluster                 `json:"connection"`
	Source              Patch                   `json:"source"`
	EnvironmentSecrets  []plannedEnv            `json:"environmentSecrets"`
	EnvSelectors        map[string]EnvReference `json:"envSelectors"`
	SqliteStorage       ManagementS3            `json:"sqliteStorage,omitempty"`
}

// NewInput contains only the application manager's persistence and operation settings.
type NewInput struct {
	// PrometheusQueryUrl and LokiQueryUrl override the private management query stores.
	PrometheusQueryUrl, LokiQueryUrl string
	DatabasePath                     string
	RolloutTimeout                   time.Duration
	PollInterval                     time.Duration
	// InfrastructureEnabled runs durable reconciliation for registered deployment targets.
	InfrastructureEnabled bool
	// ManagementEnabled defers target credential renewal until management setup succeeds.
	ManagementEnabled bool
	// ManagementBootstrapOnly accepts initial configuration through Kubernetes before supporting services exist.
	ManagementBootstrapOnly bool
	// ManagementKubeconfigPath is empty when Borq uses its own service account.
	ManagementKubeconfigPath string
	// PlatformSettings specifies the managed component profile and public endpoints.
	PlatformSettings platform.Settings
}

// Manager copies share one worker lifetime so server handlers cannot outlive the database.
type Manager struct {
	database sqlite.Database
	// Pointer ownership preserves cancellation and synchronization across value copies in HTTP handlers.
	runtime *managerRuntime
}

type managerRuntime struct {
	managementBootstrapOnly bool
	managementRunsMutex     sync.Mutex
	managementRuns          map[string]bool
	// Teardown cancels ordinary work while its own operation and HTTP API survive.
	lifecycleContext         context.Context
	destroyCancel            context.CancelFunc
	destroyWorkers           sync.WaitGroup
	destroying               bool
	destroyRunning           bool
	activeRequests           int
	requestsDrained          chan struct{}
	quiescenceOnce           sync.Once
	quiescenceDone           chan struct{}
	addonSlots               chan struct{}
	postgresSlots            chan struct{}
	storageMutex             sync.Mutex
	registryMutex            sync.Mutex
	secretMutex              sync.Mutex
	offsiteMutex             sync.Mutex
	backupMutex              sync.Mutex
	observability            observability.Client
	context                  context.Context
	cancel                   context.CancelFunc
	workers                  sync.WaitGroup
	mutex                    sync.Mutex
	closing                  bool
	rolloutTimeout           time.Duration
	pollInterval             time.Duration
	credentialsDir           string
	infrastructureEnabled    bool
	managementKubeconfigPath string
	platformSettings         platform.Settings
	platformMutex            sync.Mutex
	componentMutex           sync.Mutex
	componentWake            chan struct{}
	spaceStorageWake         chan struct{}
	pocketIdWake             chan struct{}
	registrationMutex        sync.Mutex
	credentialRenewalOnce    sync.Once
	networkDnsOnce           sync.Once
	deferredOperations       []Operation
	deferredQueue            []Operation
	operationsResumed        bool
	operationCancels         map[string]context.CancelFunc
}

// NewManager opens persistent state and resumes unfinished, idempotent apply operations.
func NewManager(ctx context.Context, input NewInput) (Manager, error) {
	if _, err := os.Stat(filepath.Join(filepath.Dir(input.DatabasePath), "RECOVERY_REQUIRED")); err == nil {
		return Manager{}, errors.New("restored management state requires offline review and storage/vault rebinding before explicit local resume")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manager{}, errors.New("inspect management recovery barrier")
	}
	if input.ManagementEnabled && !input.InfrastructureEnabled {
		return Manager{}, errors.New("management setup requires infrastructure management to be enabled")
	}
	if input.InfrastructureEnabled {
		var settingsErr error
		if input.ManagementBootstrapOnly {
			settingsErr = platform.ValidateBootstrapSettings(input.PlatformSettings)
		} else {
			settingsErr = platform.ValidateServerSettings(input.PlatformSettings)
		}
		if settingsErr != nil {
			return Manager{}, fmt.Errorf("validate infrastructure settings: %w", settingsErr)
		}
		if _, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.ManagementKubeconfigPath}, logger.Logger{}); err != nil {
			return Manager{}, fmt.Errorf("load management cluster connection: %w", err)
		}
	}
	database, err := sqlite.New(ctx, input.DatabasePath)
	if err != nil {
		return Manager{}, err
	}
	if input.RolloutTimeout <= 0 {
		input.RolloutTimeout = 10 * time.Minute
	}
	if input.PollInterval <= 0 {
		input.PollInterval = time.Second
	}
	telemetry := observability.Client{PrometheusUrl: input.PrometheusQueryUrl, LokiUrl: input.LokiQueryUrl}
	if input.InfrastructureEnabled {
		managementTelemetry, err := observability.NewManagementClient(input.ManagementKubeconfigPath, input.PlatformSettings.Namespace)
		if err != nil {
			_ = database.Close()
			return Manager{}, err
		}
		telemetry = managementTelemetry
		// Explicit private endpoints use their own transport and never receive Kubernetes credentials.
		if input.PrometheusQueryUrl != "" {
			telemetry.PrometheusUrl = input.PrometheusQueryUrl
		}
		if input.LokiQueryUrl != "" {
			telemetry.LokiUrl = input.LokiQueryUrl
		}
	}
	workerContext, cancel := context.WithCancel(ctx)
	manager := Manager{database: database, runtime: &managerRuntime{
		lifecycleContext: ctx,
		quiescenceDone:   make(chan struct{}),
		addonSlots:       make(chan struct{}, 3),
		postgresSlots:    make(chan struct{}, 3),
		context:          workerContext, cancel: cancel,
		observability:  telemetry,
		rolloutTimeout: input.RolloutTimeout, pollInterval: input.PollInterval,
		credentialsDir:           filepath.Join(filepath.Dir(input.DatabasePath), "clusters"),
		infrastructureEnabled:    input.InfrastructureEnabled,
		managementKubeconfigPath: input.ManagementKubeconfigPath,
		managementBootstrapOnly:  input.ManagementBootstrapOnly,
		managementRuns:           make(map[string]bool),
		platformSettings:         input.PlatformSettings,
		componentWake:            make(chan struct{}, 1),
		spaceStorageWake:         make(chan struct{}, 1),
		pocketIdWake:             make(chan struct{}, 1),
	}}
	if active, err := manager.initializeManagementDestroy(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("load management teardown: %w", err)
	} else if active {
		return manager, nil
	}
	if err := manager.migrateLegacyHierarchy(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("migrate organizations and spaces: %w", err)
	}
	if err := manager.migrateEnvironmentGroups(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("migrate secrets: %w", err)
	}
	if err := manager.retireUnusedManagementSpace(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("retire unused management space: %w", err)
	}
	if err := manager.migrateSpaceStorage(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("migrate space backup storage: %w", err)
	}
	operations, err := database.List(ctx, "operation")
	if err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, err
	}
	pending := []Operation{}
	queued := []Operation{}
	for _, record := range operations {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			cancel()
			_ = database.Close()
			return Manager{}, fmt.Errorf("read unfinished operation: %w", err)
		}
		if operation.Status == "queued" {
			queued = append(queued, operation)
			continue
		}
		if operationFinished(operation.Status) {
			continue
		}
		pending = append(pending, operation)
	}
	manager.runtime.deferredOperations, manager.runtime.deferredQueue = pending, queued
	if !input.ManagementEnabled {
		if err := manager.resumeOperationWorkers(ctx); err != nil {
			cancel()
			_ = database.Close()
			return Manager{}, err
		}
	}
	if input.InfrastructureEnabled && !input.ManagementEnabled {
		manager.startCredentialRenewal()
	}
	if input.ManagementEnabled && input.ManagementBootstrapOnly {
		if err := manager.resumeManagementOperations(ctx); err != nil {
			cancel()
			_ = database.Close()
			return Manager{}, err
		}
	}
	return manager, nil
}

func (manager Manager) resumeOperationWorkers(ctx context.Context) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing || manager.runtime.destroying {
		return errors.New("application manager is closing")
	}
	if manager.runtime.operationsResumed {
		return nil
	}
	resumed, err := manager.resumeQueueHeads(ctx, manager.runtime.deferredQueue)
	if err != nil {
		return err
	}
	pending := append(manager.runtime.deferredOperations, resumed...)
	for _, operation := range pending {
		current, err := manager.GetOperation(ctx, operation.ID)
		if err != nil {
			return err
		}
		if !operationFinished(current.Status) {
			manager.startOperation(current)
		}
	}
	manager.runtime.deferredOperations, manager.runtime.deferredQueue = nil, nil
	if err := manager.resumeAddonOperations(ctx); err != nil {
		return err
	}
	if err := manager.resumeManagementOperations(ctx); err != nil {
		return err
	}
	if err := manager.resumeSpacePocketIdOperations(ctx); err != nil {
		return err
	}
	if err := manager.resumePostgresOperations(ctx); err != nil {
		return err
	}
	manager.startOffsiteWorker()
	manager.startManagementBackupWorker()
	manager.startSpaceStorageWorker()
	manager.startPocketIdWorker()
	manager.runtime.operationsResumed = true
	return nil
}

// Close stops operation workers before closing their durable state.
func (manager Manager) Close() error {
	manager.runtime.mutex.Lock()
	manager.runtime.closing = true
	manager.runtime.cancel()
	if manager.runtime.destroyCancel != nil {
		manager.runtime.destroyCancel()
	}
	manager.runtime.mutex.Unlock()
	manager.runtime.workers.Wait()
	manager.runtime.destroyWorkers.Wait()
	return manager.database.Close()
}

// CheckHealth verifies the local durable state independently of managed cluster availability.
func (manager Manager) CheckHealth(ctx context.Context) error {
	return manager.database.Ping(ctx)
}

// RegisterCluster uses the same registration path for the server's own cluster and remote clusters.
func (manager Manager) RegisterCluster(ctx context.Context, input ClusterInput) (Cluster, error) {
	manager.runtime.registrationMutex.Lock()
	defer manager.runtime.registrationMutex.Unlock()
	return manager.registerCluster(ctx, input, nil)
}

// registerCluster commits any accompanying space writes with the registration.
// The caller holds registrationMutex across name allocation and persistence.
func (manager Manager) registerCluster(ctx context.Context, input ClusterInput, writes []sqlite.Write) (Cluster, error) {
	if err := validateClusterCredentialAuthority(ctx, input); err != nil {
		return Cluster{}, err
	}
	reconcile := manager.runtime.infrastructureEnabled && (input.Reconcile == nil || *input.Reconcile)
	input.Reconcile = nil
	if err := validateNames(input.Organization, input.Name); err != nil {
		return Cluster{}, err
	}
	if err := validateClientApiServer(input.ClientApiServer); err != nil {
		return Cluster{}, err
	}
	if err := validateClusterTailscale(input); err != nil {
		return Cluster{}, err
	}
	if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", input.Organization); err != nil {
		return Cluster{}, err
	}
	input.ID = platform.ClusterID(input.Organization, input.Name)
	platformSettings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return Cluster{}, err
	}
	if input.Kubeconfig != "" {
		if input.InCluster || input.KubeconfigPath != "" {
			return Cluster{}, fmt.Errorf("%w: supply one kubeconfig source or in-cluster credentials", ErrInvalid)
		}
		if err := os.MkdirAll(manager.runtime.credentialsDir, 0o700); err != nil {
			return Cluster{}, fmt.Errorf("create private cluster directory: %w", err)
		}
		input.KubeconfigPath = filepath.Join(manager.runtime.credentialsDir, input.ID+".yaml")
		// Credentials stay private except through the explicit kubeconfig download.
		created, err := storeClusterKubeconfig(input.KubeconfigPath, input.Kubeconfig)
		if err != nil {
			return Cluster{}, err
		}
		if created {
			// Failed validation must not leave an immutable, unusable registration file.
			path := input.KubeconfigPath
			defer func() {
				if _, _, err := loadDocument[Cluster](context.Background(), manager.database, "cluster", input.Organization+"/"+input.Name); errors.Is(err, ErrNotFound) {
					_ = os.Remove(path)
				}
			}()
		}
		input.Kubeconfig = ""
	}
	client, err := newClusterClient(input)
	if err != nil {
		return Cluster{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := client.Validate(ctx); err != nil {
		return Cluster{}, fmt.Errorf("%w: cannot connect to cluster: %v", ErrInvalid, err)
	}
	if manager.runtime.infrastructureEnabled {
		err := platform.CheckTargetOwnership(ctx, platform.TargetInput{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster, Organization: input.Organization, Name: input.Name, ID: input.ID, Settings: platformSettings})
		if errors.Is(err, platform.ErrTargetOwnership) {
			return Cluster{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		if err != nil {
			return Cluster{}, err
		}
	}
	key := input.Organization + "/" + input.Name
	existing, previous, err := loadDocument[Cluster](ctx, manager.database, "cluster", key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Cluster{}, err
	}
	exists := err == nil
	if exists {
		existing.ID = input.ID
	}
	input, createdRef, err := manager.stageClusterTailscale(input, existing)
	if err != nil {
		return Cluster{}, err
	}
	if createdRef != "" {
		defer manager.cleanClusterTailscale(input, createdRef)
	}
	if reconcile || (input.Tailscale != nil && platformSettings.Network.Tailscale != nil) {
		settings, converged, err := manager.clusterPlatformSettings(ctx, input, platformSettings)
		if err != nil {
			return Cluster{}, err
		}
		if settings.Network.Tailscale != nil {
			if _, err := manager.clusterOperatorCredentials(ctx, input, converged); err != nil {
				return Cluster{}, err
			}
		}
	}
	changed := !exists
	if exists {
		storedClientApiServer := existing.ClientApiServer
		if input.ClientApiServer == "" {
			input.ClientApiServer = storedClientApiServer
		}
		existing.ClientApiServer = input.ClientApiServer
		wantedConnection, existingConnection := input, existing
		wantedConnection.Tailscale, existingConnection.Tailscale = nil, nil
		wantedConnection.tailscaleStagedRef, existingConnection.tailscaleStagedRef = "", ""
		wantedConnection.tailscaleBaseRef, existingConnection.tailscaleBaseRef = "", ""
		wantedConnection.tailscaleBaseCaptured, existingConnection.tailscaleBaseCaptured = false, false
		if existingConnection != wantedConnection {
			return Cluster{}, fmt.Errorf("%w: cluster is already registered with different connection settings", ErrConflict)
		}
		previousRef, currentRef := "", ""
		if existing.Tailscale != nil {
			previousRef = existing.Tailscale.CredentialsRef
		}
		if input.Tailscale != nil {
			currentRef = input.Tailscale.CredentialsRef
		}
		changed = storedClientApiServer != input.ClientApiServer || previousRef != currentRef
	}
	if changed {
		write, err := documentWrite("cluster", key, input, previous.Revision)
		if err != nil {
			return Cluster{}, err
		}
		writes = append(writes, write)
	}
	// Persist the registration and reconciliation request together. No
	// target installation belongs in this request; inline credentials are already private.
	if err := manager.commitClusterRegistration(ctx, input, writes, reconcile); err != nil {
		return Cluster{}, err
	}
	return input, nil
}

// Plan renders the complete space and freezes its live comparison without mutating Kubernetes.
func (manager Manager) Plan(ctx context.Context, input PlanInput) (Plan, error) {
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space",
		spaceKey(input.Organization, input.Space))
	if err != nil {
		return Plan{}, err
	}
	if stored.ActiveOperation != "" {
		operation, err := manager.GetOperation(ctx, stored.ActiveOperation)
		if err != nil {
			return Plan{}, err
		}
		if operation.Phase == "deleting" {
			return Plan{}, fmt.Errorf("%w: wait for application deletion to finish before planning a deployment", ErrConflict)
		}
	}
	if stored.Space.Cluster == "" {
		return Plan{}, fmt.Errorf("%w: space %s is not attached to a cluster", ErrInvalid, input.Space)
	}
	stored.Space.CertificateIssuer, err = normalizeCertificateIssuer(stored.Space.CertificateIssuer)
	if err != nil {
		return Plan{}, err
	}
	connection, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", input.Organization+"/"+stored.Space.Cluster)
	if err != nil {
		return Plan{}, err
	}
	if manager.runtime.infrastructureEnabled {
		installation, err := manager.GetClusterComponents(ctx, input.Organization, stored.Space.Cluster)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Plan{}, err
		}
		if installation.Status != "ready" {
			return Plan{}, errors.Join(ErrConflict, ErrComponentsNotReady)
		}
	}
	client, err := newClusterClient(connection)
	if err != nil {
		return Plan{}, err
	}
	id, err := newId()
	if err != nil {
		return Plan{}, err
	}
	configuration, secrets, selectors, err := manager.prepareEnvironment(ctx, client, stored, input.Patch)
	if err != nil {
		return Plan{}, err
	}
	var predeployApps []string
	if input.Patch.Apps != nil {
		predeployApps = make([]string, 0, len(*input.Patch.Apps))
		for _, application := range *input.Patch.Apps {
			predeployApps = append(predeployApps, application.Name)
		}
		latest, err := manager.latestAcceptedOperation(ctx, stored)
		if err != nil {
			return Plan{}, err
		}
		// Desired state survives termination, so its unfinished barriers must too.
		for _, operationID := range append([]string{latest, stored.ActiveOperation}, stored.Queue...) {
			if operationID == "" {
				continue
			}
			operation, err := manager.GetOperation(ctx, operationID)
			if err != nil {
				return Plan{}, err
			}
			if operation.Phase == "applications" || operation.Phase == "deleting" {
				continue
			}
			previous, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
			if err != nil {
				return Plan{}, err
			}
			jobs, _ := splitPlanManifests(previous.Plan.Manifests)
			for _, job := range jobs {
				metadata, _ := job["metadata"].(map[string]any)
				labels, _ := metadata["labels"].(map[string]any)
				name, _ := labels["app.kubernetes.io/name"].(string)
				predeployApps = append(predeployApps, name)
			}
		}
	}
	sqliteStorage, err := manager.prepareSqliteStorage(ctx, configuration, stored)
	if err != nil {
		return Plan{}, err
	}
	tunnelDomain, err := manager.applicationTunnelDomain(ctx, stored.Space)
	if err != nil {
		return Plan{}, err
	}
	rendered, err := renderWithEnv(configuration, stored.Space.Namespace, stored.Space.ID, id, stored.Space.CertificateIssuer, envSnapshots(secrets), ResourceScope{Organization: stored.Space.Organization, Space: stored.Space.Name, Cluster: stored.Space.Cluster, tunnelDomain: tunnelDomain}, predeployApps, sqliteStorage)
	if err != nil {
		return Plan{}, fmt.Errorf("render apps: %w", err)
	}
	registries, err := manager.readRegistries(ctx, stored.Space.ID)
	if err != nil {
		return Plan{}, err
	}
	addRegistryPullSecrets(rendered.Manifests, registries)
	rendered.Manifests = append(rendered.Manifests, registryExternalSecrets(registries, stored.Space.Namespace, stored.Space.ID, "borq-registry-openbao")...)
	rendered.Manifests = preserveUnselectedRegistryPullSecrets(rendered.Manifests, stored.Manifests, input.Patch)
	rendered.Manifests, err = manager.omitDeletedRegistryProjections(ctx, stored.Space.ID, rendered.Manifests)
	if err != nil {
		return Plan{}, err
	}
	if len(registries) > 0 {
		rendered.Values["registries"] = registries
	}
	retained, retiredIngresses, err := manager.captureApplicationIngressRetirements(ctx, stored, connection, rendered.Manifests)
	if err != nil {
		return Plan{}, err
	}
	retained, retiredCertificates := applicationRetiredCertificates(retained, rendered.Manifests)
	retiredCertificates, err = manager.captureApplicationCertificateRetirements(ctx, stored, connection, retiredCertificates, rendered.Manifests)
	if err != nil {
		return Plan{}, err
	}
	if err := rejectResourceDeletions(retained, rendered.Manifests); err != nil {
		return Plan{}, err
	}
	differences, err := client.Observe(ctx, rendered.Manifests, stored.Space.ID)
	if err != nil {
		if errors.Is(err, kubeapps.ErrOwnershipConflict) {
			return Plan{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return Plan{}, fmt.Errorf("compare live resources: %w", err)
	}
	configurationDiff, err := diffConfiguration(stored.Space.Configuration, configuration)
	if err != nil {
		return Plan{}, err
	}
	for _, manifest := range retiredIngresses {
		differences = append(differences, kubeapps.ResourceDiff{ApiVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: stored.Space.Namespace, Name: manifestName(manifest), Action: "delete", Live: manifest})
	}
	plan := Plan{
		ID: id, Space: stored.Space, BaseRevision: stored.Space.Revision,
		Before: stored.Space.Configuration, Configuration: configuration, ConfigurationDiff: configurationDiff,
		Values: rendered.Values, Manifests: rendered.Manifests, RendererVersion: rendered.RendererVersion,
		ResourceDiffs: differences, CreatedAt: time.Now().UTC(),
	}
	write, err := documentWrite("plan", id, planRecord{RetiredIngresses: retiredIngresses, RetiredCertificates: retiredCertificates, Registries: registries, Plan: plan, Connection: connection, Source: input.Patch, EnvironmentSecrets: secrets, EnvSelectors: selectors, SqliteStorage: sqliteStorage}, 0)
	if err != nil {
		return Plan{}, err
	}
	return plan, manager.database.Commit(ctx, []sqlite.Write{write})
}

// GetPlan returns the saved artifacts, never a fresh rendering under a newer template.
func (manager Manager) GetPlan(ctx context.Context, id string) (Plan, error) {
	record, _, err := loadDocument[planRecord](ctx, manager.database, "plan", id)
	return record.Plan, err
}

// Apply accepts an exact saved plan, superseding unfinished work in its environment.
func (manager Manager) Apply(ctx context.Context, id string) (Operation, error) {
	manager.runtime.mutex.Lock()
	closing := manager.runtime.closing
	manager.runtime.mutex.Unlock()
	if closing {
		return Operation{}, errors.New("application manager is stopping")
	}
	previous, _, err := loadDocument[Operation](ctx, manager.database, "operation", id)
	if err == nil {
		return previous, nil // Polling or retrying an accepted plan never applies it again.
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", id)
	if err != nil {
		return Operation{}, err
	}
	client, err := newClusterClient(saved.Connection)
	if err != nil {
		return Operation{}, err
	}
	// Validate before reserving a revision. Queue execution checks again when its turn arrives.
	if err := manager.verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
		return Operation{}, err
	}
	registryManifests, _ := splitRegistryManifests(saved.Plan.Manifests)
	manager.runtime.secretMutex.Lock()
	err = manager.verifyRegistryManifests(ctx, saved.Plan.Space.ID, registryManifests)
	manager.runtime.secretMutex.Unlock()
	if err != nil {
		return Operation{}, err
	}
	return manager.enqueuePlan(ctx, id)
}

// GetOperation is the polling boundary used by apply --wait and future MCP clients.
func (manager Manager) GetOperation(ctx context.Context, id string) (Operation, error) {
	operation, _, err := loadDocument[Operation](ctx, manager.database, "operation", id)
	if err == nil && operation.Phase == "" {
		operation.Phase = "applications" // Older completed operations predate predeploy support.
	}
	return operation, err
}

func (manager Manager) startOperation(operation Operation) {
	// Callers hold the runtime mutex, so cancellation is registered before acceptance returns.
	ctx, cancel := context.WithDeadline(manager.runtime.context, manager.operationDeadline(operation))
	if manager.runtime.operationCancels == nil {
		manager.runtime.operationCancels = map[string]context.CancelFunc{}
	}
	manager.runtime.operationCancels[operation.ID] = cancel
	if operation.Status == "canceling" {
		cancel()
	}
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		defer cancel()
		defer func() {
			manager.runtime.mutex.Lock()
			delete(manager.runtime.operationCancels, operation.ID)
			manager.runtime.mutex.Unlock()
		}()
		manager.runOperation(ctx, operation)
	}()
}

func (manager Manager) runOperation(ctx context.Context, operation Operation) {
	// Load saved jobs even after cancellation so recovery can stop them.
	saved, _, err := loadDocument[planRecord](manager.runtime.context, manager.database, "plan", operation.PlanID)
	if err != nil {
		manager.finishOperation(operation, "failed", err.Error())
		return
	}
	if saved.DeleteApp != "" {
		manager.runAppDeletion(ctx, operation, saved)
		return
	}
	if saved.SqliteStorage.Bucket != "" {
		current, err := manager.currentSqliteStorage(manager.runtime.context, saved.Plan.Space)
		if err != nil || current != saved.SqliteStorage {
			manager.finishOperation(operation, "failed", "SQLite storage changed; create a fresh deployment plan before retrying.")
			return
		}
	}
	client, err := newClusterClient(saved.Connection)
	if err != nil {
		manager.finishOperation(operation, "failed", err.Error())
		return
	}
	registryManifests, remaining := splitRegistryManifests(saved.Plan.Manifests)
	secretManifests, remaining := splitEnvironmentManifests(remaining)
	sqliteManifests, remaining := splitSqliteManifests(remaining)
	jobs, manifests := splitPlanManifests(remaining)
	if operation.Phase == "" {
		// Operations accepted before phases were added remain resumable.
		operation.Phase = "applications"
		if len(jobs) > 0 {
			operation.Phase = "predeploy"
		}
	}
	if err := manager.verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	namespace, err := plannedNamespace(saved.Plan)
	if err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	operation.Status, operation.Message = "applying", "preparing environment namespace"
	err = manager.saveOperation(ctx, operation)
	if err == nil {
		err = client.EnsureNamespace(ctx, namespace)
	}
	if err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	if err := manager.applyRegistryManifests(ctx, client, saved, registryManifests); err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	if operation.Phase == "secrets" {
		operation, err = manager.runEnvironment(ctx, operation, client, secretManifests, len(jobs) > 0)
		if err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
	}
	if err := client.Apply(ctx, sqliteManifests, saved.Plan.Space.ID); err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	if err := manager.prepareSqliteRuntime(ctx, client, saved); err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	if operation.Phase == "predeploy" {
		operation, err = manager.runPredeploy(ctx, operation, client, jobs, saved.EnvironmentSecrets, saved.Plan.Space.ID)
		if err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
	}
	// A predeploy task or server restart may have outlived the pinned version's deployment window.
	if err := manager.verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	operation.Status, operation.Message = "applying", "applying saved application manifests"
	err = manager.saveOperation(ctx, operation)
	if err == nil {
		err = client.Apply(ctx, manifests, saved.Plan.Space.ID)
	}
	if err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	operation, err = manager.waitForApplications(ctx, operation, client, manifests, saved.Plan.Space.ID)
	if err != nil {
		manager.failOperation(operation, client, jobs, err)
		return
	}
	if operation.Status == "succeeded" {
		if err := manager.retireApplicationIngresses(ctx, saved); err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
		if err := manager.retireApplicationCertificates(ctx, saved); err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
	}
	manager.finishOperation(operation, operation.Status, operation.Message)
}

func (manager Manager) runPredeploy(ctx context.Context, operation Operation, client kubeapps.Client, jobs []map[string]any, entries []plannedEnv, targetId string) (Operation, error) {
	completed := []kubeapps.ResourceStatus{}
	for _, resource := range operation.Resources {
		if resource.Kind == "ExternalSecret" {
			completed = append(completed, resource)
		}
	}
	for _, job := range jobs {
		if err := manager.verifyEnvironment(ctx, client, entries); err != nil {
			return operation, err
		}
		operation.Status, operation.Message = "applying", "starting predeploy Job "+manifestName(job)
		if err := manager.saveOperation(ctx, operation); err != nil {
			return operation, err
		}
		if err := client.StartJob(ctx, job, targetId); err != nil {
			return operation, err
		}
		for {
			status, err := client.JobStatus(ctx, job, targetId)
			if ctx.Err() != nil {
				return operation, ctx.Err()
			}
			operation.Status = "waiting"
			if err != nil {
				operation.Message = "waiting for predeploy Job status: " + err.Error()
			} else {
				operation.Resources = append(append([]kubeapps.ResourceStatus{}, completed...), status)
				operation.Message = "predeploy Job " + status.Name + ": " + status.Message
				if status.State == "failed" {
					return operation, errors.New(operation.Message)
				}
				if status.State == "succeeded" {
					completed = append(completed, status)
					break
				}
			}
			if err := manager.saveOperation(ctx, operation); err != nil {
				return operation, err
			}
			if err := manager.waitForPoll(ctx); err != nil {
				return operation, err
			}
		}
	}
	// Checkpoint the barrier before any Deployment changes. Recovery can skip
	// completed Jobs even if those historical resources are later cleaned up.
	operation.Phase, operation.Resources = "applications", completed
	operation.Status, operation.Message = "applying", "all predeploy Jobs completed"
	return operation, manager.saveOperation(ctx, operation)
}

func (manager Manager) waitForApplications(ctx context.Context, operation Operation, client kubeapps.Client, manifests []map[string]any, targetId string) (Operation, error) {
	completed := []kubeapps.ResourceStatus{}
	for _, resource := range operation.Resources {
		if resource.Kind == "Job" || resource.Kind == "ExternalSecret" {
			completed = append(completed, resource)
		}
	}
	for {
		status, err := client.Status(ctx, manifests, targetId)
		if ctx.Err() != nil {
			return operation, ctx.Err()
		}
		if err == nil {
			operation.Resources = append(append([]kubeapps.ResourceStatus{}, completed...), status.Resources...)
			if status.State == "succeeded" || status.State == "failed" {
				operation.Status, operation.Message = status.State, status.Message
				return operation, nil
			}
			operation.Status, operation.Message = "waiting", status.Message
		} else {
			operation.Status, operation.Message = "waiting", "waiting for Kubernetes status: "+err.Error()
		}
		if err := manager.saveOperation(ctx, operation); err != nil {
			return operation, err
		}
		if err := manager.waitForPoll(ctx); err != nil {
			return operation, err
		}
	}
}

func (manager Manager) waitForPoll(ctx context.Context) error {
	timer := time.NewTimer(manager.runtime.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (manager Manager) failOperation(operation Operation, client kubeapps.Client, jobs []map[string]any, err error) {
	if manager.runtime.context.Err() != nil {
		return // Graceful shutdown leaves active Jobs running for recovery.
	}
	message := err.Error()
	status := "failed"
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(message, "context deadline") {
		message = "rollout deadline exceeded: " + operation.Message
	}
	state, _, stateError := loadDocument[operationState](manager.runtime.context, manager.database, "operation-state", operation.ID)
	if stateError == nil && state.StopStatus != "" {
		status, message = state.StopStatus, state.StopMessage
	}
	if operation.Phase == "predeploy" {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, job := range jobs {
			if stopError := client.StopJob(cleanup, job, operation.SpaceID); stopError != nil {
				message += "; stop predeploy Job " + manifestName(job) + ": " + stopError.Error()
			}
		}
	}
	manager.finishOperation(operation, status, message)
}

func (manager Manager) saveOperation(ctx context.Context, operation Operation) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	current, record, err := loadDocument[Operation](ctx, manager.database, "operation", operation.ID)
	if err != nil {
		return err
	}
	if current.Status == "canceling" || operationFinished(current.Status) {
		return context.Canceled
	}
	operation.UpdatedAt = time.Now().UTC()
	write, err := documentWrite("operation", operation.ID, operation, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) finishOperation(operation Operation, status, message string) {
	for {
		if err := manager.completeQueuedOperation(operation, status, message); err == nil {
			return
		}
		// A transient database failure must not abandon the active slot and stall its queue.
		// Shutdown leaves the durable operation unfinished so startup can resume it.
		if err := manager.waitForPoll(manager.runtime.context); err != nil {
			return
		}
	}
}

func newClusterClient(cluster Cluster) (kubeapps.Client, error) {
	return kubeapps.New(kubeapps.NewInput{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster})
}

func loadDocument[Value any](ctx context.Context, database sqlite.Database, collection, key string) (Value, sqlite.Record, error) {
	var value Value
	record, err := database.Get(ctx, collection, key)
	if err != nil {
		return value, sqlite.Record{}, translateStoreError(err)
	}

	if err := json.Unmarshal(record.Value, &value); err != nil {
		return value, sqlite.Record{}, fmt.Errorf("decode %s document: %w", collection, err)
	}
	return value, record, nil
}

func documentWrite(collection, key string, value any, revision int64) (sqlite.Write, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return sqlite.Write{}, fmt.Errorf("encode %s document: %w", collection, err)
	}
	return sqlite.Write{Collection: collection, Key: key, Value: encoded, ExpectedRevision: revision}, nil
}

func translateStoreError(err error) error {
	if errors.Is(err, sqlite.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, sqlite.ErrConflict) {
		return ErrConflict
	}
	return err
}

func validateNames(names ...string) error {
	pattern := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	for _, name := range names {
		if len(name) > 63 || !pattern.MatchString(name) {
			return fmt.Errorf("%w: names must be 1–63 lowercase letters, digits, or hyphens, starting and ending with a letter or digit", ErrInvalid)
		}
	}
	return nil
}

func spaceKey(organization, space string) string {
	return strings.Join([]string{organization, space}, "/")
}

func stableId(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func newId() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("allocate plan identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func diffConfiguration(before, after Configuration) (string, error) {
	previous, err := yaml.Marshal(before)
	if err != nil {
		return "", err
	}
	next, err := yaml.Marshal(after)
	if err != nil {
		return "", err
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(string(previous)), B: difflib.SplitLines(string(next)),
		FromFile: "stored apps", ToFile: "planned apps", Context: 3,
	})
}

func rejectResourceDeletions(previous, next []map[string]any) error {
	identities := map[string]bool{}
	for _, manifest := range next {
		identities[manifestIdentity(manifest)] = true
	}
	for _, manifest := range previous {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if isPredeployJob(manifest) || manifest["kind"] == "ExternalSecret" || manifest["kind"] == "ConfigMap" && labels["borq.run/phase"] == "sqlite" {
			continue // Retain historical Jobs, environment snapshots and backup configuration.
		}
		if !identities[manifestIdentity(manifest)] {
			return fmt.Errorf("%w: resource deletion is deferred; update would remove %s", ErrInvalid, manifestIdentity(manifest))
		}
	}
	return nil
}

func splitPlanManifests(manifests []map[string]any) ([]map[string]any, []map[string]any) {
	jobs, applications := []map[string]any{}, []map[string]any{}
	for _, manifest := range manifests {
		if isPredeployJob(manifest) {
			jobs = append(jobs, manifest)
			continue
		}
		applications = append(applications, manifest)
	}
	return jobs, applications
}

func isPredeployJob(manifest map[string]any) bool {
	metadata, _ := manifest["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	return manifest["apiVersion"] == "batch/v1" && manifest["kind"] == "Job" && labels["borq.run/phase"] == "predeploy"
}

func manifestName(manifest map[string]any) string {
	metadata, _ := manifest["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

func manifestIdentity(manifest map[string]any) string {
	metadata, _ := manifest["metadata"].(map[string]any)
	return fmt.Sprintf("%v/%v/%v/%v", manifest["apiVersion"], manifest["kind"], metadata["namespace"], metadata["name"])
}
