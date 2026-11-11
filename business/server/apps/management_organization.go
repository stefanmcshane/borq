package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) ensureOrganizationIdentity(ctx context.Context, organization Organization) (Organization, error) {
	if organization.ID != "" {
		return organization, nil
	}
	for {
		current, previous, err := loadDocument[Organization](ctx, manager.database, "organization", organization.Name)
		if err != nil {
			return Organization{}, err
		}
		if current.ID != "" {
			return current, nil
		}
		current.ID, err = newId()
		if err != nil {
			return Organization{}, err
		}
		write, err := documentWrite("organization", current.Name, current, previous.Revision)
		if err != nil {
			return Organization{}, err
		}
		err = manager.database.Commit(ctx, []sqlite.Write{write})
		if errors.Is(err, sqlite.ErrConflict) {
			continue
		}
		return current, err
	}
}

// SetOrganizationManagedAddons explicitly grants shared S3 usage to one organization.
func (manager Manager) SetOrganizationManagedAddons(ctx context.Context, name string, enabled bool) (Organization, error) {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	organization, previous, err := loadDocument[Organization](ctx, manager.database, "organization", name)
	if err != nil {
		return Organization{}, err
	}
	if enabled {
		storage, err := manager.GetManagementS3(ctx)
		if err != nil {
			return Organization{}, err
		}
		if err := manager.validateManagementStorage(storage); err != nil {
			return Organization{}, err
		}
		config, err := manager.ResolveManagementS3(ctx, storage)
		if err != nil {
			return Organization{}, err
		}
		client, err := objectstore.New(config)
		if err != nil {
			return Organization{}, err
		}
		if err := client.Validate(ctx); err != nil {
			return Organization{}, fmt.Errorf("%w: shared S3 validation failed before organization grant", ErrInvalid)
		}
	} else if organization.ManagedAddons {
		archives, err := manager.database.List(ctx, "postgres-archive")
		if err != nil {
			return Organization{}, err
		}
		for _, archive := range archives {
			var dependency struct {
				Organization string `json:"organization"`
			}
			if err := json.Unmarshal(archive.Value, &dependency); err != nil {
				return Organization{}, err
			}
			if dependency.Organization == name {
				return Organization{}, fmt.Errorf("%w: organization has managed addons or preserved archives", ErrConflict)
			}
		}
	}
	if organization.ID == "" {
		organization.ID, err = newId()
		if err != nil {
			return Organization{}, err
		}
	}
	organization.ManagedAddons = enabled
	write, err := documentWrite("organization", name, organization, previous.Revision)
	if err != nil {
		return Organization{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
		return Organization{}, translateStoreError(err)
	}
	return organization, nil
}
