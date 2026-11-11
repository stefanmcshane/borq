package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// The index contains identity only; OpenBao remains the authority for values and versions.
type managementSecretGroup struct {
	OrganizationID string `json:"organizationId"`
	Name           string `json:"name"`
}

func (manager Manager) managementSecretOrganization(ctx context.Context, name string) (Organization, error) {
	if err := validateNames(name); err != nil {
		return Organization{}, err
	}
	organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil {
		return Organization{}, err
	}
	if organization.Name != name {
		return Organization{}, errors.New("management secret organization ownership is invalid")
	}
	organization, err = manager.ensureOrganizationIdentity(ctx, organization)
	if err != nil {
		return Organization{}, err
	}
	if organization.Name != name || len(organization.ID) != 32 || strings.Trim(organization.ID, "0123456789abcdef") != "" {
		return Organization{}, errors.New("management secret organization ownership is invalid")
	}
	return organization, nil
}

func managementSecretPath(organizationID, name string) string {
	return "borq/management-secret-groups/" + organizationID + "/" + name
}

// ListManagementSecrets lists published groups in one organization's management vault,
// without requiring a deployment space or cluster registration.
func (manager Manager) ListManagementSecrets(ctx context.Context, organization string) ([]Secret, error) {
	owner, err := manager.managementSecretOrganization(ctx, organization)
	if err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "management-secret-group")
	if err != nil {
		return nil, err
	}
	secrets := []Secret{}
	for _, record := range records {
		name, exists := strings.CutPrefix(record.Key, owner.ID+"/")
		if !exists {
			continue
		}
		secret, err := manager.resolveManagementSecret(ctx, owner.ID, name, "latest")
		// Registration precedes publication so a failed vault write is safe to retry.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	slices.SortFunc(secrets, func(left, right Secret) int { return strings.Compare(left.Name, right.Name) })
	return secrets, nil
}

// GetManagementSecret returns a complete immutable publication to trusted administrators.
func (manager Manager) GetManagementSecret(ctx context.Context, organization, name, version string) (Secret, error) {
	if err := validateConfigName(name); err != nil {
		return Secret{}, err
	}
	if _, err := configVersion(version); err != nil {
		return Secret{}, err
	}
	owner, err := manager.managementSecretOrganization(ctx, organization)
	if err != nil {
		return Secret{}, err
	}
	return manager.resolveManagementSecret(ctx, owner.ID, name, version)
}

// Internal consumers receive the concrete publication number even when selecting latest.
// A stale organization identity can never resolve credentials for its replacement.
func (manager Manager) resolveManagementSecret(ctx context.Context, organizationID, name, version string) (Secret, error) {
	if len(organizationID) != 32 || strings.Trim(organizationID, "0123456789abcdef") != "" {
		return Secret{}, fmt.Errorf("%w: invalid management secret organization identity", ErrInvalid)
	}
	if err := validateConfigName(name); err != nil {
		return Secret{}, err
	}
	number, err := configVersion(version)
	if err != nil {
		return Secret{}, err
	}
	organizations, err := manager.database.List(ctx, "organization")
	if err != nil {
		return Secret{}, err
	}
	found := false
	for _, record := range organizations {
		var owner Organization
		if json.Unmarshal(record.Value, &owner) != nil {
			return Secret{}, errors.New("management secret organization ownership is invalid")
		}
		if owner.ID != organizationID {
			continue
		}
		if found || owner.Name != record.Key || validateNames(owner.Name) != nil {
			return Secret{}, errors.New("management secret organization ownership is invalid")
		}
		found = true
	}
	if !found {
		return Secret{}, fmt.Errorf("%w: management secret organization no longer exists", ErrNotFound)
	}
	stored, _, err := loadDocument[managementSecretGroup](ctx, manager.database, "management-secret-group", organizationID+"/"+name)
	if err != nil {
		return Secret{}, err
	}
	if stored.OrganizationID != organizationID || stored.Name != name {
		return Secret{}, errors.New("management secret ownership is invalid")
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return Secret{}, errors.New("management secret storage is unavailable")
	}
	path := managementSecretPath(organizationID, name)
	metadata, err := vault.ReadKvMetadata(ctx, "secret", path)
	if errors.Is(err, openbao.ErrNotFound) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, errors.New("could not read management secret history")
	}
	if number == 0 {
		number = metadata.CurrentVersion
	}
	if err := validateEnvironmentGroupVersion(name, number, metadata.CurrentVersion); err != nil {
		return Secret{}, err
	}
	entry, err := vault.ReadKv(ctx, "secret", path, number)
	if errors.Is(err, openbao.ErrNotFound) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, errors.New("could not read management secret")
	}
	return Secret{Name: name, Type: "secret-group", Data: entry.Values, Version: entry.Version, OldestVersion: max(1, metadata.OldestVersion, metadata.CurrentVersion-previousEnvironmentGroupVersions)}, nil
}

// PutManagementSecret publishes a complete value map in management OpenBao.
// Unchanged values retain their version; optional baseVersion and vault CAS prevent lost updates.
func (manager Manager) PutManagementSecret(ctx context.Context, organization, name string, input PutSecretInput) (Secret, error) {
	if input.Type != "secret-group" {
		return Secret{}, fmt.Errorf("%w: management secrets support only secret-group", ErrInvalid)
	}
	if err := validateConfigName(name); err != nil {
		return Secret{}, err
	}
	if input.Data == nil {
		return Secret{}, fmt.Errorf("%w: management secret data must be an object", ErrInvalid)
	}
	if err := validateConfigValues(sortedVariables(input.Data)); err != nil {
		return Secret{}, err
	}
	if input.BaseVersion != nil && *input.BaseVersion < 0 {
		return Secret{}, fmt.Errorf("%w: baseVersion must not be negative", ErrInvalid)
	}
	manager.runtime.secretMutex.Lock()
	defer manager.runtime.secretMutex.Unlock()
	owner, err := manager.managementSecretOrganization(ctx, organization)
	if err != nil {
		return Secret{}, err
	}
	key := owner.ID + "/" + name
	stored, _, err := loadDocument[managementSecretGroup](ctx, manager.database, "management-secret-group", key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}
	if err == nil && (stored.OrganizationID != owner.ID || stored.Name != name) {
		return Secret{}, errors.New("management secret ownership is invalid")
	}
	if errors.Is(err, ErrNotFound) {
		write, err := documentWrite("management-secret-group", key, managementSecretGroup{OrganizationID: owner.ID, Name: name}, 0)
		if err != nil {
			return Secret{}, err
		}
		if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
			return Secret{}, translateStoreError(err)
		}
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return Secret{}, errors.New("management secret storage is unavailable")
	}
	path := managementSecretPath(owner.ID, name)
	metadata, err := vault.ReadKvMetadata(ctx, "secret", path)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return Secret{}, errors.New("could not read management secret history")
	}
	if input.BaseVersion != nil && *input.BaseVersion != metadata.CurrentVersion {
		return Secret{}, fmt.Errorf("%w: management secret changed since its base version", ErrConflict)
	}
	if metadata.CurrentVersion > 0 {
		current, err := vault.ReadKv(ctx, "secret", path, metadata.CurrentVersion)
		if err != nil {
			return Secret{}, errors.New("could not read current management secret")
		}
		if maps.Equal(current.Values, input.Data) {
			return Secret{Name: name, Type: "secret-group", Data: current.Values, Version: current.Version, OldestVersion: max(1, metadata.OldestVersion, metadata.CurrentVersion-previousEnvironmentGroupVersions)}, nil
		}
	}
	if err := vault.SetKvRetention(ctx, "secret", path, previousEnvironmentGroupVersions+1); err != nil {
		return Secret{}, errors.New("could not configure management secret history")
	}
	identity, err := newId()
	if err != nil {
		return Secret{}, err
	}
	entry, err := vault.WriteKv(ctx, "secret", path, openbao.WriteKvInput{Values: input.Data, PlanID: "management-secret/" + identity, CheckVersion: metadata.CurrentVersion})
	if errors.Is(err, openbao.ErrConflict) {
		return Secret{}, fmt.Errorf("%w: management secret was published concurrently; reload before saving", ErrConflict)
	}
	if err != nil {
		return Secret{}, errors.New("could not publish management secret")
	}
	return Secret{Name: name, Type: "secret-group", Data: entry.Values, Version: entry.Version, OldestVersion: max(1, metadata.OldestVersion, entry.Version-previousEnvironmentGroupVersions)}, nil
}
