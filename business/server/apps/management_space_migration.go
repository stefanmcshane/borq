package apps

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) retireUnusedManagementSpace(ctx context.Context) error {
	const migration = "unused-management-space-v1"
	if _, err := manager.database.Get(ctx, "migration", migration); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return err
	}
	usedIDs, usedNames := map[string]bool{}, map[string]bool{}
	for _, collection := range []string{"plan", "operation", "addon-operation", "environment-group", "env-config", "addon"} {
		records, err := manager.database.List(ctx, collection)
		if err != nil {
			return err
		}
		for _, record := range records {
			var scope struct {
				SpaceID      string `json:"spaceId"`
				Organization string `json:"organization"`
				Space        string `json:"space"`
				Plan         struct {
					Space Space `json:"space"`
				} `json:"plan"`
			}
			if err := json.Unmarshal(record.Value, &scope); err != nil {
				return err
			}
			usedIDs[scope.SpaceID] = true
			usedIDs[scope.Plan.Space.ID] = true
			usedNames[spaceKey(scope.Organization, scope.Space)] = true
		}
	}
	records, err := manager.database.List(ctx, "space")
	if err != nil {
		return err
	}
	writes := []sqlite.Write{}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return err
		}
		space := stored.Space
		initial := initialSpace(space.Organization, "management")
		if space.Name != "management" || space.Cluster != "management" || record.Key != spaceKey(space.Organization, space.Name) ||
			record.Revision != 1 || space.ID != initial.ID || space.Namespace != initial.Namespace || space.CertificateIssuer != initial.CertificateIssuer ||
			space.Revision != 0 || space.LastSuccessfulRevision != 0 || len(space.Configuration.Apps) != 0 ||
			stored.ActiveOperation != "" || len(stored.Queue) != 0 || len(stored.Manifests) != 0 ||
			len(stored.EnvSnapshots) != 0 || len(stored.EnvSelectors) != 0 || stored.EnvironmentGroupPrefix != "" ||
			usedIDs[space.ID] || usedNames[record.Key] {
			continue
		}
		if _, err := manager.database.Get(ctx, "cluster", space.Organization+"/management"); errors.Is(err, sqlite.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		bindingKey := space.Organization + "/" + space.Cluster + "/" + space.Namespace
		binding, bindingRecord, err := loadDocument[map[string]string](ctx, manager.database, "namespace", bindingKey)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if binding["spaceId"] != space.ID {
			continue
		}
		writes = append(writes,
			sqlite.Write{Collection: "archived-space", Key: record.Key, Value: record.Value},
			sqlite.Write{Collection: "space", Key: record.Key, ExpectedRevision: record.Revision, Delete: true},
			sqlite.Write{Collection: "namespace", Key: bindingKey, ExpectedRevision: bindingRecord.Revision, Delete: true},
		)
	}
	marker, err := documentWrite("migration", migration, map[string]bool{"complete": true}, 0)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, append(writes, marker))
}
