package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/stefanmcshane/borq/business/server/platform"
)

// Publications retain their accepted base and private payload across worker restarts.
type managementSecretPublication struct {
	BaseVersion    int    `json:"baseVersion"`
	Version        int    `json:"version"`
	CredentialsRef string `json:"credentialsRef"`
}

// A separate immutable receipt keeps an installed or queued version available even
// after ordinary secret-group history retention removes that publication.
type pocketIdEnvironment struct {
	Name           string `json:"name"`
	Version        int    `json:"version"`
	Selection      string `json:"selection"`
	CredentialsRef string `json:"credentialsRef"`
}

func managementOrganizationHasSecrets(configuration ManagementConfiguration, organization string) bool {
	for _, group := range configuration.SecretGroups {
		if group.Organization == organization {
			return true
		}
	}
	return false
}

func validateManagementSecretGroups(configuration ManagementConfiguration) error {
	organizations := map[string]bool{}
	for _, organization := range configuration.Organizations {
		organizations[organization.Name] = true
	}
	groups := map[string]bool{}
	for _, group := range configuration.SecretGroups {
		if !organizations[group.Organization] {
			return fmt.Errorf("%w: management secret group must reference an organization in this configuration", ErrInvalid)
		}
		if err := validateConfigName(group.Name); err != nil {
			return err
		}
		key := group.Organization + "/" + group.Name
		if groups[key] {
			return fmt.Errorf("%w: duplicate management secret group", ErrInvalid)
		}
		groups[key] = true
		if group.Data != nil && group.Version != "" {
			return fmt.Errorf("%w: supply management secret group data or version, not both", ErrInvalid)
		}
		if _, err := configVersion(group.Version); err != nil {
			return err
		}
		if err := validateConfigValues(sortedVariables(group.Data)); err != nil {
			return err
		}
	}
	for _, organization := range configuration.Organizations {
		addon := organization.ManagedPocketId
		if addon == nil {
			continue
		}
		if addon.Env != nil {
			if err := validateConfigName(addon.Env.Name); err != nil {
				return err
			}
			if _, err := configVersion(addon.Env.Version); err != nil {
				return err
			}
		}
	}
	return nil
}

func (manager Manager) stageManagementSecretGroups(ctx context.Context, configuration *ManagementConfiguration, authentication map[string]managementAuthentication) (map[string]managementSecretPublication, map[string]pocketIdEnvironment, error) {
	publications := map[string]managementSecretPublication{}
	environments := map[string]pocketIdEnvironment{}
	pending := map[string]Secret{}
	for index := range configuration.SecretGroups {
		group := &configuration.SecretGroups[index]
		owner := authentication[group.Organization].OrganizationID
		if group.Data == nil {
			selected, err := manager.resolveManagementSecret(ctx, owner, group.Name, group.Version)
			if err != nil {
				return nil, nil, err
			}
			group.Version = strconv.Itoa(selected.Version)
			continue
		}
		current, err := manager.resolveManagementSecret(ctx, owner, group.Name, "latest")
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, nil, err
		}
		version := current.Version
		if version == 0 || !maps.Equal(current.Data, group.Data) {
			version++
		}
		encoded, err := json.Marshal(group.Data)
		if err != nil {
			return nil, nil, err
		}
		ref, err := manager.stageManagementSecret(ctx, "management-secret-group", owner, group.Name+"@"+strconv.Itoa(version), string(encoded))
		if err != nil {
			return nil, nil, err
		}
		key := group.Organization + "/" + group.Name
		publications[key] = managementSecretPublication{BaseVersion: current.Version, Version: version, CredentialsRef: ref}
		pending[key] = Secret{Name: group.Name, Type: "secret-group", Data: group.Data, Version: version}
		group.Version, group.Data = strconv.Itoa(version), nil
	}
	for _, organization := range configuration.Organizations {
		addon := organization.ManagedPocketId
		if addon == nil {
			continue
		}
		if addon.Env == nil {
			continue
		}
		owner := authentication[organization.Name].OrganizationID
		selected, exists := pending[organization.Name+"/"+addon.Env.Name]
		number, err := configVersion(addon.Env.Version)
		if err != nil {
			return nil, nil, err
		}
		var environment pocketIdEnvironment
		if !exists || (number != 0 && number != selected.Version) {
			environment, err = manager.resolvePocketIdEnvironment(ctx, owner, *addon.Env)
		} else {
			environment, err = manager.stagePocketIdEnvironment(ctx, owner, *addon.Env, selected)
		}
		if err != nil {
			return nil, nil, err
		}
		environments[organization.Name] = environment
	}
	return publications, environments, nil
}

func (manager Manager) applyManagementSecretGroup(ctx context.Context, group ManagementSecretGroup, accepted managementAuthentication, publication managementSecretPublication) error {
	if _, _, err := manager.managementOidcOwner(ctx, group.Organization, accepted); err != nil {
		return err
	}
	if publication.CredentialsRef == "" {
		return nil
	}
	if publication.Version < 1 || publication.BaseVersion < 0 || publication.Version < publication.BaseVersion || publication.Version > publication.BaseVersion+1 || group.Version != strconv.Itoa(publication.Version) {
		return fmt.Errorf("%w: invalid accepted management secret publication", ErrInvalid)
	}
	encoded, err := manager.readManagementSecret(ctx, "management-secret-group", accepted.OrganizationID, publication.CredentialsRef, group.Name+"@"+group.Version)
	if err != nil {
		return err
	}
	var data map[string]string
	if json.Unmarshal([]byte(encoded), &data) != nil || data == nil {
		return fmt.Errorf("%w: invalid accepted management secret data", ErrInvalid)
	}
	current, err := manager.resolveManagementSecret(ctx, accepted.OrganizationID, group.Name, "latest")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	// A crash after publication must replay without publishing a second version.
	if current.Version == publication.Version && maps.Equal(current.Data, data) {
		return nil
	}
	_, err = manager.PutManagementSecret(ctx, group.Organization, group.Name, PutSecretInput{Type: "secret-group", Data: data, BaseVersion: &publication.BaseVersion})
	return err
}

func (environment pocketIdEnvironment) valid(organizationID string) bool {
	if environment.Name == "" {
		return environment == (pocketIdEnvironment{})
	}
	selection, err := configVersion(environment.Selection)
	return validateConfigName(environment.Name) == nil && environment.Version > 0 && err == nil && (selection == 0 || selection == environment.Version) && validManagementSecretRef("pocketid-environment", organizationID, environment.CredentialsRef)
}

func (environment pocketIdEnvironment) reference(concrete bool) *EnvReference {
	if environment.Name == "" {
		return nil
	}
	version := environment.Selection
	if concrete {
		version = strconv.Itoa(environment.Version)
	}
	return &EnvReference{Name: environment.Name, Version: version}
}

// Exact installed selections remain reapplicable after the group's ordinary
// history expires. A latest selection intentionally resolves a new publication.
func (manager Manager) resolvePocketIdEnvironment(ctx context.Context, organizationID string, reference EnvReference) (pocketIdEnvironment, error) {
	number, err := configVersion(reference.Version)
	if err != nil {
		return pocketIdEnvironment{}, err
	}
	if number != 0 {
		stored, _, err := manager.readPocketId(ctx, organizationID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return pocketIdEnvironment{}, err
		}
		if stored.Environment.Name == reference.Name && stored.Environment.Version == number {
			environment := stored.Environment
			environment.Selection = reference.Version
			_, err := manager.readPocketIdEnvironment(ctx, organizationID, environment)
			return environment, err
		}
	}
	selected, err := manager.resolveManagementSecret(ctx, organizationID, reference.Name, reference.Version)
	if err != nil {
		return pocketIdEnvironment{}, err
	}
	return manager.stagePocketIdEnvironment(ctx, organizationID, reference, selected)
}

func (manager Manager) stagePocketIdEnvironment(ctx context.Context, organizationID string, reference EnvReference, selected Secret) (pocketIdEnvironment, error) {
	if selected.Name != reference.Name || selected.Type != "secret-group" || selected.Version < 1 {
		return pocketIdEnvironment{}, fmt.Errorf("%w: invalid management secret selection", ErrInvalid)
	}
	if err := platform.ValidatePocketIdEnvironment(selected.Data); err != nil {
		return pocketIdEnvironment{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	selection := reference.Version
	if selection == "" {
		selection = "latest"
	}
	environment := pocketIdEnvironment{Name: reference.Name, Version: selected.Version, Selection: selection}
	stored, _, err := manager.readPocketId(ctx, organizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return pocketIdEnvironment{}, err
	}
	if stored.Environment.Name == environment.Name && stored.Environment.Version == environment.Version {
		environment.CredentialsRef = stored.Environment.CredentialsRef
		return environment, nil
	}
	encoded, err := json.Marshal(selected.Data)
	if err != nil {
		return pocketIdEnvironment{}, err
	}
	environment.CredentialsRef, err = manager.stageManagementSecret(ctx, "pocketid-environment", organizationID, environment.Name+"@"+strconv.Itoa(environment.Version), string(encoded))
	return environment, err
}

func (manager Manager) readPocketIdEnvironment(ctx context.Context, organizationID string, environment pocketIdEnvironment) (map[string]string, error) {
	if !environment.valid(organizationID) {
		return nil, fmt.Errorf("%w: invalid Pocket ID environment ownership", ErrInvalid)
	}
	if environment.Name == "" {
		return nil, nil
	}
	encoded, err := manager.readManagementSecret(ctx, "pocketid-environment", organizationID, environment.CredentialsRef, environment.Name+"@"+strconv.Itoa(environment.Version))
	if err != nil {
		return nil, err
	}
	var data map[string]string
	if json.Unmarshal([]byte(encoded), &data) != nil || data == nil || platform.ValidatePocketIdEnvironment(data) != nil {
		return nil, fmt.Errorf("%w: invalid Pocket ID environment configuration", ErrInvalid)
	}
	return data, nil
}
