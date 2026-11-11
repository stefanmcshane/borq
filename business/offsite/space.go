package offsite

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
)

// SelectSpaceObjects chooses one generation per application using captured chronology, never rehydrated timestamps.
// Explicit selections disambiguate equal times or request an older recovery generation.
func SelectSpaceObjects(archive Archive, spaceID string, selections map[string]string) ([]managementbackup.SpaceObject, error) {
	if (archive.Version != 2 && archive.Version != 3 && archive.Version != 4) || !identity(spaceID) {
		return nil, errors.New("space recovery requires a completed manifest and the saved space identity")
	}
	latest := map[string]string{}
	times := map[string]time.Time{}
	available := map[string]bool{}
	ambiguous := map[string]bool{}
	missingTime := map[string]bool{}
	counts := map[string]int{}
	for _, generation := range archive.Sqlite {
		parts := strings.Split(generation, "/")
		if len(parts) < 5 || parts[0] != "sqlite" || parts[1] != spaceID {
			continue
		}
		app, generationPart := parts[2], 3
		if len(parts) == 6 && parts[2] == "pocketid" && identity(parts[3]) {
			app, generationPart = path.Join(parts[2], parts[3]), 4
		}
		if len(parts) != generationPart+2 || parts[generationPart] != "generations" {
			return nil, errors.New("invalid SQLite generation in recovery manifest")
		}
		id := parts[generationPart+1]
		if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
			return nil, errors.New("invalid SQLite generation in recovery manifest")
		}
		available[generation] = true
		modified := time.Time{}
		hasSnapshot := false
		for _, object := range archive.Objects {
			if strings.HasPrefix(object.Key, generation+"/") && sqliteKey(object.Key) {
				hasSnapshot = hasSnapshot || strings.Contains(object.Key, "/snapshots/")
				if object.LastModified.After(modified) {
					modified = object.LastModified
				}
			}
		}
		if !hasSnapshot {
			return nil, errors.New("captured SQLite generation has no snapshot")
		}
		if selected := selections[app]; selected != "" {
			if selected == id {
				latest[app], times[app] = generation, modified
			}
			continue
		}
		counts[app]++
		missingTime[app] = missingTime[app] || modified.IsZero()
		if latest[app] == "" || modified.After(times[app]) {
			latest[app], times[app] = generation, modified
			ambiguous[app] = false
		} else if modified.Equal(times[app]) {
			ambiguous[app] = true
		}
	}
	for app := range latest {
		if selections[app] == "" && (ambiguous[app] || counts[app] > 1 && missingTime[app]) {
			return nil, errors.New("ambiguous captured generation chronology; select the application's generation explicitly")
		}
	}
	for app, id := range selections {
		if !available[path.Join("sqlite", spaceID, app, "generations", id)] || latest[app] == "" {
			return nil, errors.New("selected generation is absent from this space's completed snapshot")
		}
	}
	result := []managementbackup.SpaceObject{}
	for _, object := range archive.Objects {
		if managedKey(object.Key) {
			result = append(result, managementbackup.SpaceObject{Key: object.Key, Size: object.Size, SHA256: object.SHA256})
			continue
		}
		for _, generation := range latest {
			if strings.HasPrefix(object.Key, generation+"/") && sqliteKey(object.Key) {
				result = append(result, managementbackup.SpaceObject{Key: object.Key, Size: object.Size, SHA256: object.SHA256})
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

// ReadArchiveManifest loads the bounded completion manifest saved after successful recovery.
func ReadArchiveManifest(filename string) (Archive, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Archive{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return Archive{}, errors.New("recovery manifest must be a regular file no larger than 64 MiB")
	}
	var archive Archive
	decoder := json.NewDecoder(io.LimitReader(file, (64<<20)+1))
	if err := decoder.Decode(&archive); err != nil {
		return Archive{}, errors.New("invalid recovery completion manifest")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Archive{}, errors.New("unexpected data after recovery manifest")
	}
	return archive, nil
}
