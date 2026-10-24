package apps

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strconv"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

type plannedEnv struct {
	Snapshot EnvSnapshot `json:"snapshot"`
}

func envSnapshots(entries []plannedEnv) []EnvSnapshot {
	snapshots := make([]EnvSnapshot, 0, len(entries))
	for _, entry := range entries {
		snapshots = append(snapshots, entry.Snapshot)
	}
	return snapshots
}

// The OpenBao SDK-backed client owns shared transport state and requires a pointer.
func openEnvironmentVault(ctx context.Context, client kubeapps.Client) (*openbao.Client, string, error) {
	access, err := client.OpenBaoStore(ctx, "borq-openbao")
	if err != nil {
		return nil, "", err
	}
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: access.Address, Token: access.Token, HttpClient: access.HttpClient})
	return vault, access.Mount, err
}

func (manager Manager) prepareEnvironment(ctx context.Context, client kubeapps.Client, stored spaceRecord, patch Patch) (Configuration, []plannedEnv, map[string]EnvReference, error) {
	current := stored.Space.Configuration
	current.Apps = append([]App{}, current.Apps...)
	for index, application := range current.Apps {
		if selector, exists := stored.EnvSelectors[application.Name]; exists {
			current.Apps[index].Env = selector
		}
	}
	configuration, err := Merge(current, patch)
	if err != nil {
		return Configuration{}, nil, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	selectors := map[string]EnvReference{}
	entries := []plannedEnv{}
	// Open lazily so applications without published configuration need no vault credentials.
	var vault *openbao.Client
	mount := ""
	for index, application := range configuration.Apps {
		selector := application.Env
		selectors[application.Name] = selector
		implicit := selector.Name == ""
		name := selector.Name
		if implicit {
			prefix := stored.Space.Organization + "-" + stored.Space.Name
			if stored.EnvironmentGroupPrefix != "" {
				prefix = stored.EnvironmentGroupPrefix
			}
			name = prefix + "-" + application.Name
		}
		application.Env = EnvReference{Name: name, Version: "0"}
		if selector.Version == "0" {
			configuration.Apps[index] = application
			continue
		}
		catalogKnown := false
		if implicit {
			_, _, catalogErr := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", stored.Space.ID+"/"+name)
			catalogKnown = catalogErr == nil
			if catalogErr != nil && !errors.Is(catalogErr, ErrNotFound) {
				return Configuration{}, nil, nil, catalogErr
			}
		}
		number, err := configVersion(selector.Version)
		if err != nil {
			return Configuration{}, nil, nil, err
		}
		if vault == nil {
			vault, mount, err = openEnvironmentVault(ctx, client)
			if err != nil {
				if implicit && number == 0 && !catalogKnown && errors.Is(err, kubeapps.ErrSecretStoreNotFound) {
					configuration.Apps[index] = application
					continue
				}
				return Configuration{}, nil, nil, err
			}
		}
		path, err := manager.resolveEnvironmentGroupPath(ctx, stored.Space.ID, name)
		if err != nil {
			return Configuration{}, nil, nil, err
		}
		metadata, err := vault.ReadKvMetadata(ctx, mount, path)
		if errors.Is(err, openbao.ErrNotFound) {
			if implicit && number == 0 {
				configuration.Apps[index] = application
				continue
			}
			return Configuration{}, nil, nil, fmt.Errorf("%w: environment group %s version %s is unavailable", ErrNotFound, name, selector.Version)
		}
		if err != nil {
			return Configuration{}, nil, nil, err
		}
		if number == 0 {
			number = metadata.CurrentVersion
			if number == 0 && implicit {
				configuration.Apps[index] = application
				continue
			}
		}
		if err := validateEnvironmentGroupVersion(name, number, metadata.CurrentVersion); err != nil {
			return Configuration{}, nil, nil, err
		}
		entry, err := vault.ReadKv(ctx, mount, path, number)
		if errors.Is(err, openbao.ErrNotFound) {
			return Configuration{}, nil, nil, fmt.Errorf("%w: environment group %s version %d is unavailable", ErrNotFound, name, number)
		}
		if err != nil {
			return Configuration{}, nil, nil, err
		}
		application.Env.Version = strconv.Itoa(entry.Version)
		configuration.Apps[index] = application
		snapshot := EnvSnapshot{AppName: application.Name, ConfigName: name, Store: "borq-openbao", Path: path, Version: entry.Version, SnapshotID: stored.Space.ID + "/" + name + "/" + strconv.Itoa(entry.Version), Values: sortedVariables(entry.Values)}
		entries = append(entries, plannedEnv{Snapshot: snapshot})
	}
	return configuration, entries, selectors, nil
}

func variableValues(entries []Variable) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		values[entry.Name] = entry.Value
	}
	return values
}
func sortedVariables(values map[string]string) []Variable {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]Variable, 0, len(values))
	for _, name := range names {
		entries = append(entries, Variable{Name: name, Value: values[name]})
	}
	return entries
}

func verifyEnvironment(ctx context.Context, client kubeapps.Client, entries []plannedEnv) error {
	if len(entries) == 0 {
		return nil
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		metadata, err := vault.ReadKvMetadata(ctx, mount, entry.Snapshot.Path)
		if errors.Is(err, openbao.ErrNotFound) {
			return fmt.Errorf("%w: pinned environment group %s version %d is unavailable", ErrNotFound, entry.Snapshot.ConfigName, entry.Snapshot.Version)
		}
		if err != nil {
			return err
		}
		if err := validateEnvironmentGroupVersion(entry.Snapshot.ConfigName, entry.Snapshot.Version, metadata.CurrentVersion); err != nil {
			return err
		}
		pinned, err := vault.ReadKv(ctx, mount, entry.Snapshot.Path, entry.Snapshot.Version)
		if errors.Is(err, openbao.ErrNotFound) {
			return fmt.Errorf("pinned environment group %s version %d is unavailable", entry.Snapshot.ConfigName, entry.Snapshot.Version)
		}
		if err != nil {
			return err
		}
		if !maps.Equal(pinned.Values, variableValues(entry.Snapshot.Values)) {
			return fmt.Errorf("pinned environment group %s no longer matches the reviewed snapshot", entry.Snapshot.ConfigName)
		}
	}
	return nil
}

func (manager Manager) runEnvironment(ctx context.Context, operation Operation, client kubeapps.Client, manifests []map[string]any, hasJobs bool) (Operation, error) {
	operation.Message = "synchronizing pinned environment group"
	if err := manager.saveOperation(ctx, operation); err != nil {
		return operation, err
	}
	if err := client.Apply(ctx, manifests, operation.SpaceID); err != nil {
		return operation, err
	}
	for {
		status, err := client.Status(ctx, manifests, operation.SpaceID)
		if ctx.Err() != nil {
			return operation, ctx.Err()
		}
		if err == nil {
			operation.Resources, operation.Message = status.Resources, status.Message
			if status.State == "failed" {
				return operation, errors.New(status.Message)
			}
			if status.State == "succeeded" {
				break
			}
		} else {
			operation.Message = "waiting for environment Secrets: " + err.Error()
		}
		operation.Status = "waiting"
		if err := manager.saveOperation(ctx, operation); err != nil {
			return operation, err
		}
		if err := manager.waitForPoll(ctx); err != nil {
			return operation, err
		}
	}
	operation.Phase, operation.Status, operation.Message = "applications", "applying", "environment Secrets ready"
	if hasJobs {
		operation.Phase = "predeploy"
	}
	return operation, manager.saveOperation(ctx, operation)
}

func splitEnvironmentManifests(manifests []map[string]any) ([]map[string]any, []map[string]any) {
	secrets, remaining := []map[string]any{}, []map[string]any{}
	for _, manifest := range manifests {
		if manifest["kind"] == "ExternalSecret" {
			secrets = append(secrets, manifest)
		} else {
			remaining = append(remaining, manifest)
		}
	}
	return secrets, remaining
}

// RenderApps returns saved plan artifacts exactly, or creates a target-independent preview.
func (manager Manager) RenderApps(ctx context.Context, configuration Configuration, namespace, targetID, planID, issuer string, scope ResourceScope) (Rendered, error) {
	if planID == "" {
		planID = "preview"
	}
	if manager.runtime != nil && planID != "preview" {
		saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", planID)
		if err == nil {
			if saved.Plan.Space.ID != targetID || saved.Plan.Space.Namespace != namespace || !reflect.DeepEqual(saved.Plan.Configuration, configuration) || issuer != "" && issuer != saved.Plan.Space.CertificateIssuer {
				return Rendered{}, fmt.Errorf("%w: rendering must match the saved plan", ErrInvalid)
			}
			if scope != (ResourceScope{}) && scope != (ResourceScope{Organization: saved.Plan.Space.Organization, Space: saved.Plan.Space.Name, Cluster: saved.Plan.Space.Cluster}) {
				return Rendered{}, fmt.Errorf("%w: rendering scope must match the saved plan", ErrInvalid)
			}
			return Rendered{Values: saved.Plan.Values, Manifests: saved.Plan.Manifests, RendererVersion: saved.Plan.RendererVersion, EnvSnapshots: envSnapshots(saved.EnvironmentSecrets)}, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Rendered{}, err
		}
	}
	return RenderWithEnv(configuration, namespace, targetID, planID, issuer, nil, scope)
}

// InspectApps reconstructs environment group references without retrieving or merging secret values.
func (manager Manager) InspectApps(ctx context.Context, manifests []map[string]any) (Configuration, error) {
	return Inspect(manifests)
}
