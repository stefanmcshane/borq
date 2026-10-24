package apps

import (
	"context"
	"errors"
	"strings"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// LinkCluster allocates a unique suffixed cluster name and its initial space.
// The first available default space is named default; later ones include the cluster name.
func (manager Manager) LinkCluster(ctx context.Context, input ClusterInput) (Cluster, error) {
	manager.runtime.registrationMutex.Lock()
	defer manager.runtime.registrationMutex.Unlock()
	if err := validateNames(input.Organization, input.Name); err != nil {
		return Cluster{}, err
	}
	// Reserve room for both the suffix and the default- space prefix within 63 characters.
	prefix := strings.TrimRight(input.Name[:min(len(input.Name), 49)], "-")
	for {
		if err := ctx.Err(); err != nil {
			return Cluster{}, err
		}
		id, err := newId()
		if err != nil {
			return Cluster{}, err
		}
		input.Name = prefix + "-" + id[:5]
		_, _, err = loadDocument[Cluster](ctx, manager.database, "cluster", input.Organization+"/"+input.Name)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return Cluster{}, err
		}
		spaceName := "default"
		_, _, err = loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(input.Organization, spaceName))
		if err == nil {
			spaceName += "-" + input.Name
			_, _, err = loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(input.Organization, spaceName))
			if err == nil {
				continue
			}
		}
		if !errors.Is(err, ErrNotFound) {
			return Cluster{}, err
		}
		space := initialSpace(input.Organization, spaceName)
		space.Cluster = input.Name
		spaceWrite, err := documentWrite("space", spaceKey(space.Organization, space.Name), spaceRecord{Space: space, Manifests: []map[string]any{}}, 0)
		if err != nil {
			return Cluster{}, err
		}
		bindingWrite, err := documentWrite("namespace", space.Organization+"/"+space.Cluster+"/"+space.Namespace, map[string]string{"spaceId": space.ID}, 0)
		if err != nil {
			return Cluster{}, err
		}
		return manager.registerCluster(ctx, input, []sqlite.Write{spaceWrite, bindingWrite})
	}
}
