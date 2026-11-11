package offsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
)

// Export writes a deterministic tar stream and a terminal manifest only after all captured bytes validate.
func Export(ctx context.Context, config objectstore.Config, output io.Writer) error {
	inventory, err := Freeze(ctx, config)
	if err != nil {
		return err
	}
	return exportInventory(ctx, ExportInput{Storage: storageConfig(config), Inventory: inventory}, output)
}

func exportInventory(ctx context.Context, input ExportInput, output io.Writer) error {
	config := input.Storage.Config()
	if err := input.Inventory.validate(config); err != nil {
		return err
	}
	client, err := objectstore.New(config)
	if err != nil {
		return err
	}
	// Filtering must not mutate the frozen inventory reused by the caller.
	objects := append([]objectstore.Object{}, input.Inventory.Objects...)
	nativeEntries, err := managementbackup.Inventory(ctx, client, objects)
	if err != nil {
		return err
	}
	nativeKeys := map[string]bool{}
	nativeDigests := map[string]string{}
	native := []managementbackup.Artifact{}
	for _, entry := range nativeEntries {
		nativeKeys[entry.Payload.Key] = true
		nativeKeys[entry.Completion.Key] = true
		nativeDigests[entry.Payload.Key] = entry.Artifact.SHA256
		native = append(native, entry.Artifact)
	}
	selected := objects[:0]
	for _, object := range objects {
		if managedKey(object.Key) || nativeKeys[object.Key] || sqliteKey(object.Key) {
			selected = append(selected, object)
		}
	}
	objects = selected
	checkpointBodies := map[string][]byte{}
	checkpointPayload := ""
	if input.Checkpoint.Artifact.Kind != "" {
		if err := input.Checkpoint.validate(input.Inventory); err != nil {
			return err
		}
		artifact := input.Checkpoint.Artifact
		marker, _ := json.Marshal(artifact)
		metadataSize := int64(len(marker))
		for _, entry := range nativeEntries {
			metadataSize += entry.Completion.Size
		}
		if metadataSize > 32<<20 {
			return errors.New("native snapshot completion metadata exceeds supported bounds")
		}
		payloadKey, completionKey := managementbackup.PayloadKey(artifact), managementbackup.CompletionKey(artifact)
		for _, object := range objects {
			if object.Key == payloadKey || object.Key == completionKey {
				return errors.New("vault checkpoint collides with frozen source inventory")
			}
		}
		checkpointPayload = payloadKey
		checkpointBodies[completionKey] = marker
		nativeDigests[payloadKey] = artifact.SHA256
		native = append(native, artifact)
		objects = append(objects, objectstore.Object{Key: payloadKey, Size: artifact.Size, ETag: artifact.SHA256}, objectstore.Object{Key: completionKey, Size: int64(len(marker)), ETag: artifact.SHA256})
		sort.Slice(native, func(i, j int) bool {
			a, b := native[i], native[j]
			if a.CapturedAt.Equal(b.CapturedAt) {
				if a.ID == b.ID {
					return a.Kind < b.Kind
				}
				return a.ID < b.ID
			}
			return a.CapturedAt.Before(b.CapturedAt)
		})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	metadata := map[string][]byte{}
	metadataBytes := int64(0)
	for _, object := range objects {
		if metadataKey(object.Key) {
			metadataBytes += object.Size
		}
	}
	if metadataBytes > 32<<20 {
		return errors.New("captured PostgreSQL metadata exceeds 32 MiB")
	}
	for index, object := range objects {
		if index > 0 && object.Key == objects[index-1].Key {
			return errors.New("source inventory contains duplicate objects")
		}
		if !metadataKey(object.Key) {
			continue
		}
		body, err := readMetadata(ctx, client, object)
		if err != nil {
			return err
		}
		metadata[object.Key] = body
	}
	backups, excluded, err := evaluate(objects, metadata)
	if err != nil {
		return err
	}
	generations := sqliteGenerations(objects)
	if len(backups) == 0 && len(native) == 0 && len(generations) == 0 {
		return errors.New("no complete managed backups were captured")
	}
	manifest := Archive{Version: 2, Native: native, Source: SourceID(config), Objects: []Object{}, Backups: backups}
	if len(generations) > 0 {
		manifest.Version, manifest.Sqlite = 3, generations
	}
	if checkpointPayload != "" {
		manifest.Version = 4
		manifest.InventoryCapturedAt, manifest.VaultCheckpoint = input.Inventory.CapturedAt, input.Checkpoint.Artifact
	}
	writer := tar.NewWriter(output)
	for _, object := range objects {
		skip := false
		for _, prefix := range excluded {
			if strings.HasPrefix(object.Key, prefix) {
				skip = true
				break
			}
		}
		if sqliteKey(object.Key) {
			included := false
			for _, generation := range generations {
				included = included || strings.HasPrefix(object.Key, generation+"/")
			}
			skip = skip || !included
		}
		if skip {
			continue
		}
		header := archiveHeader(object.Key, object.Size)
		if err := writer.WriteHeader(&header); err != nil {
			return err
		}
		var body io.ReadCloser
		if object.Key == checkpointPayload {
			body, err = os.Open(input.Checkpoint.File)
		} else if data, exists := checkpointBodies[object.Key]; exists {
			body = io.NopCloser(bytes.NewReader(data))
		} else {
			body, err = client.OpenCaptured(ctx, object)
		}
		if err != nil {
			return err
		}
		digest := sha256.New()
		count, copyErr := io.Copy(io.MultiWriter(writer, digest), io.LimitReader(body, object.Size+1))
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil || count != object.Size {
			return errors.New("captured source object could not be completely exported")
		}
		if expected := nativeDigests[object.Key]; expected != "" && expected != hex.EncodeToString(digest.Sum(nil)) {
			return errors.New("completed native snapshot digest does not match captured payload")
		}
		captured := Object{Key: object.Key, Size: object.Size, ETag: object.ETag, SHA256: hex.EncodeToString(digest.Sum(nil))}
		if manifest.Version >= 3 {
			captured.LastModified = object.LastModified
		}
		manifest.Objects = append(manifest.Objects, captured)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(data) > 64<<20 {
		return errors.New("offsite manifest exceeds 64 MiB")
	}
	header := archiveHeader("borq-manifest.json", int64(len(data)))
	if err := writer.WriteHeader(&header); err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		return err
	}
	return writer.Close()
}

func managedKey(key string) bool {
	if strings.ContainsAny(key, "\\\x00") || path.Clean(key) != key || strings.HasPrefix(key, "/") {
		return false
	}
	parts := strings.Split(key, "/")
	if len(parts) < 7 || parts[0] != "organizations" || !identity(parts[1]) || parts[2] != "postgres" || !identity(parts[3]) || parts[4] != "archive-"+parts[3] {
		return false
	}
	if parts[5] == "base" {
		return len(parts) == 8 && len(parts[6]) == 15 && parts[6][8] == 'T' && strings.Trim(parts[6][:8]+parts[6][9:], "0123456789") == "" && parts[7] != ""
	}
	if parts[5] != "wals" {
		return false
	}
	if len(parts) == 7 {
		return len(parts[6]) >= 16 && strings.Trim(parts[6][:8], "0123456789ABCDEF") == "" && strings.HasPrefix(parts[6][8:], ".history")
	}
	return len(parts) == 8 && len(parts[6]) == 16 && len(parts[7]) >= 24 && strings.Trim(parts[7][:24], "0123456789ABCDEF") == "" && parts[6] == parts[7][:16]

}
func identity(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}
func metadataKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 8 && parts[5] == "base" && parts[7] == "backup.info"
}
func readMetadata(ctx context.Context, client objectstore.Client, object objectstore.Object) ([]byte, error) {
	if object.Size > 1<<20 {
		return nil, errors.New("backup.info exceeds 1 MiB")
	}
	body, err := client.OpenCaptured(ctx, object)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, (1<<20)+1))
	if err != nil || int64(len(data)) != object.Size {
		return nil, errors.New("captured backup.info could not be read completely")
	}
	return data, nil
}
func evaluate(objects []objectstore.Object, metadata map[string][]byte) (map[string][]postgres.BackupMetadata, []string, error) {
	servers := map[string]map[string][]byte{}
	for key, body := range metadata {
		if !metadataKey(key) {
			continue
		}
		parts := strings.Split(key, "/")
		server := strings.Join(parts[:5], "/")
		if servers[server] == nil {
			servers[server] = map[string][]byte{}
		}
		servers[server][key] = body
	}
	result := map[string][]postgres.BackupMetadata{}
	excluded := []string{}
	for server, bodies := range servers {
		backups, err := postgres.EvaluateCatalogue(objects, bodies, server)
		if err != nil {
			return nil, nil, err
		}
		for _, backup := range backups {
			if backup.Completed && !backup.Eligible {
				return nil, nil, fmt.Errorf("completed PostgreSQL backup is not recoverable: %s", backup.Reason)
			}
			if !backup.Completed {
				excluded = append(excluded, server+"/base/"+backup.ID+"/")
				continue
			}
			result[server] = append(result[server], backup)
		}
	}
	return result, excluded, nil
}

// Restore imports a validated archive into an empty, unpublished destination prefix.
// A failure can leave isolated partial objects; callers must never publish that prefix on error.
func Restore(ctx context.Context, config objectstore.Config, source string, input io.Reader) (Archive, error) {
	if strings.Trim(config.Prefix, "/") == "" || source == "" {
		return Archive{}, errors.New("recovery requires an explicit fresh destination prefix and source identity")
	}
	// Native OpenBao and Litestream payloads already carry client-side encryption.
	// Requiring Vault-backed SSE here would make the Vault checkpoint depend on itself.
	config.ServerSideEncryption = ""
	client, err := objectstore.New(config)
	if err != nil {
		return Archive{}, err
	}
	postgresConfig := config
	postgresConfig.ServerSideEncryption = "AES256"
	postgresClient, err := objectstore.New(postgresConfig)
	if err != nil {
		return Archive{}, err
	}
	page, err := client.ListPage(ctx, "", "", 1)
	if err != nil {
		return Archive{}, err
	}
	if len(page.Objects) > 0 {
		return Archive{}, errors.New("recovery destination prefix must be empty")
	}
	if err := client.PutNew(ctx, ".borq-offsite-restore", strings.NewReader(source), int64(len(source))); err != nil {
		return Archive{}, err
	}
	reader := tar.NewReader(input)
	objects := []Object{}
	inventory := []objectstore.Object{}
	metadata := map[string][]byte{}
	metadataBytes := int64(0)
	nativeMetadataBytes := int64(0)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return Archive{}, errors.New("offsite archive has no completion manifest")
		}
		if err != nil {
			return Archive{}, errors.New("offsite archive is truncated or invalid")
		}
		if header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Size < 0 || header.Size > objectstore.MaxStreamSize || len(header.PAXRecords) > 0 || len(header.Xattrs) > 0 {
			return Archive{}, errors.New("offsite archive contains unsupported tar metadata")
		}
		if header.Name == "borq-manifest.json" {
			if header.Size > 64<<20 {
				return Archive{}, errors.New("offsite manifest exceeds 64 MiB")
			}
			var manifest Archive
			decoder := json.NewDecoder(io.LimitReader(reader, header.Size))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return Archive{}, errors.New("offsite completion manifest is invalid")
			}
			if decoder.Decode(&struct{}{}) != io.EOF {
				return Archive{}, errors.New("offsite completion manifest contains trailing data")
			}
			if (manifest.Version != 2 && manifest.Version != 3 && manifest.Version != 4) || manifest.Source != source || len(manifest.Objects) != len(objects) {
				return Archive{}, errors.New("offsite completion manifest does not match this source and inventory")
			}
			for index, object := range objects {
				captured := manifest.Objects[index]
				if object.Key != captured.Key || object.Size != captured.Size || object.SHA256 != captured.SHA256 || captured.ETag == "" {
					return Archive{}, errors.New("offsite object size, digest or order does not match manifest")
				}
			}
			generations := sqliteGenerations(inventory)
			expectedSqlite, _ := json.Marshal(generations)
			claimedSqlite, _ := json.Marshal(manifest.Sqlite)
			if len(generations) > 0 && manifest.Version < 3 || len(generations) != len(manifest.Sqlite) || len(generations) > 0 && string(expectedSqlite) != string(claimedSqlite) {
				return Archive{}, errors.New("offsite SQLite generation inventory does not match captured objects")
			}
			for _, object := range inventory {
				if sqliteKey(object.Key) {
					included := false
					for _, generation := range generations {
						included = included || strings.HasPrefix(object.Key, generation+"/")
					}
					if !included {
						return Archive{}, errors.New("offsite SQLite generation has no snapshot")
					}
				}
			}
			if err := validateNativeRestore(manifest, objects, metadata); err != nil {
				return Archive{}, err
			}
			if err := validateCheckpointManifest(manifest); err != nil {
				return Archive{}, err
			}
			backups, _, err := evaluate(inventory, metadata)
			if err != nil {
				return Archive{}, err
			}
			expected, _ := json.Marshal(backups)
			claimed, _ := json.Marshal(manifest.Backups)
			if string(expected) != string(claimed) {
				return Archive{}, errors.New("offsite recovery catalogue does not match captured objects")
			}
			if _, err := reader.Next(); err != io.EOF {
				return Archive{}, errors.New("offsite archive has entries after completion")
			}
			if _, err := io.Copy(zeroPadding{}, input); err != nil {
				return Archive{}, errors.New("offsite archive contains trailing data or failed producer")
			}
			return manifest, nil
		}
		if (!managedKey(header.Name) && !managementbackup.ManagedKey(header.Name) && !sqliteKey(header.Name)) || (len(objects) > 0 && header.Name <= objects[len(objects)-1].Key) {
			return Archive{}, errors.New("offsite archive contains unsafe, duplicate or unsorted object keys")
		}
		if len(objects) >= 100000 {
			return Archive{}, errors.New("offsite inventory exceeds 100000 objects")
		}

		if managementbackup.ManagedKey(header.Name) && strings.HasSuffix(header.Name, "/payload") && header.Size > managementbackup.MaxPayloadSize {
			return Archive{}, errors.New("native payload exceeds supported 8 GiB limit")
		}
		digest := sha256.New()
		body := io.TeeReader(reader, digest)
		if metadataKey(header.Name) || strings.HasSuffix(header.Name, "/complete.json") {
			if header.Size > 1<<20 {
				return Archive{}, errors.New("backup.info exceeds 1 MiB")
			}
			if managementbackup.ManagedKey(header.Name) {
				if header.Size > 64<<10 {
					return Archive{}, errors.New("native snapshot completion marker exceeds 64 KiB")
				}
				nativeMetadataBytes += header.Size
				if nativeMetadataBytes > 32<<20 {
					return Archive{}, errors.New("native snapshot completion metadata exceeds supported bounds")
				}
			} else {
				metadataBytes += header.Size
			}
			if metadataBytes > 32<<20 {
				return Archive{}, errors.New("captured PostgreSQL metadata exceeds 32 MiB")
			}
			data, err := io.ReadAll(body)
			if err != nil {
				return Archive{}, errors.New("offsite backup metadata is truncated")
			}
			metadata[header.Name] = data
			body = strings.NewReader(string(data))
		}
		destination := client
		if managedKey(header.Name) {
			destination = postgresClient
		}
		if err := destination.PutNew(ctx, header.Name, body, header.Size); err != nil {
			return Archive{}, err
		}
		objects = append(objects, Object{Key: header.Name, Size: header.Size, SHA256: hex.EncodeToString(digest.Sum(nil))})
		inventory = append(inventory, objectstore.Object{Key: header.Name, Size: header.Size})
	}
}

type zeroPadding struct{}

func (zeroPadding) Write(data []byte) (int, error) {
	for _, value := range data {
		if value != 0 {
			return 0, errors.New("nonzero trailing archive data")
		}
	}
	return len(data), nil
}

func archiveHeader(key string, size int64) tar.Header {
	return tar.Header{Name: key, Mode: 0600, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatGNU}
}

func validateNativeRestore(manifest Archive, objects []Object, metadata map[string][]byte) error {
	captured := map[string]Object{}
	for _, object := range objects {
		captured[object.Key] = object
	}
	native := []managementbackup.Artifact{}
	for key, body := range metadata {
		if !managementbackup.ManagedKey(key) {
			continue
		}
		artifact, err := managementbackup.ParseCompletion(key, body)
		if err != nil {
			return err
		}
		payload, exists := captured[managementbackup.PayloadKey(artifact)]
		if !exists || payload.Size != artifact.Size || payload.SHA256 != artifact.SHA256 {
			return errors.New("rehydrated native payload does not match its completion marker")
		}
		native = append(native, artifact)
	}
	for _, object := range objects {
		if managementbackup.ManagedKey(object.Key) && strings.HasSuffix(object.Key, "/payload") {
			complete := strings.TrimSuffix(object.Key, "payload") + "complete.json"
			if _, exists := metadata[complete]; !exists {
				return errors.New("native payload has no completion marker")
			}
		}
	}
	sort.Slice(native, func(i, j int) bool {
		a, b := native[i], native[j]
		if a.CapturedAt.Equal(b.CapturedAt) {
			if a.ID == b.ID {
				return a.Kind < b.Kind
			}
			return a.ID < b.ID
		}
		return a.CapturedAt.Before(b.CapturedAt)
	})
	expected, _ := json.Marshal(native)
	claimed, _ := json.Marshal(manifest.Native)
	if string(expected) != string(claimed) {
		return errors.New("native completion inventory does not match offsite manifest")
	}
	return nil
}
