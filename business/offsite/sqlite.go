package offsite

import (
	"path"
	"sort"
	"strings"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

func sqliteKey(key string) bool {
	parts := strings.Split(key, "/")
	if path.Clean(key) != key || strings.ContainsAny(key, "\\\x00") || parts[0] != "sqlite" {
		return false
	}
	switch len(parts) {
	case 8:
		if !identity(parts[1]) || parts[2] != "pocketid" || !identity(parts[3]) {
			return false
		}
		parts = parts[4:]
	case 7:
		if !identity(parts[1]) || parts[2] == "" {
			return false
		}
		parts = parts[3:]
	case 5:
		// Pocket ID has a dedicated bucket, so its Litestream prefix has no Space or app name.
		parts = parts[1:]
	default:
		return false
	}
	if parts[0] != "generations" || len(parts[1]) != 16 || strings.Trim(parts[1], "0123456789abcdef") != "" {
		return false
	}
	name := parts[3]
	if parts[2] == "snapshots" {
		return len(name) == 21 && strings.HasSuffix(name, ".snapshot.lz4") && strings.Trim(name[:8], "0123456789abcdef") == ""
	}
	return parts[2] == "wal" && len(name) == 25 && name[8] == '_' && strings.HasSuffix(name, ".wal.lz4") && strings.Trim(name[:8]+name[9:17], "0123456789abcdef") == ""
}

func sqliteGenerations(objects []objectstore.Object) []string {
	seen := map[string]bool{}
	for _, object := range objects {
		if !sqliteKey(object.Key) {
			continue
		}
		parts := strings.Split(object.Key, "/")
		if parts[len(parts)-2] == "snapshots" {
			seen[strings.Join(parts[:len(parts)-2], "/")] = true
		}
	}
	result := []string{}
	for generation := range seen {
		result = append(result, generation)
	}
	sort.Strings(result)
	return result
}
