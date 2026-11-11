package apps

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// GetManagementConfiguration exports accepted resources and private credential references without credentials.
func (manager Manager) GetManagementConfiguration(ctx context.Context) (ManagementConfiguration, error) {
	configuration := ManagementConfiguration{Organizations: []ManagementOrganization{}}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return ManagementConfiguration{}, err
	}
	configuration.BorqPublicURL = settings.BorqPublicURL
	if configuration.BorqPublicURL == "" && settings.Network.Cloudflare != nil {
		if host := platform.ManagementPublicHostname(settings); host != "" {
			configuration.BorqPublicURL = "https://" + host
		}
	}
	if settings.Network.Tailscale != nil || settings.Network.Cloudflare != nil {
		network := settings.Network
		configuration.Network = &network
	}
	overrides, _, err := loadDocument[map[string]platform.ComponentOverride](ctx, manager.database, "management-components", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementConfiguration{}, err
	}
	configuration.Components = overrides
	configuration.Registries, err = manager.readRegistries(ctx, "management")
	if err != nil {
		return ManagementConfiguration{}, err
	}
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
	backups, _, backupErr := loadDocument[ManagementBackupInput](ctx, manager.database, "management-backups", "shared")
	if backupErr == nil {
		status, err := manager.managementBackupStatus(ctx)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		backups.Status = &status
		configuration.Backups = &backups
	} else if !errors.Is(backupErr, ErrNotFound) {
		return ManagementConfiguration{}, backupErr
	}
	organizations, err := manager.ListOrganizations(ctx)
	if err != nil {
		return ManagementConfiguration{}, err
	}
	for _, organization := range organizations {
		enabled := organization.ManagedAddons
		entry := ManagementOrganization{Name: organization.Name, ManagedAddons: &enabled, Spaces: []ManagementSpace{}}
		if err := manager.exportManagementAuthentication(ctx, organization, &entry); err != nil {
			return ManagementConfiguration{}, err
		}
		groups, err := manager.ListManagementSecrets(ctx, organization.Name)
		if err != nil {
			return ManagementConfiguration{}, err
		}
		for _, group := range groups {
			configuration.SecretGroups = append(configuration.SecretGroups, ManagementSecretGroup{Organization: organization.Name, Name: group.Name, Version: strconv.Itoa(group.Version)})
		}
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
			fragment.Registries, err = manager.readRegistries(ctx, space.ID)
			if err != nil {
				return ManagementConfiguration{}, err
			}
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
			providers, err := manager.ListPocketId(ctx, organization.Name, space.Name)
			if err != nil {
				return ManagementConfiguration{}, err
			}
			for _, provider := range providers {
				fragment.ManagedAddons.PocketId = append(fragment.ManagedAddons.PocketId, NamedPocketId{Name: provider.Name, PocketIdInput: provider.Specification})
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
	configured, _, err := loadDocument[managementNetworkConfiguration](ctx, manager.database, "management-network-settings", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return platform.Settings{}, err
	}
	if err == nil {
		settings.Network, settings.BorqPublicURL = configured.Network, configured.BorqPublicURL
	}
	if settings.Network.Tailscale != nil {
		state, _, err := loadDocument[platform.NetworkState](ctx, manager.database, "management-network-state", "shared")
		if err != nil && !errors.Is(err, ErrNotFound) {
			return platform.Settings{}, err
		}
		if err == nil {
			settings.Network.ManagementURL = state.ManagementURL
		}
	}
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
