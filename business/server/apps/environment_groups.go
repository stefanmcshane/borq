package apps

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Keep the latest version and this many earlier versions available for deployment.
const previousEnvironmentGroupVersions = 10

// ErrEnvironmentGroupManaged lets transports explain producer ownership without exposing backend errors.
var ErrEnvironmentGroupManaged = errors.New("environment group is managed by PostgreSQL and follows rotating credentials")

func validateEnvironmentGroupVersion(name string, version, latest int) error {
	if version < 1 || version > latest {
		return fmt.Errorf("%w: environment group %s version %d is unavailable", ErrNotFound, name, version)
	}
	if latest-version > previousEnvironmentGroupVersions {
		return fmt.Errorf("%w: environment group %s version %d is older than the %d previous versions; latest is %d and oldest allowed is %d", ErrInvalid, name, version, previousEnvironmentGroupVersions, latest, latest-previousEnvironmentGroupVersions)
	}
	return nil
}

// EnvironmentGroupTarget scopes environment-variable configuration names to an organization space.
type EnvironmentGroupTarget struct {
	// Organization owns the space.
	Organization string
	// Space selects the registered cluster and namespace.
	Space string
}

// EnvironmentGroup is one published, immutable version of a named environment group.
type EnvironmentGroup struct {
	// Name selects the configuration family independently of an application image.
	Name string `json:"name"`
	// ManagedBy identifies a producer; managed values are read-only and can be cloned.
	ManagedBy string `json:"managedBy,omitempty"`
	// Version is the existing KV v2 version allocated when the values were published.
	Version int `json:"version"`
	// Values exposes the complete values to trusted API and CLI callers.
	Values []Variable `json:"values"`
}

// PublishEnvironmentGroupInput publishes a complete declarative values document.
type PublishEnvironmentGroupInput struct {
	// Target selects the space containing the vault connection.
	Target EnvironmentGroupTarget
	// Name identifies the configuration family to create or update.
	Name string
	// Values replaces the complete configuration; omitted keys are removed.
	Values []Variable
	// BaseVersion is optional; a pointer distinguishes an expected new key (zero) from no precondition.
	BaseVersion *int
}

// CloneEnvironmentGroupInput copies a published configuration into another named family.
type CloneEnvironmentGroupInput struct {
	// Target scopes both source and destination names.
	Target EnvironmentGroupTarget
	// Name selects the destination family, which must not already have a version.
	Name string
	// SourceName selects the existing configuration family.
	SourceName string
	// SourceVersion selects a concrete version or latest.
	SourceVersion string
}

type environmentGroupRecord struct {
	SpaceID string `json:"spaceId"`
	Name    string `json:"name"`
	// VaultPath retains the original KV history when a group predates the rename.
	VaultPath    string `json:"vaultPath,omitempty"`
	PostgresID   string `json:"postgresId,omitempty"`
	PostgresName string `json:"postgresName,omitempty"`
}

func (manager Manager) environmentGroupConnection(ctx context.Context, target EnvironmentGroupTarget) (Space, kubeapps.Client, error) {
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(target.Organization, target.Space))
	if err != nil {
		return Space{}, kubeapps.Client{}, err
	}
	if stored.Space.Cluster == "" {
		return Space{}, kubeapps.Client{}, fmt.Errorf("%w: space %s is not attached to a cluster", ErrInvalid, target.Space)
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", target.Organization+"/"+stored.Space.Cluster)
	if err != nil {
		return Space{}, kubeapps.Client{}, err
	}
	client, err := newClusterClient(cluster)
	return stored.Space, client, err
}

func environmentGroupPath(targetID, name string) string {
	return "borq/environment-groups/" + targetID + "/" + name
}

// Legacy vault paths must remain stable so existing version numbers and saved
// plans continue to select the same immutable secret history.
func legacyEnvironmentGroupPath(targetID, name string) string {
	return "borq/env-configs/" + targetID + "/" + name
}

func (manager Manager) resolveEnvironmentGroupPath(ctx context.Context, targetID, name string) (string, error) {
	record, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", targetID+"/"+name)
	if errors.Is(err, ErrNotFound) {
		return environmentGroupPath(targetID, name), nil
	}
	if err != nil {
		return "", err
	}
	if record.VaultPath != "" {
		return record.VaultPath, nil
	}
	return environmentGroupPath(targetID, name), nil
}

func validateConfigName(name string) error {
	if len(name) > 253 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return fmt.Errorf("%w: environment group name must be a DNS name of at most 253 characters", ErrInvalid)
	}
	return nil
}

func configVersion(version string) (int, error) {
	if version == "" || version == "latest" {
		return 0, nil
	}
	number, err := strconv.Atoi(version)
	if err != nil || number < 1 || strconv.Itoa(number) != version {
		return 0, fmt.Errorf("%w: environment group version must be latest or a positive decimal string", ErrInvalid)
	}
	return number, nil
}

func validateConfigValues(values []Variable) error {
	names := map[string]bool{}
	for _, variable := range values {
		if len(validation.IsEnvVarName(variable.Name)) > 0 || names[variable.Name] {
			return fmt.Errorf("%w: environment group requires valid, unique variable names", ErrInvalid)
		}
		names[variable.Name] = true
	}
	return nil
}

func (manager Manager) registerEnvironmentGroup(ctx context.Context, environmentID, name string) error {
	record, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", environmentID+"/"+name)
	if err == nil {
		if record.PostgresID != "" {
			return errors.Join(ErrConflict, ErrEnvironmentGroupManaged)
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	write, err := documentWrite("environment-group", environmentID+"/"+name, environmentGroupRecord{SpaceID: environmentID, Name: name}, 0)
	if err != nil {
		return err
	}
	err = manager.database.Commit(ctx, []sqlite.Write{write})
	if errors.Is(err, sqlite.ErrConflict) {
		record, _, err = loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", environmentID+"/"+name)
		if err != nil {
			return err
		}
		if record.PostgresID != "" {
			return errors.Join(ErrConflict, ErrEnvironmentGroupManaged)
		}
		return nil
	}
	return err
}

// PublishEnvironmentGroup writes a new version only when the complete values change.
// Check-and-set prevents a concurrent publisher from silently losing an update.
func (manager Manager) PublishEnvironmentGroup(ctx context.Context, input PublishEnvironmentGroupInput) (EnvironmentGroup, error) {
	if err := validateConfigName(input.Name); err != nil {
		return EnvironmentGroup{}, err
	}
	if err := validateConfigValues(input.Values); err != nil {
		return EnvironmentGroup{}, err
	}
	if input.BaseVersion != nil && *input.BaseVersion < 0 {
		return EnvironmentGroup{}, fmt.Errorf("%w: baseVersion must not be negative", ErrInvalid)
	}
	environment, client, err := manager.environmentGroupConnection(ctx, input.Target)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	if err := manager.registerEnvironmentGroup(ctx, environment.ID, input.Name); err != nil {
		return EnvironmentGroup{}, err
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	path, err := manager.resolveEnvironmentGroupPath(ctx, environment.ID, input.Name)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	metadata, err := vault.ReadKvMetadata(ctx, mount, path)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return EnvironmentGroup{}, err
	}
	if input.BaseVersion != nil && *input.BaseVersion != metadata.CurrentVersion {
		return EnvironmentGroup{}, fmt.Errorf("%w: environment group changed since its base version", ErrConflict)
	}
	values := variableValues(input.Values)
	if metadata.CurrentVersion > 0 {
		entry, readErr := vault.ReadKv(ctx, mount, path, metadata.CurrentVersion)
		if readErr != nil && !errors.Is(readErr, openbao.ErrNotFound) {
			return EnvironmentGroup{}, readErr
		}
		if readErr == nil && maps.Equal(entry.Values, values) {
			return EnvironmentGroup{Name: input.Name, Version: entry.Version, Values: sortedVariables(entry.Values)}, nil
		}
	}
	if err := vault.SetKvRetention(ctx, mount, path, previousEnvironmentGroupVersions+1); err != nil {
		return EnvironmentGroup{}, err
	}
	identity, err := newId()
	if err != nil {
		return EnvironmentGroup{}, err
	}
	entry, err := vault.WriteKv(ctx, mount, path, openbao.WriteKvInput{Values: values, PlanID: "config/" + identity, CheckVersion: metadata.CurrentVersion})
	if errors.Is(err, openbao.ErrConflict) {
		return EnvironmentGroup{}, fmt.Errorf("%w: environment group was published concurrently; fetch the latest version before publishing", ErrConflict)
	}
	if err != nil {
		return EnvironmentGroup{}, err
	}
	return EnvironmentGroup{Name: input.Name, Version: entry.Version, Values: sortedVariables(entry.Values)}, nil
}

// GetEnvironmentGroup retrieves full values for latest or an existing immutable version.
func (manager Manager) GetEnvironmentGroup(ctx context.Context, target EnvironmentGroupTarget, name, version string) (EnvironmentGroup, error) {
	if err := validateConfigName(name); err != nil {
		return EnvironmentGroup{}, err
	}
	number, err := configVersion(version)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	environment, client, err := manager.environmentGroupConnection(ctx, target)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	record, _, recordErr := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", environment.ID+"/"+name)
	if recordErr != nil && !errors.Is(recordErr, ErrNotFound) {
		return EnvironmentGroup{}, recordErr
	}
	if record.PostgresID != "" && number != 0 {
		return EnvironmentGroup{}, fmt.Errorf("%w: managed PostgreSQL credentials are available only as latest", ErrInvalid)
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	path, err := manager.resolveEnvironmentGroupPath(ctx, environment.ID, name)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	entry, err := vault.ReadKv(ctx, mount, path, number)
	if errors.Is(err, openbao.ErrNotFound) {
		return EnvironmentGroup{}, fmt.Errorf("%w: environment group %s version %s is unavailable", ErrNotFound, name, version)
	}
	if err != nil {
		return EnvironmentGroup{}, err
	}
	managedBy := ""
	if record.PostgresID != "" {
		managedBy = "postgres/" + record.PostgresName
	}
	return EnvironmentGroup{Name: name, ManagedBy: managedBy, Version: entry.Version, Values: sortedVariables(entry.Values)}, nil
}

// CloneEnvironmentGroup publishes a complete copy under a new family name without deploying it.
func (manager Manager) CloneEnvironmentGroup(ctx context.Context, input CloneEnvironmentGroupInput) (EnvironmentGroup, error) {
	source, err := manager.GetEnvironmentGroup(ctx, input.Target, input.SourceName, input.SourceVersion)
	if err != nil {
		return EnvironmentGroup{}, err
	}
	if source.ManagedBy != "" {
		return EnvironmentGroup{}, fmt.Errorf("%w: rotating PostgreSQL credentials cannot be cloned into a pinned environment group", ErrConflict)
	}
	expected := 0
	return manager.PublishEnvironmentGroup(ctx, PublishEnvironmentGroupInput{Target: input.Target, Name: input.Name, Values: source.Values, BaseVersion: &expected})
}

// ListEnvironmentGroups returns the latest full values for published configuration families.
func (manager Manager) ListEnvironmentGroups(ctx context.Context, target EnvironmentGroupTarget) ([]EnvironmentGroup, error) {
	environment, _, err := manager.environmentGroupConnection(ctx, target)
	if err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "environment-group")
	if err != nil {
		return nil, err
	}
	configs := []EnvironmentGroup{}
	for _, record := range records {
		if !strings.HasPrefix(record.Key, environment.ID+"/") {
			continue
		}
		name := strings.TrimPrefix(record.Key, environment.ID+"/")
		config, err := manager.GetEnvironmentGroup(ctx, target, name, "latest")
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, nil
}
