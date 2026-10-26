package postgres

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// BackupMetadata is a strictly parsed physical Barman backup and provable recovery range.
type BackupMetadata struct {
	// ID is the Barman backup identity.
	ID string
	// CompletedAt and RecoveryEnd come from database backup metadata, never S3 write timestamps.
	CompletedAt, RecoveryEnd time.Time
	// MajorVersion and SizeBytes describe the physical backup.
	MajorVersion int
	SizeBytes    int64
	// Completed records DONE metadata independently of archive completeness.
	Completed bool
	// Eligible requires DONE metadata, data archive objects and every required WAL segment.
	Eligible bool
	// Reason explains incomplete or unsupported physical backup state.
	Reason string
	// Fingerprint freezes metadata and object identity for execution-time revalidation.
	Fingerprint                string
	beginWal, endWal, systemID string
	segmentSize                uint64
}

// Catalogue reads bounded Barman 3.20 metadata and actual archive object inventories.
func Catalogue(ctx context.Context, config objectstore.Config, server string) ([]BackupMetadata, error) {
	client, err := objectstore.New(config)
	if err != nil {
		return nil, err
	}
	objects, err := client.List(ctx, server+"/")
	if err != nil {
		return nil, err
	}
	if len(objects) > 200000 {
		return nil, errors.New("PostgreSQL archive inventory exceeds supported catalogue bound")
	}
	metadataObjects := []objectstore.Object{}
	metadataSize := int64(0)
	for _, object := range objects {
		if path.Base(object.Key) != "backup.info" || !strings.HasPrefix(object.Key, server+"/base/") || strings.Count(strings.TrimPrefix(object.Key, server+"/base/"), "/") != 1 {
			continue
		}
		if object.Size < 0 || object.Size > 1<<20 {
			return nil, errors.New("PostgreSQL backup.info exceeds supported 1 MiB size")
		}
		metadataSize += object.Size
		metadataObjects = append(metadataObjects, object)
		if len(metadataObjects) > 10000 {
			return nil, errors.New("PostgreSQL backup catalogue exceeds supported bound")
		}
		if metadataSize > 32<<20 {
			return nil, errors.New("PostgreSQL backup metadata exceeds supported 32 MiB aggregate size")
		}
	}
	metadata := map[string][]byte{}
	for _, object := range metadataObjects {
		body, err := client.Read(ctx, object.Key)
		if err != nil {
			return nil, err
		}
		if int64(len(body)) != object.Size || len(body) > 1<<20 {
			return nil, errors.New("PostgreSQL backup.info body does not match captured size")
		}
		metadata[object.Key] = body
	}
	return EvaluateCatalogue(objects, metadata, server)
}

// EvaluateCatalogue evaluates captured archive objects and backup.info bodies without live reads.
// The recovery boundary is the latest complete backup connected by consistency WAL.
func EvaluateCatalogue(objects []objectstore.Object, metadata map[string][]byte, server string) ([]BackupMetadata, error) {
	if len(objects) > 200000 || len(metadata) > 10000 {
		return nil, errors.New("PostgreSQL captured catalogue exceeds supported bound")
	}
	metadataSize := 0
	for _, body := range metadata {
		if len(body) > 1<<20 {
			return nil, errors.New("PostgreSQL backup.info exceeds supported 1 MiB size")
		}
		metadataSize += len(body)
		if metadataSize > 32<<20 {
			return nil, errors.New("PostgreSQL backup metadata exceeds supported 32 MiB aggregate size")
		}
	}
	inventory := map[string]int64{}
	for _, object := range objects {
		inventory[object.Key] = object.Size
	}
	result := make([]BackupMetadata, 0, len(metadata))
	for key, body := range metadata {
		object := objectstore.Object{Key: key}
		backup, err := ParseBackupMetadata(body)
		if err != nil {
			return nil, fmt.Errorf("invalid PostgreSQL archive metadata: %w", err)
		}
		backup.ID = path.Base(path.Dir(object.Key))
		if !regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`).MatchString(backup.ID) {
			continue
		}
		digest := sha256.New()
		_, _ = digest.Write(body)
		base := path.Dir(object.Key) + "/"
		keys := []string{}
		data := false
		archiveSize := int64(0)
		for key, size := range inventory {
			if !strings.HasPrefix(key, base) || key == object.Key {
				continue
			}
			keys = append(keys, key)
			archiveSize += size
			if key == base+"data.tar.gz" || key == base+"data.tar" || key == base+"data.tar.zst" || key == base+"data.tar.bz2" || key == base+"data.tar.lz4" {
				data = size > 0
			}
			if size <= 0 {
				backup.Eligible = false
				backup.Reason = "physical backup contains an empty archive object"
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = fmt.Fprintf(digest, "%s:%d\n", key, inventory[key])
		}
		if !data {
			backup.Eligible = false
			backup.Reason = "physical data archive is missing"
		}
		backup.SizeBytes = archiveSize
		if backup.Eligible && !walRangePresent(inventory, server, backup.beginWal, backup.endWal, backup.segmentSize) {
			backup.Eligible = false
			backup.Reason = "required consistency WAL segments are missing"
		}
		backup.Fingerprint = hex.EncodeToString(digest.Sum(nil))
		result = append(result, backup)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CompletedAt.Equal(result[j].CompletedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CompletedAt.Before(result[j].CompletedAt)
	})
	for index := range result {
		current := &result[index]
		current.RecoveryEnd = current.CompletedAt
		if !current.Eligible {
			continue
		}
		for later := index + 1; later < len(result); later++ {
			candidate := result[later]
			if !candidate.Eligible || candidate.systemID != current.systemID || candidate.segmentSize != current.segmentSize {
				continue
			}
			if walRangePresent(inventory, server, current.beginWal, candidate.endWal, current.segmentSize) {
				current.RecoveryEnd = candidate.CompletedAt
			}
		}
	}
	return result, nil
}

// ParseBackupMetadata parses only primitive fields from Barman's line format and never evaluates Python values.
func ParseBackupMetadata(body []byte) (BackupMetadata, error) {
	if len(body) > 1024*1024 {
		return BackupMetadata{}, errors.New("backup.info exceeds 1 MiB")
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return BackupMetadata{}, errors.New("malformed backup.info line")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if _, exists := values[key]; exists {
			return BackupMetadata{}, errors.New("duplicate backup.info field")
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return BackupMetadata{}, errors.New("unreadable backup.info")
	}
	result := BackupMetadata{Completed: values["status"] == "DONE", Eligible: values["status"] == "DONE", Reason: "backup is not complete", beginWal: values["begin_wal"], endWal: values["end_wal"], systemID: values["systemid"]}
	if !result.Eligible {
		return result, nil
	}
	result.Reason = ""
	version, err := strconv.Atoi(values["version"])
	if err != nil || version < 140000 || version >= 190000 {
		return BackupMetadata{}, errors.New("unsupported physical PostgreSQL version")
	}
	result.MajorVersion = version / 10000
	if values["size"] != "None" {
		result.SizeBytes, err = strconv.ParseInt(values["size"], 10, 64)
		if err != nil || result.SizeBytes <= 0 {
			return BackupMetadata{}, errors.New("invalid physical backup size")
		}
	}
	result.segmentSize, err = strconv.ParseUint(values["xlog_segment_size"], 10, 64)
	if err != nil || result.segmentSize < 1024*1024 || result.segmentSize > 1024*1024*1024 || result.segmentSize&(result.segmentSize-1) != 0 {
		return BackupMetadata{}, errors.New("invalid WAL segment size")
	}
	validWal := regexp.MustCompile(`^[0-9A-F]{24}$`)
	if !validWal.MatchString(result.beginWal) || !validWal.MatchString(result.endWal) || result.beginWal[:8] != result.endWal[:8] || result.systemID == "" {
		return BackupMetadata{}, errors.New("invalid physical backup WAL identity")
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-0700", "2006-01-02 15:04:05.999999999-07:00 MST"} {
		result.CompletedAt, err = time.Parse(layout, values["end_time"])
		if err == nil {
			break
		}
	}
	if err != nil {
		return BackupMetadata{}, errors.New("invalid physical backup completion timestamp")
	}
	if tablespaces := values["tablespaces"]; tablespaces != "" && tablespaces != "None" && tablespaces != "[]" {
		result.Eligible = false
		result.Reason = "external tablespaces require explicit physical archive verification"
	}
	if snapshots := values["snapshots_info"]; snapshots != "" && snapshots != "None" {
		result.Eligible = false
		result.Reason = "snapshot-only backup is not a full object storage backup"
	}
	if parent := values["parent_backup_id"]; parent != "" && parent != "None" {
		result.Eligible = false
		result.Reason = "incremental backup chains are not supported"
	}
	return result, nil
}

func walRangePresent(objects map[string]int64, server, begin, end string, segmentSize uint64) bool {
	if len(begin) != 24 || len(end) != 24 || begin[:8] != end[:8] || segmentSize == 0 {
		return false
	}
	first, err := strconv.ParseUint(begin[8:], 16, 64)
	if err != nil {
		return false
	}
	last, err := strconv.ParseUint(end[8:], 16, 64)
	if err != nil {
		return false
	}
	perLog := uint64(1) << 32 / segmentSize
	firstIndex := (first>>32)*perLog + (first & 0xffffffff)
	lastIndex := (last>>32)*perLog + (last & 0xffffffff)
	if lastIndex < firstIndex || lastIndex-firstIndex > 100000 {
		return false
	}
	for index := firstIndex; index <= lastIndex; index++ {
		wal := fmt.Sprintf("%s%08X%08X", begin[:8], index/perLog, index%perLog)
		key := server + "/wals/" + wal[:16] + "/" + wal
		found := false
		for _, suffix := range []string{"", ".gz", ".bz2", ".snappy", ".lz4", ".zst"} {
			if objects[key+suffix] > 0 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
