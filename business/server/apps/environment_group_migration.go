package apps

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// migrateEnvironmentGroups renames the catalog without copying vault versions or
// rewriting the manifests and secret paths frozen into saved plans.
func (manager Manager) migrateEnvironmentGroups(ctx context.Context) error {
	if _, err := manager.database.Get(ctx, "migration", "environment-groups-v1"); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return err
	}
	records, err := manager.database.List(ctx, "env-config")
	if err != nil {
		return err
	}
	writes := []sqlite.Write{}
	for _, record := range records {
		var group environmentGroupRecord
		if err := json.Unmarshal(record.Value, &group); err != nil {
			return err
		}
		group.VaultPath = legacyEnvironmentGroupPath(group.SpaceID, group.Name)
		write, err := documentWrite("environment-group", record.Key, group, 0)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	spaces, err := manager.database.List(ctx, "space")
	if err != nil {
		return err
	}
	for _, record := range spaces {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &document); err != nil {
			return err
		}
		prefix, exists := document["envConfigPrefix"]
		if !exists {
			continue
		}
		document["environmentGroupPrefix"] = prefix
		delete(document, "envConfigPrefix")
		write, err := documentWrite("space", record.Key, document, record.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	marker, err := documentWrite("migration", "environment-groups-v1", map[string]bool{"complete": true}, 0)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, append(writes, marker))
}
