package offsite

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
)

// Export writes a deterministic tar stream and a terminal manifest only after all captured bytes validate.
func Export(ctx context.Context, config objectstore.Config, output io.Writer) error {
	client, err := objectstore.New(config)
	if err != nil {
		return err
	}
	objects := []objectstore.Object{}
	continuation := ""
	for {
		page, err := client.ListPage(ctx, "organizations/", continuation, 1000)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if !managedKey(object.Key) {
				continue
			}
			if object.Size > objectstore.MaxStreamSize {
				return errors.New("source object exceeds supported 625 GiB streaming limit")
			}
			if object.ETag == "" || object.Size < 0 {
				return errors.New("source inventory lacks captured object identities")
			}
			objects = append(objects, object)
			if len(objects) > 100000 {
				return errors.New("offsite inventory exceeds 100000 objects")
			}
		}
		if page.Continuation == "" {
			break
		}
		if page.Continuation == continuation {
			return errors.New("source pagination did not advance")
		}
		continuation = page.Continuation
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
	if len(backups) == 0 {
		return errors.New("no complete managed PostgreSQL backups were captured")
	}
	manifest := Archive{Version: 1, Source: SourceID(config), Objects: []Object{}, Backups: backups}
	writer := tar.NewWriter(output)
	for _, object := range objects {
		skip := false
		for _, prefix := range excluded {
			if strings.HasPrefix(object.Key, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		header := archiveHeader(object.Key, object.Size)
		if err := writer.WriteHeader(&header); err != nil {
			return err
		}
		body, err := client.OpenCaptured(ctx, object)
		if err != nil {
			return err
		}
		digest := sha256.New()
		count, copyErr := io.Copy(io.MultiWriter(writer, digest), io.LimitReader(body, object.Size+1))
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil || count != object.Size {
			return errors.New("captured source object could not be completely exported")
		}
		manifest.Objects = append(manifest.Objects, Object{Key: object.Key, Size: object.Size, ETag: object.ETag, SHA256: hex.EncodeToString(digest.Sum(nil))})
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
	client, err := objectstore.New(config)
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
			if manifest.Version != 1 || manifest.Source != source || len(manifest.Objects) != len(objects) {
				return Archive{}, errors.New("offsite completion manifest does not match this source and inventory")
			}
			for index, object := range objects {
				captured := manifest.Objects[index]
				if object.Key != captured.Key || object.Size != captured.Size || object.SHA256 != captured.SHA256 || captured.ETag == "" {
					return Archive{}, errors.New("offsite object size, digest or order does not match manifest")
				}
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
		if !managedKey(header.Name) || (len(objects) > 0 && header.Name <= objects[len(objects)-1].Key) {
			return Archive{}, errors.New("offsite archive contains unsafe, duplicate or unsorted object keys")
		}
		if len(objects) >= 100000 {
			return Archive{}, errors.New("offsite inventory exceeds 100000 objects")
		}
		digest := sha256.New()
		body := io.TeeReader(reader, digest)
		if metadataKey(header.Name) {
			if header.Size > 1<<20 {
				return Archive{}, errors.New("backup.info exceeds 1 MiB")
			}
			metadataBytes += header.Size
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
		if err := client.PutNew(ctx, header.Name, body, header.Size); err != nil {
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
