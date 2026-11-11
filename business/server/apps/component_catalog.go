package apps

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/business/server/platform"
)

// GetComponentCatalog reads desired Helm configuration using saved overrides and a registered target context.
func (manager Manager) GetComponentCatalog(ctx context.Context, scope platform.ComponentScope, organization, cluster string) (platform.ComponentCatalog, error) {
	if scope != platform.ComponentScopeManagement && scope != platform.ComponentScopeTarget && scope != platform.ComponentScopePostgres {
		return platform.ComponentCatalog{}, fmt.Errorf("%w: unsupported component scope %q", ErrInvalid, scope)
	}
	input := platform.ComponentCatalogInput{Scope: scope}
	var target Cluster
	if scope == platform.ComponentScopeTarget {
		if organization == "" || cluster == "" {
			return platform.ComponentCatalog{}, fmt.Errorf("%w: target component scope requires organization and cluster", ErrInvalid)
		}
		if err := validateNames(organization, cluster); err != nil {
			return platform.ComponentCatalog{}, err
		}
		if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization); err != nil {
			return platform.ComponentCatalog{}, err
		}
		registered, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+cluster)
		if err != nil {
			return platform.ComponentCatalog{}, err
		}
		input.ClusterID = platform.ClusterID(registered.Organization, registered.Name)
		target = registered
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return platform.ComponentCatalog{}, err
	}
	if scope == platform.ComponentScopeTarget {
		settings, _, err = manager.clusterPlatformSettings(ctx, target, settings)
		if err != nil {
			return platform.ComponentCatalog{}, err
		}
	}
	input.Settings = settings
	result, err := platform.GetComponentCatalog(input)
	if err != nil {
		return platform.ComponentCatalog{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return result, nil
}
