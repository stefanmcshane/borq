package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"golang.org/x/crypto/bcrypt"
)

type managementTask struct {
	Hostnames             []string                               `json:"hostnames,omitempty"`
	InputRef              string                                 `json:"inputRef,omitempty"`
	InputUID              string                                 `json:"inputUid,omitempty"`
	InputCleanupRef       string                                 `json:"inputCleanupRef,omitempty"`
	InputCleanupUID       string                                 `json:"inputCleanupUid,omitempty"`
	RequestHash           string                                 `json:"requestHash,omitempty"`
	ClusterTailscaleBases map[string]string                      `json:"clusterTailscaleBases,omitempty"`
	SecretGroups          map[string]managementSecretPublication `json:"secretGroups,omitempty"`
	PocketIdEnvironments  map[string]pocketIdEnvironment         `json:"pocketIdEnvironments,omitempty"`
	Authentication        map[string]managementAuthentication    `json:"authentication,omitempty"`
	Configuration         ManagementConfiguration                `json:"configuration"`
	Next                  int                                    `json:"next"`
	ChildKind             string                                 `json:"childKind,omitempty"`
	ChildID               string                                 `json:"childId,omitempty"`
	Deadline              time.Time                              `json:"deadline,omitempty"`
	ComponentRuns         []managementComponentRun               `json:"componentRuns,omitempty"`
	ComponentsStarted     bool                                   `json:"componentsStarted,omitempty"`
}

type managementComponentRun struct {
	Organization string `json:"organization"`
	Cluster      string `json:"cluster"`
	ID           string `json:"id"`
}

type managementAction struct {
	kind          string
	organization  ManagementOrganization
	space         ManagementSpace
	cluster       ClusterInput
	custom        NamedCustomAddon
	postgres      NamedPostgres
	spacePocketId NamedPocketId
	pocketId      ManagementPocketId
	secretGroup   ManagementSecretGroup
}

// GetManagementOperation returns durable aggregate progress after disconnects and restarts.
func (manager Manager) GetManagementOperation(ctx context.Context, id string) (Operation, error) {
	operation, _, err := loadDocument[Operation](ctx, manager.database, "management-operation", id)
	if err == nil && operation.Status == "running" && operation.Phase == "management" && manager.runtime.infrastructureEnabled {
		if stored, _, readErr := loadDocument[componentRecord](ctx, manager.database, "components", "management"); readErr == nil {
			operation.Message = stored.Installation.Message
		}
		operation.Resources = manager.managementSetupDiagnostics(ctx)
	}
	return operation, err
}

// ApplyManagementConfiguration stages credentials and accepts a durable named-resource update.
func (manager Manager) ApplyManagementConfiguration(ctx context.Context, configuration ManagementConfiguration) (Operation, error) {
	var err error
	configuration, err = manager.normalizeManagementIdentity(ctx, configuration)
	if err != nil {
		return Operation{}, err
	}
	if err := validateManagementConfiguration(configuration); err != nil {
		return Operation{}, err
	}
	if err := manager.validateBootstrapNetwork(configuration.Network); err != nil {
		return Operation{}, err
	}
	for _, organization := range configuration.Organizations {
		if addon := organization.ManagedPocketId; addon != nil {
			if err := manager.validateNetworkPocketIdName(addon.IssuerURL); err != nil {
				return Operation{}, err
			}
		}
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Operation{}, errors.New("application manager is stopping")
	}
	if err := manager.validateManagementApplicationHostnames(ctx, configuration); err != nil {
		return Operation{}, err
	}
	digest, err := managementRequestDigest(configuration)
	if err != nil {
		return Operation{}, err
	}
	operations, err := manager.database.List(ctx, "management-operation")
	if err != nil {
		return Operation{}, err
	}
	for _, record := range operations {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return Operation{}, err
		}
		if !operationFinished(operation.Status) {
			task, _, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
			if err != nil {
				return Operation{}, err
			}
			matches, err := task.matchesRequest(digest)
			if err != nil {
				return Operation{}, err
			}
			if matches {
				return operation, nil
			}
			return Operation{}, managementOperationConflict{ID: operation.ID, Legacy: task.RequestHash == ""}
		}
	}
	requestHash, err := bcrypt.GenerateFromPassword(digest[:], bcrypt.DefaultCost)
	if err != nil {
		return Operation{}, err
	}
	if manager.runtime.managementBootstrapOnly {
		return manager.acceptInitialManagementConfiguration(ctx, configuration, string(requestHash))
	}
	safe, err := manager.stageManagementConfiguration(ctx, configuration)
	if err != nil {
		return Operation{}, err
	}
	authentication, identityWrites, err := manager.stageManagementAuthentication(ctx, &safe)
	if err != nil {
		manager.cleanManagementKubeconfigs(safe)
		return Operation{}, err
	}
	publications, environments, err := manager.stageManagementSecretGroups(ctx, &safe, authentication)
	if err != nil {
		manager.cleanManagementKubeconfigs(safe)
		return Operation{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			manager.cleanManagementKubeconfigs(safe)
		}
	}()
	id, err := newId()
	if err != nil {
		return Operation{}, err
	}
	now := time.Now().UTC()
	operation := Operation{ID: id, Status: "queued", Phase: "management", Message: "management configuration accepted", Resources: []kubeapps.ResourceStatus{}, StartedAt: now, UpdatedAt: now}
	task := managementTask{RequestHash: string(requestHash), Configuration: safe, Authentication: authentication, SecretGroups: publications, PocketIdEnvironments: environments, Deadline: now.Add(manager.runtime.rolloutTimeout * 4)}
	task.ClusterTailscaleBases = managementClusterTailscaleBases(safe)
	taskWrite, err := documentWrite("management-task", id, task, 0)
	if err != nil {
		return Operation{}, err
	}
	operationWrite, err := documentWrite("management-operation", id, operation, 0)
	if err != nil {
		return Operation{}, err
	}
	if err := manager.database.Commit(ctx, append(identityWrites, taskWrite, operationWrite)); err != nil {
		return Operation{}, err
	}
	accepted = true
	select {
	case manager.runtime.pocketIdWake <- struct{}{}:
	default:
	}
	manager.startManagementOperation(operation)
	return operation, nil
}

func validateManagementConfiguration(configuration ManagementConfiguration) error {
	if err := validateManagementSecretGroups(configuration); err != nil {
		return err
	}
	if err := validateManagementAuthentication(configuration); err != nil {
		return err
	}
	if err := platform.ValidateComponentOverrides(configuration.Components); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := validateRegistries(configuration.Registries, "management"); err != nil {
		return err
	}
	organizations := map[string]bool{}
	for _, organization := range configuration.Organizations {
		if err := validateNames(organization.Name); err != nil {
			return err
		}
		if organizations[organization.Name] {
			return fmt.Errorf("%w: duplicate organization %s", ErrInvalid, organization.Name)
		}
		organizations[organization.Name] = true
		clusters := map[string]bool{}
		for _, cluster := range organization.Clusters {
			if err := validateNames(cluster.Name); err != nil {
				return err
			}
			cluster.Organization = organization.Name
			if err := validateClusterTailscale(cluster); err != nil {
				return err
			}
			if clusters[cluster.Name] {
				return fmt.Errorf("%w: duplicate cluster %s", ErrInvalid, cluster.Name)
			}
			clusters[cluster.Name] = true
		}
		spaces := map[string]bool{}
		for _, space := range organization.Spaces {
			if err := validateRegistries(space.Registries, initialSpace(organization.Name, space.Name).ID); err != nil {
				return err
			}
			if err := validateNames(space.Name); err != nil {
				return err
			}
			if spaces[space.Name] {
				return fmt.Errorf("%w: duplicate space %s", ErrInvalid, space.Name)
			}
			spaces[space.Name] = true
			apps := map[string]bool{}
			for _, app := range space.Apps {
				if err := validateNames(app.Name); err != nil {
					return err
				}
				if apps[app.Name] {
					return fmt.Errorf("%w: duplicate app %s", ErrInvalid, app.Name)
				}
				apps[app.Name] = true
			}
			addons := map[string]bool{}
			for _, addon := range space.CustomAddons {
				if err := validateNames(addon.Name); err != nil {
					return err
				}
				if addons[addon.Name] {
					return fmt.Errorf("%w: duplicate custom addon %s", ErrInvalid, addon.Name)
				}
				addons[addon.Name] = true
				if err := validateAddonInput(addon.AddonInput); err != nil {
					return err
				}
			}
			providers := map[string]bool{}
			for _, provider := range space.ManagedAddons.PocketId {
				if err := validateNames(provider.Name); err != nil {
					return err
				}
				if providers[provider.Name] {
					return fmt.Errorf("%w: duplicate managed Pocket ID %s", ErrInvalid, provider.Name)
				}
				providers[provider.Name] = true
			}
			databases := map[string]bool{}
			for _, postgres := range space.ManagedAddons.Postgres {
				if err := validateNames(postgres.Name); err != nil {
					return err
				}
				if databases[postgres.Name] {
					return fmt.Errorf("%w: duplicate managed postgres %s", ErrInvalid, postgres.Name)
				}
				databases[postgres.Name] = true
			}
		}
	}
	return nil
}

func (manager Manager) stageManagementConfiguration(ctx context.Context, configuration ManagementConfiguration) (ManagementConfiguration, error) {
	data, err := json.Marshal(configuration)
	if err != nil {
		return ManagementConfiguration{}, fmt.Errorf("%w: configuration cannot be encoded", ErrInvalid)
	}
	var safe ManagementConfiguration
	staged := false
	defer func() {
		if !staged {
			manager.cleanManagementKubeconfigs(safe)
		}
	}()
	if err := json.Unmarshal(data, &safe); err != nil {
		return ManagementConfiguration{}, err
	}
	safe.Registries, err = manager.stageRegistries(ctx, safe.Registries, "management")
	if err != nil {
		return ManagementConfiguration{}, err
	}
	if safe.Backups != nil {
		input := *safe.Backups
		if !input.Enabled && input.EncryptionKey == "" {
			previous, _, err := loadDocument[ManagementBackupInput](ctx, manager.database, "management-backups", "shared")
			if err == nil {
				input = previous
				input.Enabled = false
			} else if !errors.Is(err, ErrNotFound) {
				return ManagementConfiguration{}, err
			}
		}
		policy, err := prepareManagementBackups(input)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		safe.Backups = &policy
	}
	prospectiveStorage := ManagementS3{}
	if safe.S3 != nil {
		storage, err := manager.prepareManagementS3(ctx, *safe.S3)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		prospectiveStorage = storage
		safe.S3 = &ManagementS3Input{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: storage.PathStyle, CredentialsRef: storage.CredentialsRef}
	}
	if safe.Offsite != nil {
		if prospectiveStorage.Bucket == "" {
			storage, err := manager.GetManagementS3(ctx)
			if err != nil {
				return ManagementConfiguration{}, err
			}
			prospectiveStorage = storage
		}
		policy, err := manager.prepareOffsite(ctx, *safe.Offsite, prospectiveStorage)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		input := policy.input()
		safe.Offsite = &input
	}
	for organizationIndex := range safe.Organizations {
		organization := &safe.Organizations[organizationIndex]
		for spaceIndex := range organization.Spaces {
			space := &organization.Spaces[spaceIndex]
			space.Registries, err = manager.stageRegistries(ctx, space.Registries, initialSpace(organization.Name, space.Name).ID)
			if err != nil {
				return ManagementConfiguration{}, err
			}
		}
		for clusterIndex := range organization.Clusters {
			cluster := &organization.Clusters[clusterIndex]
			cluster.Organization = organization.Name
			cluster.ID = platform.ClusterID(organization.Name, cluster.Name)
			previous, _, readErr := loadDocument[Cluster](ctx, manager.database, "cluster", organization.Name+"/"+cluster.Name)
			if readErr != nil && !errors.Is(readErr, ErrNotFound) {
				return ManagementConfiguration{}, readErr
			}
			if readErr == nil {
				previous.ID = cluster.ID
			}
			if cluster.Tailscale != nil {
				stagedCluster, _, stageErr := manager.stageClusterTailscale(*cluster, previous)
				if stageErr != nil {
					return ManagementConfiguration{}, stageErr
				}
				*cluster = stagedCluster
				cluster.tailscaleBaseCaptured = true
				if previous.Tailscale != nil {
					cluster.tailscaleBaseRef = previous.Tailscale.CredentialsRef
				}
			}
			if cluster.Kubeconfig == "" {
				continue
			}
			if cluster.KubeconfigPath != "" || cluster.InCluster {
				return ManagementConfiguration{}, fmt.Errorf("%w: supply one kubeconfig source", ErrInvalid)
			}
			if err := os.MkdirAll(manager.runtime.credentialsDir, 0o700); err != nil {
				return ManagementConfiguration{}, err
			}
			existing, _, existingErr := loadDocument[Cluster](ctx, manager.database, "cluster", organization.Name+"/"+cluster.Name)
			if existingErr == nil && existing.KubeconfigPath != "" {
				cluster.KubeconfigPath = existing.KubeconfigPath
			} else {
				if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
					return ManagementConfiguration{}, existingErr
				}
				id, err := newId()
				if err != nil {
					return ManagementConfiguration{}, err
				}
				cluster.KubeconfigPath = filepath.Join(manager.runtime.credentialsDir, "management-"+id+".yaml")
			}
			if _, err := storeClusterKubeconfig(cluster.KubeconfigPath, cluster.Kubeconfig); err != nil {
				return ManagementConfiguration{}, err
			}
			cluster.Kubeconfig = ""
		}
	}
	staged = true
	return safe, nil
}

func managementActions(configuration ManagementConfiguration) []managementAction {
	actions := []managementAction{}
	if len(configuration.Registries) > 0 {
		actions = append(actions, managementAction{kind: "management-registries"})
	}
	if configuration.S3 != nil {
		actions = append(actions, managementAction{kind: "test-s3"})
	}
	if configuration.Offsite != nil && (configuration.Offsite.Enabled || configuration.Offsite.Repository != "") {
		actions = append(actions, managementAction{kind: "test-offsite"})
	}
	if configuration.S3 != nil {
		actions = append(actions, managementAction{kind: "s3"})
	}
	if configuration.Backups != nil {
		actions = append(actions, managementAction{kind: "backups"})
	}
	if configuration.Offsite != nil {
		actions = append(actions, managementAction{kind: "offsite"})
	}
	if len(configuration.Components) > 0 {
		actions = append(actions, managementAction{kind: "components"})
	}
	for _, organization := range configuration.Organizations {
		actions = append(actions, managementAction{kind: "organization", organization: organization})
		for _, group := range configuration.SecretGroups {
			if group.Organization == organization.Name {
				actions = append(actions, managementAction{kind: "management-secret-group", organization: organization, secretGroup: group})
			}
		}
		if organization.ManagedPocketId != nil {
			actions = append(actions, managementAction{kind: "pocketid", organization: organization, pocketId: *organization.ManagedPocketId})
		}
		if organization.Oidc != nil || organization.ManagedPocketId != nil {
			actions = append(actions, managementAction{kind: "oidc", organization: organization})
		}
		if organization.ManagedAddons != nil {
			actions = append(actions, managementAction{kind: "grant", organization: organization})
		}
		for _, cluster := range organization.Clusters {
			actions = append(actions, managementAction{kind: "cluster", organization: organization, cluster: cluster})
		}
		for _, space := range organization.Spaces {
			actions = append(actions, managementAction{kind: "space", organization: organization, space: space})
			if len(space.Registries) > 0 {
				actions = append(actions, managementAction{kind: "space-registries", organization: organization, space: space})
			}
			// Retain this action slot so persisted management checkpoints keep their meaning.
			if len(space.Apps) > 0 || len(space.Registries) > 0 {
				actions = append(actions, managementAction{kind: "apps", organization: organization, space: space})
			}
			for _, custom := range space.CustomAddons {
				actions = append(actions, managementAction{kind: "custom", organization: organization, space: space, custom: custom})
			}
			for _, postgres := range space.ManagedAddons.Postgres {
				actions = append(actions, managementAction{kind: "postgres", organization: organization, space: space, postgres: postgres})
			}
			for _, provider := range space.ManagedAddons.PocketId {
				actions = append(actions, managementAction{kind: "space-pocketid", organization: organization, space: space, spacePocketId: provider})
			}
		}
	}
	return actions
}

func (manager Manager) resumeManagementOperations(ctx context.Context) error {
	if err := manager.resumeStorageRotation(ctx); err != nil {
		return err
	}
	records, err := manager.database.List(ctx, "management-operation")
	if err != nil {
		return err
	}
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		if !operationFinished(operation.Status) {
			manager.startManagementOperation(operation)
		} else {
			task, previous, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
			if err == nil && (task.InputRef != "" || task.InputCleanupRef != "") && manager.deleteManagementInput(ctx, task) == nil {
				task.InputRef, task.InputUID, task.InputCleanupRef, task.InputCleanupUID = "", "", "", ""
				write, err := documentWrite("management-task", operation.ID, task, previous.Revision)
				if err != nil {
					return err
				}
				if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (manager Manager) startManagementOperation(operation Operation) {
	manager.runtime.managementRunsMutex.Lock()
	if manager.runtime.managementRuns[operation.ID] {
		manager.runtime.managementRunsMutex.Unlock()
		return
	}
	manager.runtime.managementRuns[operation.ID] = true
	manager.runtime.managementRunsMutex.Unlock()
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		defer func() {
			manager.runtime.managementRunsMutex.Lock()
			delete(manager.runtime.managementRuns, operation.ID)
			manager.runtime.managementRunsMutex.Unlock()
		}()
		manager.runManagementOperation(operation)
	}()
}
func (manager Manager) runManagementOperation(operation Operation) {
	current, err := manager.GetManagementOperation(manager.runtime.context, operation.ID)
	if err != nil || operationFinished(current.Status) {
		return
	}
	operation = current
	task, _, err := loadDocument[managementTask](manager.runtime.context, manager.database, "management-task", operation.ID)
	if err != nil {
		return
	}
	if task.Deadline.IsZero() {
		task.Deadline = operation.StartedAt.Add(manager.runtime.rolloutTimeout * 4)
	}
	ctx, cancel := context.WithDeadline(manager.runtime.context, task.Deadline)
	defer cancel()
	if task.InputRef != "" {
		operation.Status, operation.Phase, operation.Message = "running", "management", "configuring management services"
		if err = manager.saveManagementProgress(ctx, operation, task); err == nil {
			task, err = manager.prepareInitialManagementConfiguration(ctx, operation, task)
		}
	}
	actions := managementActions(task.Configuration)
	for err == nil && task.Next < len(actions) {
		action := actions[task.Next]
		operation.Status, operation.Phase, operation.Message = "running", action.kind, "applying management "+action.kind
		switch action.kind {
		case "test-s3":
			operation.Message = "Testing primary S3 connection. Saved settings remain active."
		case "test-offsite":
			operation.Message = "Testing offsite S3 connection. Saved settings remain active."
		}
		if err = manager.saveManagementProgress(ctx, operation, task); err != nil {
			if manager.runtime.context.Err() != nil {
				return
			}
			break
		}
		if action.kind == "components" {
			err = manager.applyManagementComponents(ctx, operation, &task)
		} else if task.ChildID != "" {
			err = manager.waitManagementChild(ctx, task.ChildKind, task.ChildID)
		} else {
			task.ChildKind, task.ChildID, err = manager.executeManagementAction(ctx, task, action)
			if err == nil && task.ChildID != "" {
				if err = manager.saveManagementProgress(ctx, operation, task); err == nil {
					err = manager.waitManagementChild(ctx, task.ChildKind, task.ChildID)
				}
			}
		}
		if err != nil {
			break
		}
		if !strings.HasSuffix(task.ChildKind, "-existing") {
			task.Next++
		}
		task.ChildKind, task.ChildID = "", ""
	}
	if err == nil {
		for _, organization := range task.Configuration.Organizations {
			accepted := task.Authentication[organization.Name]
			if organization.Oidc != nil || organization.ManagedPocketId != nil {
				continue
			}
			// Implicit providers have no declared action, but must finish before
			// the management operation is marked successful.
			operation.Status, operation.Phase, operation.Message = "running", "oidc", "waiting for organization authentication setup"
			if err = manager.saveManagementProgress(ctx, operation, task); err == nil {
				err = manager.waitImplicitManagementPocketId(ctx, organization.Name, accepted)
			}
			if err != nil {
				break
			}
		}
	}
	if manager.runtime.context.Err() != nil {
		return
	}
	if err == nil {
		err = ctx.Err()
	}
	operation.Status, operation.Message = "succeeded", "management configuration applied"
	if err != nil {
		operation.Status, operation.Message = "failed", safeManagementError(err)
	}
	final, cancelFinal := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelFinal()
	if err != nil && operation.Phase == "management" && manager.runtime.infrastructureEnabled {
		operation.Resources = manager.managementSetupDiagnostics(final)
		if stored, _, readErr := loadDocument[componentRecord](final, manager.database, "components", "management"); readErr == nil && stored.Installation.Message != "" {
			operation.Message += "; " + stored.Installation.Message
		}
	}
	if err != nil {
		slog.ErrorContext(final, "management_operation_failed", "operation_id", operation.ID, "phase", operation.Phase, "message", operation.Message, "resources", operation.Resources)
	}
	manager.cleanManagementKubeconfigs(task.Configuration)
	if manager.saveManagementProgress(final, operation, task) == nil {
		if (task.InputRef != "" || task.InputCleanupRef != "") && manager.deleteManagementInput(final, task) == nil {
			task.InputRef, task.InputUID, task.InputCleanupRef, task.InputCleanupUID = "", "", "", ""
			_ = manager.saveManagementProgress(final, operation, task)
		}
	}
}
func safeManagementError(err error) string {
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrComponentsNotReady) {
		return err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "management configuration exceeded its execution timeout"
	}
	return "management configuration failed; inspect the affected resource operation"
}
func (manager Manager) saveManagementProgress(ctx context.Context, operation Operation, task managementTask) error {
	_, storedOperation, err := loadDocument[Operation](ctx, manager.database, "management-operation", operation.ID)
	if err != nil {
		return err
	}
	_, storedTask, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
	if err != nil {
		return err
	}
	operation.UpdatedAt = time.Now().UTC()
	operationWrite, err := documentWrite("management-operation", operation.ID, operation, storedOperation.Revision)
	if err != nil {
		return err
	}
	taskWrite, err := documentWrite("management-task", operation.ID, task, storedTask.Revision)
	if err != nil {
		return err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{operationWrite, taskWrite}); err != nil {
		return err
	}
	slog.InfoContext(ctx, "management_operation_progress", "operation_id", operation.ID, "status", operation.Status, "phase", operation.Phase, "message", operation.Message)
	return nil
}
func (manager Manager) executeManagementAction(ctx context.Context, task managementTask, action managementAction) (string, string, error) {
	configuration := task.Configuration
	organization, space := action.organization.Name, action.space.Name
	switch action.kind {
	case "pocketid":
		return "", "", manager.applyManagementPocketId(ctx, action, task.Authentication[organization], task.PocketIdEnvironments[organization])
	case "management-secret-group":
		return "", "", manager.applyManagementSecretGroup(ctx, action.secretGroup, task.Authentication[organization], task.SecretGroups[organization+"/"+action.secretGroup.Name])
	case "oidc":
		return "", "", manager.applyManagementOidc(ctx, action.organization, task.Authentication[organization])
	case "management-registries":
		return "", "", manager.activateManagementRegistries(ctx, configuration.Registries)
	case "space-registries":
		stored, err := manager.GetSpace(ctx, organization, space)
		if err != nil {
			return "", "", err
		}
		return "", "", manager.activateSpaceRegistries(ctx, stored, action.space.Registries)
	case "test-s3", "s3":
		input := configuration.S3
		storage := ManagementS3{Endpoint: input.Endpoint, Region: input.Region, Bucket: input.Bucket, Prefix: input.Prefix, PathStyle: input.PathStyle, CredentialsRef: input.CredentialsRef}
		if action.kind == "test-s3" {
			return "", "", manager.testManagementS3(ctx, storage)
		}
		manager.runtime.storageMutex.Lock()
		defer manager.runtime.storageMutex.Unlock()
		_, err := manager.activateManagementS3(ctx, storage)
		return "", "", err
	case "test-offsite":
		return "", "", manager.testOffsite(ctx, *configuration.Offsite)
	case "backups":
		return "", "", manager.activateManagementBackups(ctx, *configuration.Backups)
	case "offsite":
		return "", "", manager.activateOffsite(ctx, *configuration.Offsite)
	case "organization":
		_, err := manager.CreateOrganization(ctx, organization)
		return "", "", err
	case "grant":
		_, err := manager.SetOrganizationManagedAddons(ctx, organization, *action.organization.ManagedAddons)
		return "", "", err
	case "cluster":
		base, captured := task.ClusterTailscaleBases[action.cluster.Organization+"/"+action.cluster.Name]
		_, err := manager.registerManagementCluster(ctx, action.cluster, base, captured)
		return "", "", err
	case "space":
		if action.space.Cluster == "" {
			existing, err := manager.GetSpace(ctx, organization, space)
			if err != nil {
				return "", "", err
			}
			return "", "", manager.waitManagementComponents(ctx, organization, existing.Cluster)
		}
		if err := manager.waitManagementComponents(ctx, organization, action.space.Cluster); err != nil {
			return "", "", err
		}
		_, err := manager.CreateSpace(ctx, SpaceInput{Organization: organization, Name: space, Cluster: action.space.Cluster})
		return "", "", err
	case "apps":
		if len(action.space.Apps) == 0 {
			return "", "", nil
		}
		patch := Patch{Apps: &action.space.Apps}
		plan, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: space, Patch: patch})
		if err != nil {
			return "", "", err
		}
		operation, err := manager.Apply(ctx, plan.ID)
		return "apps", operation.ID, err
	case "custom":
		existing, err := manager.GetAddon(ctx, organization, space, action.custom.Name)
		if err == nil && existing.State == "pending" {
			return "custom-existing", existing.Operation, nil
		}
		addon, err := manager.PutAddon(ctx, organization, space, action.custom.Name, action.custom.AddonInput, true)
		return "custom", addon.Operation, err
	case "space-pocketid":
		existing, err := manager.GetPocketId(ctx, organization, space, action.spacePocketId.Name)
		if err == nil && existing.State == "pending" {
			return "space-pocketid-existing", existing.Operation, nil
		}
		provider, err := manager.PutPocketId(ctx, organization, space, action.spacePocketId.Name, action.spacePocketId.PocketIdInput, true)
		return "space-pocketid", provider.Operation, err
	case "postgres":
		existing, err := manager.GetPostgres(ctx, organization, space, action.postgres.Name)
		if err == nil && existing.State == "pending" {
			return "postgres-existing", existing.Operation, nil
		}
		postgres, err := manager.PutPostgres(ctx, organization, space, action.postgres.Name, action.postgres.PostgresInput, true)
		return "postgres", postgres.Operation, err
	}
	return "", "", fmt.Errorf("unknown management action")
}
func (manager Manager) waitManagementComponents(ctx context.Context, organization, cluster string) error {
	if !manager.runtime.infrastructureEnabled {
		return nil
	}
	ticker := time.NewTicker(manager.runtime.pollInterval)
	defer ticker.Stop()
	for {
		installation, err := manager.GetClusterComponents(ctx, organization, cluster)
		if err != nil {
			return err
		}
		if installation.Status == "ready" {
			return nil
		}
		if installation.Status == "failed" || installation.Status == "disabled" {
			return fmt.Errorf("%w: target components are not ready", ErrComponentsNotReady)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (manager Manager) waitManagementChild(ctx context.Context, kind, id string) error {
	ticker := time.NewTicker(manager.runtime.pollInterval)
	defer ticker.Stop()
	for {
		var operation Operation
		var err error
		switch strings.TrimSuffix(kind, "-existing") {
		case "apps":
			operation, err = manager.GetOperation(ctx, id)
		case "custom":
			operation, err = manager.GetAddonOperation(ctx, id)
		case "space-pocketid":
			operation, err = manager.GetPocketIdOperation(ctx, id)
		case "postgres":
			operation, err = manager.GetPostgresOperation(ctx, id)
		default:
			return errors.New("unknown management child operation")
		}
		if err != nil {
			return err
		}
		if operation.Status == "succeeded" {
			return nil
		}
		if operationFinished(operation.Status) {
			return fmt.Errorf("%w: %s operation %s %s", ErrConflict, kind, id, operation.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager Manager) cleanManagementKubeconfigs(configuration ManagementConfiguration) {
	for _, organization := range configuration.Organizations {
		for _, cluster := range organization.Clusters {
			if cluster.tailscaleStagedRef != "" {
				manager.cleanClusterTailscale(cluster, cluster.tailscaleStagedRef)
			}
			path := cluster.KubeconfigPath
			if filepath.Dir(path) != manager.runtime.credentialsDir || !strings.HasPrefix(filepath.Base(path), "management-") {
				continue
			}
			registered, _, err := loadDocument[Cluster](context.Background(), manager.database, "cluster", organization.Name+"/"+cluster.Name)
			if errors.Is(err, ErrNotFound) || (err == nil && registered.KubeconfigPath != path) {
				_ = os.Remove(path)
			}
		}
	}
}

func (manager Manager) applyManagementComponents(ctx context.Context, operation Operation, task *managementTask) error {
	if err := manager.putComponentOverrides(ctx, task.Configuration.Components); err != nil {
		return err
	}
	if !manager.runtime.infrastructureEnabled {
		return nil
	}
	names := make([]string, 0, len(task.Configuration.Components))
	for name := range task.Configuration.Components {
		names = append(names, name)
	}
	slices.Sort(names)
	if !task.ComponentsStarted {
		if _, err := manager.reconcileManagementComponents(ctx, true, names); err != nil {
			return err
		}
		organizations, err := manager.ListOrganizations(ctx)
		if err != nil {
			return err
		}
		task.ComponentRuns = []managementComponentRun{}
		for _, organization := range organizations {
			clusters, err := manager.ListClusters(ctx, organization.Name)
			if err != nil {
				return err
			}
			for _, cluster := range clusters {
				task.ComponentRuns = append(task.ComponentRuns, managementComponentRun{Organization: organization.Name, Cluster: cluster.Name})
			}
		}
		task.ComponentsStarted = true
		if err := manager.saveManagementProgress(ctx, operation, *task); err != nil {
			return err
		}
	}
	for index := range task.ComponentRuns {
		run := &task.ComponentRuns[index]
		if run.ID != "" {
			continue
		}
		cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", run.Organization+"/"+run.Cluster)
		if err != nil {
			return err
		}
		installation, err := manager.queueClusterComponentsSelection(ctx, cluster, true, names, operation.ID)
		if err != nil {
			return err
		}
		run.ID = installation.ID
		if err := manager.saveManagementProgress(ctx, operation, *task); err != nil {
			return err
		}
	}
	for _, run := range task.ComponentRuns {
		if err := manager.waitManagementComponentRun(ctx, run); err != nil {
			return err
		}
	}
	if !slices.Contains(names, "cloudnative-pg") && !slices.Contains(names, "plugin-barman-cloud") {
		return nil
	}
	records, err := manager.database.List(ctx, "postgres")
	if err != nil {
		return err
	}
	clusters := map[string]bool{}
	for _, record := range records {
		var stored postgresRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return err
		}
		if stored.Postgres.State == "removed" {
			continue
		}
		key := stored.Organization + "/" + stored.Cluster
		if clusters[key] {
			continue
		}
		clusters[key] = true
		cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", key)
		if err != nil {
			return err
		}
		settings, err := manager.GetPlatformSettings(ctx)
		if err != nil {
			return err
		}
		manager.runtime.platformMutex.Lock()
		_, err = platform.ReconcilePostgresComponents(ctx, platform.TargetInput{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster, Organization: cluster.Organization, Name: cluster.Name, ID: cluster.ID, Settings: settings, UpgradeComponents: names, RecoverPending: true})
		manager.runtime.platformMutex.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (manager Manager) waitManagementComponentRun(ctx context.Context, run managementComponentRun) error {
	ticker := time.NewTicker(manager.runtime.pollInterval)
	defer ticker.Stop()
	for {
		installation, err := manager.GetClusterComponents(ctx, run.Organization, run.Cluster)
		if err != nil {
			return err
		}
		if installation.ID != run.ID {
			return fmt.Errorf("%w: target component reconciliation was replaced", ErrConflict)
		}
		if installation.Status == "ready" {
			return nil
		}
		if installation.Status == "failed" || installation.Status == "disabled" {
			return fmt.Errorf("%w: target component reconciliation failed", ErrComponentsNotReady)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
