package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// legacySpace reads the old project/environment envelope only during migration.
type legacySpace struct {
	Space
	Project string `json:"project"`
}

// migrateLegacyHierarchy preserves deployment identities and artifacts while
// folding each legacy project/environment into an organization-owned space.
// Old grouping documents remain archived; one transaction marks the conversion complete.
func (manager Manager) migrateLegacyHierarchy(ctx context.Context) error {
	if _, err := manager.database.Get(ctx, "migration", "organization-spaces-v1"); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return err
	}
	organizations := map[string]bool{}
	for _, collection := range []string{"project", "cluster"} {
		records, err := manager.database.List(ctx, collection)
		if err != nil {
			return err
		}
		for _, record := range records {
			var owner struct {
				Organization string `json:"organization"`
			}
			if err := json.Unmarshal(record.Value, &owner); err != nil {
				return err
			}
			if err := validateNames(owner.Organization); err != nil {
				return fmt.Errorf("legacy %s has an invalid organization", collection)
			}
			organizations[owner.Organization] = true
		}
	}
	legacyRecords, err := manager.database.List(ctx, "environment")
	if err != nil {
		return err
	}
	legacy := make([]legacySpace, len(legacyRecords))
	nameCounts := map[string]int{}
	for index, record := range legacyRecords {
		var envelope struct {
			Environment legacySpace `json:"environment"`
		}
		if err := json.Unmarshal(record.Value, &envelope); err != nil {
			return err
		}
		legacy[index] = envelope.Environment
		previous := legacy[index]
		if err := validateNames(previous.Organization, previous.Project, previous.Name); err != nil || previous.ID == "" {
			return fmt.Errorf("legacy environment has an invalid identity")
		}
		organizations[previous.Organization] = true
		nameCounts[spaceKey(previous.Organization, legacySpaceName(previous))]++
	}
	writes := []sqlite.Write{}
	names := map[string]string{}
	assigned := map[string]bool{}
	for index, record := range legacyRecords {
		previous := legacy[index]
		name := legacySpaceName(previous)
		if nameCounts[spaceKey(previous.Organization, name)] > 1 {
			prefix := strings.TrimRight(name[:min(52, len(name))], "-")
			// Reserve every ordinary name before assigning suffixes, including
			// names belonging to records later in this transaction.
			for attempt := 0; ; attempt++ {
				identity := previous.ID
				if attempt > 0 {
					identity = fmt.Sprintf("%s/%d", previous.ID, attempt)
				}
				name = prefix + "-" + stableId(identity)[:10]
				key := spaceKey(previous.Organization, name)
				if nameCounts[key] == 0 && !assigned[key] {
					break
				}
			}
		}
		key := spaceKey(previous.Organization, name)
		assigned[key] = true
		if _, err := manager.database.Get(ctx, "space", key); !errors.Is(err, sqlite.ErrNotFound) {
			if err != nil {
				return err
			}
			return fmt.Errorf("legacy space %s conflicts with an existing space", key)
		}
		names[previous.ID] = name
		converted, err := convertLegacySpaceRecord(record.Value, previous, name)
		if err != nil {
			return err
		}
		writes = append(writes, sqlite.Write{Collection: "space", Key: key, Value: converted})
	}
	for organization := range organizations {
		if _, err := manager.database.Get(ctx, "organization", organization); errors.Is(err, sqlite.ErrNotFound) {
			write, err := documentWrite("organization", organization, Organization{Name: organization, ManagedAddons: true}, 0)
			if err != nil {
				return err
			}
			writes = append(writes, write)
		} else if err != nil {
			return err
		}
	}
	for _, collection := range []string{"plan", "operation", "namespace", "env-config"} {
		records, err := manager.database.List(ctx, collection)
		if err != nil {
			return err
		}
		for _, record := range records {
			converted, changed, err := convertLegacyScope(record.Value, collection, names)
			if err != nil {
				return err
			}
			if changed {
				writes = append(writes, sqlite.Write{Collection: collection, Key: record.Key, Value: converted, ExpectedRevision: record.Revision})
			}
		}
	}
	marker, err := documentWrite("migration", "organization-spaces-v1", map[string]bool{"complete": true}, 0)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, append(writes, marker))
}

func legacySpaceName(previous legacySpace) string {
	name := previous.Project + "-" + previous.Name
	if len(name) > 63 {
		name = strings.TrimRight(name[:52], "-") + "-" + stableId(previous.ID)[:10]
	}
	return name
}

func convertLegacySpaceRecord(source json.RawMessage, previous legacySpace, name string) (json.RawMessage, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(source, &document); err != nil {
		return nil, err
	}
	space := previous.Space
	space.Name = name
	issuer, err := normalizeCertificateIssuer(space.CertificateIssuer)
	if err != nil {
		return nil, err
	}
	space.CertificateIssuer = issuer
	document["space"], err = json.Marshal(space)
	if err != nil {
		return nil, err
	}
	document["environmentGroupPrefix"], err = json.Marshal(previous.Project + "-" + previous.Name)
	if err != nil {
		return nil, err
	}
	delete(document, "environment")
	return json.Marshal(document)
}

func convertLegacyScope(source json.RawMessage, collection string, names map[string]string) (json.RawMessage, bool, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(source, &document); err != nil {
		return nil, false, err
	}
	if collection != "plan" {
		identity, exists := document["environmentId"]
		if !exists {
			return source, false, nil
		}
		document["spaceId"] = identity
		delete(document, "environmentId")
		converted, err := json.Marshal(document)
		return converted, true, err
	}
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(document["plan"], &plan); err != nil {
		return nil, false, err
	}
	old, exists := plan["environment"]
	if !exists {
		return source, false, nil
	}
	var previous legacySpace
	if err := json.Unmarshal(old, &previous); err != nil {
		return nil, false, err
	}
	name, exists := names[previous.ID]
	if !exists {
		return nil, false, fmt.Errorf("saved plan refers to a missing legacy environment")
	}
	previous.Space.Name = name
	var err error
	plan["space"], err = json.Marshal(previous.Space)
	if err != nil {
		return nil, false, err
	}
	delete(plan, "environment")
	document["plan"], err = json.Marshal(plan)
	if err != nil {
		return nil, false, err
	}
	converted, err := json.Marshal(document)
	return converted, true, err
}
