package apps

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Secret is a typed JSON document scoped to one space. Registry tokens are
// accepted on writes but omitted from reads; omission preserves an existing token.
type Secret struct {
	// Name identifies a secret independently of its registry address.
	Name string `json:"name"`
	// Type selects validation and application behavior.
	Type string `json:"type"`
	// Data contains string keys and values stored in OpenBao.
	Data map[string]string `json:"data"`
	// Version identifies the publication used for conditional updates.
	Version int `json:"version"`
	// OldestVersion excludes publications destroyed by an earlier deletion.
	OldestVersion int `json:"oldestVersion,omitempty"`
	// ManagedBy identifies a producer whose secret cannot be edited or deleted.
	ManagedBy string `json:"managedBy,omitempty"`
}

// PutSecretInput replaces a typed secret without deploying applications.
type PutSecretInput struct {
	// Type is secret-group or registry.
	Type string `json:"type"`
	// Data is the complete JSON value map; registry token omission preserves it.
	Data map[string]string `json:"data"`
	// BaseVersion optionally requires the current publication; zero creates only.
	// A pointer distinguishes omission from the create-only precondition.
	BaseVersion *int `json:"baseVersion,omitempty"`
}

func groupSecret(group EnvironmentGroup) Secret {
	return Secret{Name: group.Name, Type: "secret-group", Data: variableValues(group.Values), Version: group.Version, ManagedBy: group.ManagedBy}
}

// ListSecrets lists both supported secret types, retaining existing group history.
func (manager Manager) ListSecrets(ctx context.Context, target EnvironmentGroupTarget) ([]Secret, error) {
	groups, err := manager.ListEnvironmentGroups(ctx, target)
	if err != nil {
		return nil, err
	}
	result := []Secret{}
	for _, group := range groups {
		result = append(result, groupSecret(group))
	}
	space, err := manager.GetSpace(ctx, target.Organization, target.Space)
	if err != nil {
		return nil, err
	}
	registries, err := manager.readRegistries(ctx, space.ID)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(registries)) {
		secret, err := manager.GetSecret(ctx, target, name, "latest")
		if err != nil {
			return nil, err
		}
		result = append(result, secret)
	}
	slices.SortFunc(result, func(a, b Secret) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

// GetSecret reads a group version or safe registry metadata from its scoped store.
func (manager Manager) GetSecret(ctx context.Context, target EnvironmentGroupTarget, name, version string) (Secret, error) {
	if err := validateConfigName(name); err != nil {
		return Secret{}, err
	}
	if _, err := configVersion(version); err != nil {
		return Secret{}, err
	}
	space, err := manager.GetSpace(ctx, target.Organization, target.Space)
	if err != nil {
		return Secret{}, err
	}
	registries, record, err := loadDocument[map[string]RegistryAuth](ctx, manager.database, "registry-auth", space.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}
	if auth, ok := registries[name]; ok {
		if version != "" && version != "latest" && version != fmt.Sprint(record.Revision) {
			return Secret{}, fmt.Errorf("%w: registry secrets expose only their current version", ErrNotFound)
		}
		credentials, err := manager.registryValues(ctx, name, auth)
		if err != nil {
			return Secret{}, err
		}
		return Secret{Name: name, Type: "registry", Data: map[string]string{"server": credentials.Server, "username": credentials.Username}, Version: int(record.Revision)}, nil
	}
	groupRecord, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", space.ID+"/"+name)
	if err != nil {
		return Secret{}, err
	}
	if groupRecord.Retired {
		return Secret{}, ErrNotFound
	}
	group, err := manager.GetEnvironmentGroup(ctx, target, name, version)
	secret := groupSecret(group)
	if groupRecord.DeletedThrough > 0 {
		secret.OldestVersion = groupRecord.DeletedThrough + 1
	}
	return secret, err
}

func (manager Manager) registryValues(ctx context.Context, name string, auth RegistryAuth) (RegistryAuth, error) {
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return RegistryAuth{}, err
	}
	entry, err := vault.ReadKv(ctx, "secret", auth.CredentialsRef, 1)
	if err != nil {
		return RegistryAuth{}, err
	}
	if !registryEntryMatches(entry, name) {
		return RegistryAuth{}, fmt.Errorf("%w: registry credentialsRef belongs to another secret", ErrInvalid)
	}
	return registryEntryValues(entry)
}

// PutSecret validates a typed JSON value map and publishes it to OpenBao.
func (manager Manager) PutSecret(ctx context.Context, target EnvironmentGroupTarget, name string, input PutSecretInput) (Secret, error) {
	if input.BaseVersion != nil && *input.BaseVersion < 0 {
		return Secret{}, fmt.Errorf("%w: baseVersion must not be negative", ErrInvalid)
	}
	switch input.Type {
	case "secret-group":
		group, err := manager.PublishEnvironmentGroup(ctx, PublishEnvironmentGroupInput{Target: target, Name: name, Values: sortedVariables(input.Data), BaseVersion: input.BaseVersion})
		return groupSecret(group), err
	case "registry":
		if err := validateConfigName(name); err != nil {
			return Secret{}, err
		}
		if input.Data["password"] != "" && input.Data["token"] != "" {
			return Secret{}, fmt.Errorf("%w: provide password or token, not both", ErrInvalid)
		}
		for key := range input.Data {
			if key != "server" && key != "username" && key != "password" && key != "token" {
				return Secret{}, fmt.Errorf("%w: registry data accepts server, username, and password (or legacy token)", ErrInvalid)
			}
		}
	default:
		return Secret{}, fmt.Errorf("%w: secret type must be secret-group or registry", ErrInvalid)
	}
	manager.runtime.secretMutex.Lock()
	defer manager.runtime.secretMutex.Unlock()
	space, err := manager.GetSpace(ctx, target.Organization, target.Space)
	if err != nil {
		return Secret{}, err
	}
	groupRecord, _, groupErr := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", space.ID+"/"+name)
	if groupErr == nil && !groupRecord.Retired {
		return Secret{}, fmt.Errorf("%w: secret already exists with type secret-group", ErrConflict)
	}
	if groupErr != nil && !errors.Is(groupErr, ErrNotFound) {
		return Secret{}, groupErr
	}
	registries, record, err := loadDocument[map[string]RegistryAuth](ctx, manager.database, "registry-auth", space.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}
	currentVersion := int(record.Revision)
	if registries[name].CredentialsRef == "" {
		currentVersion = 0
	}
	if input.BaseVersion != nil && *input.BaseVersion != currentVersion {
		return Secret{}, fmt.Errorf("%w: secret changed; reload before saving", ErrConflict)
	}
	auth := RegistryAuth{Server: input.Data["server"], Username: strings.TrimSpace(input.Data["username"]), Token: input.Data["password"]}
	if auth.Token == "" {
		auth.Token = input.Data["token"]
	}
	if auth.Server == "" && name == "ghcr.io" {
		auth.Server = "ghcr.io"
	}
	if auth.Token == "" && registries[name].CredentialsRef != "" {
		previous, err := manager.registryValues(ctx, name, registries[name])
		if err != nil {
			return Secret{}, err
		}
		server, err := normalizeRegistryServer(auth.Server)
		if err != nil {
			return Secret{}, err
		}
		if server != previous.Server {
			return Secret{}, fmt.Errorf("%w: enter a password/token when changing registry server", ErrInvalid)
		}
		auth.Token = previous.Token
	}
	staged, err := manager.stageRegistries(ctx, map[string]RegistryAuth{name: auth}, space.ID)
	if err != nil {
		return Secret{}, err
	}
	if err := manager.activateSpaceRegistriesUnlocked(ctx, space, staged); err != nil {
		return Secret{}, err
	}
	return manager.GetSecret(ctx, target, name, "latest")
}

// DeleteSecret revokes registry generations and projections or retires an unused
// secret group. It never changes pod templates or schedules application deployments.
func (manager Manager) DeleteSecret(ctx context.Context, target EnvironmentGroupTarget, name string, version int) error {
	if err := validateConfigName(name); err != nil {
		return err
	}
	if version < 0 {
		return fmt.Errorf("%w: version must be positive", ErrInvalid)
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	manager.runtime.secretMutex.Lock()
	defer manager.runtime.secretMutex.Unlock()
	space, client, err := manager.environmentGroupConnection(ctx, target)
	if err != nil {
		return err
	}
	registries, record, err := loadDocument[map[string]RegistryAuth](ctx, manager.database, "registry-auth", space.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, ok := registries[name]; ok {
		if version > 0 && version != int(record.Revision) {
			return fmt.Errorf("%w: secret changed; reload before deleting", ErrConflict)
		}
		vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return err
		}
		prefix := "borq/registry-auth/" + space.ID
		keys, err := vault.ListKvKeys(ctx, "secret", prefix)
		if err != nil {
			return err
		}
		refs := []string{}
		names := []string{}
		for _, key := range keys {
			auth := RegistryAuth{CredentialsRef: prefix + "/" + key}
			if err := validateRegistryReference(auth.CredentialsRef, space.ID); err != nil {
				return err
			}
			entry, err := vault.ReadKv(ctx, "secret", auth.CredentialsRef, 1)
			if errors.Is(err, openbao.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if registryEntryMatches(entry, name) {
				refs = append(refs, auth.CredentialsRef)
				names = append(names, registrySecretName(name, auth))
			}
		}
		if err := client.DeleteRegistrySecrets(ctx, space.Namespace, space.ID, names); err != nil {
			return err
		}
		for _, ref := range refs {
			if err := vault.DeleteKvMetadata(ctx, "secret", ref); err != nil && !errors.Is(err, openbao.ErrNotFound) {
				return err
			}
		}
		delete(registries, name)
		write, err := documentWrite("registry-auth", space.ID, registries, record.Revision)
		if err != nil {
			return err
		}
		return manager.database.Commit(ctx, []sqlite.Write{write})
	}
	group, stored, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", space.ID+"/"+name)
	if err != nil {
		return err
	}
	if group.PostgresID != "" {
		return errors.Join(ErrConflict, ErrEnvironmentGroupManaged)
	}
	storedSpace, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(target.Organization, target.Space))
	if err != nil {
		return err
	}
	for _, app := range space.Configuration.Apps {
		if app.Env.Version == "0" {
			continue
		}
		prefix := space.Organization + "-" + space.Name
		if storedSpace.EnvironmentGroupPrefix != "" {
			prefix = storedSpace.EnvironmentGroupPrefix
		}
		selector := prefix + "-" + app.Name
		if app.Env.Name != "" {
			selector = app.Env.Name
		}
		if selector == name {
			return fmt.Errorf("%w: secret is used by application %s", ErrConflict, app.Name)
		}
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return err
	}
	path, err := manager.resolveEnvironmentGroupPath(ctx, space.ID, name)
	if err != nil {
		return err
	}
	metadata, err := vault.ReadKvMetadata(ctx, mount, path)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	if err == nil && version > 0 && version != metadata.CurrentVersion {
		return fmt.Errorf("%w: secret changed; reload before deleting", ErrConflict)
	}
	versions := []int{}
	for number := max(1, metadata.OldestVersion); number <= metadata.CurrentVersion; number++ {
		versions = append(versions, number)
	}
	if err := vault.DestroyKvVersions(ctx, mount, path, versions); err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	// Keep the original vault path and version counter across deletion/recreation.
	group.Retired = true
	group.DeletedThrough = max(group.DeletedThrough, metadata.CurrentVersion)
	write, err := documentWrite("environment-group", space.ID+"/"+name, group, stored.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}
