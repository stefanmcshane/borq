package apps

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// GetManagementConfiguration exports accepted resources and vault references without credentials.
func (manager Manager) GetManagementConfiguration(ctx context.Context) (ManagementConfiguration, error) {
	configuration := ManagementConfiguration{Organizations: []ManagementOrganization{}}
	overrides, _, err := loadDocument[map[string]platform.ComponentOverride](ctx, manager.database, "management-components", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementConfiguration{}, err
	}
	configuration.Components = overrides
	storage, err := manager.GetManagementS3(ctx)
	if err == nil {
		configuration.S3 = &ManagementS3Input{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: storage.PathStyle, CredentialsRef: storage.CredentialsRef}
	} else if !errors.Is(err, ErrNotFound) {
		return ManagementConfiguration{}, err
	}
	offsite, _, offsiteErr := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
	if offsiteErr == nil {
		input := offsite.input()
		status, statusErr := manager.GetManagementOffsiteStatus(ctx)
		if statusErr != nil {
			return ManagementConfiguration{}, statusErr
		}
		input.Status = &status
		configuration.Offsite = &input
	} else if !errors.Is(offsiteErr, ErrNotFound) {
		return ManagementConfiguration{}, offsiteErr
	}
	organizations, err := manager.ListOrganizations(ctx)
	if err != nil {
		return ManagementConfiguration{}, err
	}
	for _, organization := range organizations {
		enabled := organization.ManagedAddons
		entry := ManagementOrganization{Name: organization.Name, ManagedAddons: &enabled, Spaces: []ManagementSpace{}}
		entry.Clusters, err = manager.ListClusters(ctx, organization.Name)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		spaces, err := manager.ListSpaces(ctx, organization.Name)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		for _, space := range spaces {
			fragment := ManagementSpace{Name: space.Name, Cluster: space.Cluster}
			for _, application := range space.Configuration.Apps {
				data, err := json.Marshal(application)
				if err != nil {
					return ManagementConfiguration{}, err
				}
				var patch AppPatch
				if err := json.Unmarshal(data, &patch); err != nil {
					return ManagementConfiguration{}, err
				}
				fragment.Apps = append(fragment.Apps, patch)
			}
			addons, err := manager.ListAddons(ctx, organization.Name, space.Name)
			if err != nil {
				return ManagementConfiguration{}, err
			}
			for _, addon := range addons {
				fragment.CustomAddons = append(fragment.CustomAddons, NamedCustomAddon{Name: addon.Name, AddonInput: addon.Specification})
			}
			databases, err := manager.ListPostgres(ctx, organization.Name, space.Name)
			if err != nil {
				return ManagementConfiguration{}, err
			}
			for _, database := range databases {
				fragment.ManagedAddons.Postgres = append(fragment.ManagedAddons.Postgres, NamedPostgres{Name: database.Name, PostgresInput: database.Specification})
			}
			entry.Spaces = append(entry.Spaces, fragment)
		}
		configuration.Organizations = append(configuration.Organizations, entry)
	}
	return configuration, nil
}

func (manager Manager) putComponentOverrides(ctx context.Context, overrides map[string]platform.ComponentOverride) error {
	if err := platform.ValidateComponentOverrides(overrides); err != nil {
		return err
	}
	previous, record, err := loadDocument[map[string]platform.ComponentOverride](ctx, manager.database, "management-components", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if previous == nil {
		previous = map[string]platform.ComponentOverride{}
	}
	for name, override := range overrides {
		entry := previous[name]
		if override.Repo != "" {
			entry.Repo = override.Repo
		}
		if override.Chart != "" {
			entry.Chart = override.Chart
		}
		if override.Version != "" {
			entry.Version = override.Version
		}
		if override.Values != nil {
			entry.Values = override.Values
		}
		previous[name] = entry
	}
	write, err := documentWrite("management-components", "shared", previous, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

// GetPlatformSettings returns current safe component overrides for lifecycle workers.
func (manager Manager) GetPlatformSettings(ctx context.Context) (platform.Settings, error) {
	settings := manager.runtime.platformSettings
	overrides, _, err := loadDocument[map[string]platform.ComponentOverride](ctx, manager.database, "management-components", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return platform.Settings{}, err
	}
	if len(overrides) > 0 {
		settings.Components = overrides
	}
	storage, err := manager.GetManagementS3(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return platform.Settings{}, err
	}
	if err == nil && storage.CredentialsRef != "borq/platform/rustfs" {
		settings.S3 = platform.S3Settings{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: storage.PathStyle, CredentialsRef: storage.CredentialsRef}
	}
	return settings, nil
}
