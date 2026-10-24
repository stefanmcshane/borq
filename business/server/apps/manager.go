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
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"sigs.k8s.io/yaml"
)

// Stable identities expose domain failures consistently across HTTP, CLI, and future MCP clients.
var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrInvalid            = errors.New("invalid request")
	ErrComponentsNotReady = errors.New("deployment target components are not ready")
)

// ClusterInput identifies a server-side connection; credentials are returned only
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
	EnvironmentGroupPrefix string                  `json:"environmentGroupPrefix,omitempty"`
	ActiveOperation        string                  `json:"activeOperation"`
	Manifests              []map[string]any        `json:"manifests"`
	EnvSnapshots           []EnvSnapshot           `json:"envSnapshots"`
	EnvSelectors           map[string]EnvReference `json:"envSelectors"`
	Queue                  []string                `json:"queue"`
}

type planRecord struct {
	Plan               Plan                    `json:"plan"`
	Connection         Cluster                 `json:"connection"`
	Source             Patch                   `json:"source"`
	EnvironmentSecrets []plannedEnv            `json:"environmentSecrets"`
	EnvSelectors       map[string]EnvReference `json:"envSelectors"`
}

// NewInput contains only the application manager's persistence and operation settings.
type NewInput struct {
	DatabasePath   string
	RolloutTimeout time.Duration
	PollInterval   time.Duration
	// InfrastructureEnabled runs durable reconciliation for registered deployment targets.
	InfrastructureEnabled bool
	// ManagementEnabled defers target credential renewal until management setup succeeds.
	ManagementEnabled bool
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
	registrationMutex        sync.Mutex
	credentialRenewalOnce    sync.Once
	deferredOperations       []Operation
	deferredQueue            []Operation
	operationsResumed        bool
}

// NewManager opens persistent state and resumes unfinished, idempotent apply operations.
func NewManager(ctx context.Context, input NewInput) (Manager, error) {
	if input.ManagementEnabled && !input.InfrastructureEnabled {
		return Manager{}, errors.New("management setup requires infrastructure management to be enabled")
	}
	if input.InfrastructureEnabled {
		if err := platform.ValidateServerSettings(input.PlatformSettings); err != nil {
			return Manager{}, fmt.Errorf("validate infrastructure settings: %w", err)
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
	workerContext, cancel := context.WithCancel(ctx)
	manager := Manager{database: database, runtime: &managerRuntime{
		context: workerContext, cancel: cancel,
		rolloutTimeout: input.RolloutTimeout, pollInterval: input.PollInterval,
		credentialsDir:           filepath.Join(filepath.Dir(input.DatabasePath), "clusters"),
		infrastructureEnabled:    input.InfrastructureEnabled,
		managementKubeconfigPath: input.ManagementKubeconfigPath,
		platformSettings:         input.PlatformSettings,
		componentWake:            make(chan struct{}, 1),
	}}
	if err := manager.migrateLegacyHierarchy(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("migrate organizations and spaces: %w", err)
	}
	if err := manager.migrateEnvironmentGroups(ctx); err != nil {
		cancel()
		_ = database.Close()
		return Manager{}, fmt.Errorf("migrate environment groups: %w", err)
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
		if operation.Status == "succeeded" || operation.Status == "failed" {
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
	return manager, nil
}

func (manager Manager) resumeOperationWorkers(ctx context.Context) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
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
		manager.startOperation(operation)
	}
	manager.runtime.deferredOperations, manager.runtime.deferredQueue = nil, nil
	manager.runtime.operationsResumed = true
	return nil
}

// Close stops operation workers before closing their durable state.
func (manager Manager) Close() error {
	manager.runtime.mutex.Lock()
	manager.runtime.closing = true
	manager.runtime.cancel()
	manager.runtime.mutex.Unlock()
	manager.runtime.workers.Wait()
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
	reconcile := manager.runtime.infrastructureEnabled && (input.Reconcile == nil || *input.Reconcile)
	input.Reconcile = nil
	if err := validateNames(input.Organization, input.Name); err != nil {
		return Cluster{}, err
	}
	if err := validateClientApiServer(input.ClientApiServer); err != nil {
		return Cluster{}, err
	}
	if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", input.Organization); err != nil {
		return Cluster{}, err
	}
	input.ID = platform.ClusterID(input.Organization, input.Name)
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
		err := platform.CheckTargetOwnership(ctx, platform.TargetInput{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster, Organization: input.Organization, Name: input.Name, ID: input.ID, Settings: manager.runtime.platformSettings})
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
	writes := []sqlite.Write{}
	if err == nil {
		storedClientApiServer := existing.ClientApiServer
		if input.ClientApiServer == "" {
			input.ClientApiServer = storedClientApiServer
		}
		// Registrations written before stable IDs remain valid.
		existing.ID = input.ID
		existing.ClientApiServer = input.ClientApiServer
		if existing != input {
			return Cluster{}, fmt.Errorf("%w: cluster is already registered with different connection settings", ErrConflict)
		}
		if storedClientApiServer != input.ClientApiServer {
			write, err := documentWrite("cluster", key, input, previous.Revision)
			if err != nil {
				return Cluster{}, err
			}
			writes = append(writes, write)
		}
	} else {
		write, err := documentWrite("cluster", key, input, 0)
		if err != nil {
			return Cluster{}, err
		}
		writes = append(writes, write)
	}
	spaceWrites, err := manager.clusterSpaceWrites(ctx, input)
	if err != nil {
		return Cluster{}, err
	}
	writes = append(writes, spaceWrites...)
	// Persist the registration, its initial space and reconciliation request together. No
	// target installation or management-vault access belongs in this request.
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
	rendered, err := RenderWithEnv(configuration, stored.Space.Namespace, stored.Space.ID, id, stored.Space.CertificateIssuer, envSnapshots(secrets), ResourceScope{Organization: stored.Space.Organization, Space: stored.Space.Name, Cluster: stored.Space.Cluster})
	if err != nil {
		return Plan{}, fmt.Errorf("render apps: %w", err)
	}
	if err := rejectResourceDeletions(stored.Manifests, rendered.Manifests); err != nil {
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
	plan := Plan{
		ID: id, Space: stored.Space, BaseRevision: stored.Space.Revision,
		Before: stored.Space.Configuration, Configuration: configuration, ConfigurationDiff: configurationDiff,
		Values: rendered.Values, Manifests: rendered.Manifests, RendererVersion: rendered.RendererVersion,
		ResourceDiffs: differences, CreatedAt: time.Now().UTC(),
	}
	write, err := documentWrite("plan", id, planRecord{Plan: plan, Connection: connection, Source: input.Patch, EnvironmentSecrets: secrets, EnvSelectors: selectors}, 0)
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

// Apply durably queues an exact saved plan in server acceptance order for its environment.
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
	if err := verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
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
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		manager.runOperation(operation)
	}()
}

func (manager Manager) runOperation(operation Operation) {
	// Recovery retains the original deadline, including time spent in predeploy.
	ctx, cancel := context.WithDeadline(manager.runtime.context, manager.operationDeadline(operation))
	defer cancel()
	saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
	if err != nil {
		manager.finishOperation(operation, "failed", err.Error())
		return
	}
	client, err := newClusterClient(saved.Connection)
	if err != nil {
		manager.finishOperation(operation, "failed", err.Error())
		return
	}
	secretManifests, remaining := splitEnvironmentManifests(saved.Plan.Manifests)
	jobs, manifests := splitPlanManifests(remaining)
	if operation.Phase == "" {
		// Operations accepted before phases were added remain resumable.
		operation.Phase = "applications"
		if len(jobs) > 0 {
			operation.Phase = "predeploy"
		}
	}
	if err := verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
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
	if operation.Phase == "secrets" {
		operation, err = manager.runEnvironment(ctx, operation, client, secretManifests, len(jobs) > 0)
		if err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
	}
	if operation.Phase == "predeploy" {
		operation, err = manager.runPredeploy(ctx, operation, client, jobs, saved.EnvironmentSecrets, saved.Plan.Space.ID)
		if err != nil {
			manager.failOperation(operation, client, jobs, err)
			return
		}
	}
	// A predeploy task or server restart may have outlived the pinned version's deployment window.
	if err := verifyEnvironment(ctx, client, saved.EnvironmentSecrets); err != nil {
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
		if err := verifyEnvironment(ctx, client, entries); err != nil {
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
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(message, "context deadline") {
		message = "rollout deadline exceeded: " + operation.Message
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
	manager.finishOperation(operation, "failed", message)
}

func (manager Manager) saveOperation(ctx context.Context, operation Operation) error {
	_, record, err := loadDocument[Operation](ctx, manager.database, "operation", operation.ID)
	if err != nil {
		return err
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
		if isPredeployJob(manifest) || manifest["kind"] == "ExternalSecret" {
			continue // Historical Jobs and immutable env snapshots remain outside app pruning.
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
