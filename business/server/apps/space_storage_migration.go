package apps

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Upgrade allocates future dedicated buckets without moving archives already in
// shared buckets. Historical SQLite writers must never regain empty-file permission.
func (manager Manager) migrateSpaceStorage(ctx context.Context) error {
	const marker = "space-backup-storage-v1"
	if _, err := manager.database.Get(ctx, "migration", marker); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return err
	}
	writes := []sqlite.Write{}
	initializations := map[string]bool{}
	remember := func(spaceID string, configuration Configuration) {
		for _, app := range configuration.Apps {
			if app.Sqlite.Enabled {
				initializations[spaceID+"/"+app.Name] = true
			}
		}
	}
	spaces, err := manager.database.List(ctx, "space")
	if err != nil {
		return err
	}
	for _, record := range spaces {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return err
		}
		remember(stored.Space.ID, stored.Space.Configuration)
		if _, err := manager.database.Get(ctx, "space-storage", stored.Space.ID); err == nil {
			continue
		} else if !errors.Is(err, sqlite.ErrNotFound) {
			return err
		}
		write, err := newSpaceStorageWrite(stored.Space)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	operations, err := manager.database.List(ctx, "operation")
	if err != nil {
		return err
	}
	accepted := map[string]bool{}
	for _, record := range operations {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		accepted[operation.PlanID] = true
	}
	plans, err := manager.database.List(ctx, "plan")
	if err != nil {
		return err
	}
	for _, record := range plans {
		if !accepted[record.Key] {
			continue
		}
		var saved planRecord
		if err := json.Unmarshal(record.Value, &saved); err != nil {
			return err
		}
		remember(saved.Plan.Space.ID, saved.Plan.Configuration)
		remember(saved.Plan.Space.ID, saved.Plan.Before)
	}
	for key := range initializations {
		if _, err := manager.database.Get(ctx, "sqlite-initialization", key); err == nil {
			continue
		} else if !errors.Is(err, sqlite.ErrNotFound) {
			return err
		}
		write, err := documentWrite("sqlite-initialization", key, "active", 0)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	complete, err := documentWrite("migration", marker, map[string]bool{"complete": true}, 0)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, append(writes, complete))
}
