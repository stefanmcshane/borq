package offsite

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"time"
)

// TestArchiveRoundTrip owns deterministic capture, scope isolation and verified S3 rehydration.
func TestArchiveRoundTrip(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	seedArchive(fixture)
	source := fixture.config("source")
	var first, second bytes.Buffer
	assert.NoErr(Export(t.Context(), source, &first))
	assert.NoErr(Export(t.Context(), source, &second))
	assert.Equal(first.Bytes(), second.Bytes())
	destination := fixture.config("restored")
	manifest, err := Restore(t.Context(), destination, SourceID(source), bytes.NewReader(first.Bytes()))
	assert.NoErr(err)
	assert.Equal(len(manifest.Objects), 5)
	assert.Equal(len(manifest.Backups), 1)
	for _, object := range manifest.Objects {
		assert.True(!strings.Contains(object.Key, "20261025"))
		fixture.mutex.Lock()
		assert.Equal(fixture.objects["restored/"+object.Key], fixture.objects["source/"+object.Key])
		fixture.mutex.Unlock()
	}
	_, err = Restore(t.Context(), destination, SourceID(source), bytes.NewReader(first.Bytes()))
	assert.True(err != nil && strings.Contains(err.Error(), "must be empty"))
}

// TestSqliteArchiveRoundTrip owns application and Pocket ID Litestream layouts,
// generation selection, and verified rehydration without unrelated objects.
func TestSqliteArchiveRoundTrip(t *testing.T) {
	for _, test := range []struct{ name, root string }{
		{"application", "sqlite/0123456789abcdef0123456789abcdef/web/generations/"},
		{"pocketid", "sqlite/generations/"},
		{"space pocketid", "sqlite/0123456789abcdef0123456789abcdef/pocketid/abcdef0123456789abcdef0123456789/generations/"},
		{"app named snapshots", "sqlite/0123456789abcdef0123456789abcdef/snapshots/generations/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			inventory, err := Freeze(t.Context(), fixture.config("source"))
			assert.NoErr(err)
			assert.Equal(len(inventory.Objects), 0)
			generation := test.root + "0123456789abcdef/"
			fixture.objects["source/"+generation+"snapshots/00000000.snapshot.lz4"] = []byte("snapshot")
			fixture.objects["source/"+generation+"wal/00000000_00000000.wal.lz4"] = []byte("wal")
			fixture.objects["source/"+test.root+"1123456789abcdef/wal/00000000_00000000.wal.lz4"] = []byte("no snapshot")
			fixture.objects["source/"+test.root+"invalid/snapshots/00000000.snapshot.lz4"] = []byte("invalid generation")
			fixture.objects["source/"+generation+"snapshots/not-a-snapshot"] = []byte("invalid snapshot")
			fixture.objects["source/sqlite/unsafe"] = []byte("unmanaged")
			inventory, err = Freeze(t.Context(), fixture.config("source"))
			assert.NoErr(err)
			assert.True(len(inventory.Objects) > 0)
			var output bytes.Buffer
			assert.NoErr(Export(t.Context(), fixture.config("source"), &output))
			manifest, err := Restore(t.Context(), fixture.config("recovered"), SourceID(fixture.config("source")), bytes.NewReader(output.Bytes()))
			assert.NoErr(err)
			assert.Equal(manifest.Version, 3)
			assert.Equal(manifest.Sqlite, []string{strings.TrimSuffix(generation, "/")})
			assert.Equal(len(manifest.Objects), 2)
			assert.Equal(fixture.objects["recovered/"+generation+"snapshots/00000000.snapshot.lz4"], []byte("snapshot"))
			assert.Equal(fixture.objects["recovered/"+generation+"wal/00000000_00000000.wal.lz4"], []byte("wal"))
		})
	}
}

// TestArchiveSourceFailures proves captured identities and complete backup dependencies fail before a terminal manifest.
func TestArchiveSourceFailures(t *testing.T) {
	for _, failure := range []string{"changed-etag", "missing-base", "missing-wal", "truncated-body"} {
		t.Run(failure, func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			seedArchive(fixture)
			fixture.mutex.Lock()
			switch failure {
			case "changed-etag":
				fixture.changeOnGet = true
			case "missing-base":
				delete(fixture.objects, "source/"+archiveServer()+"/base/20261024T120000/data.tar.gz")
			case "missing-wal":
				delete(fixture.objects, "source/"+archiveServer()+"/wals/0000000100000000/000000010000000000000002.gz")
			case "truncated-body":
				fixture.truncateOnGet = true
			}
			fixture.mutex.Unlock()
			var output bytes.Buffer
			err := Export(t.Context(), fixture.config("source"), &output)
			assert.True(err != nil)
			assert.True(!bytes.Contains(output.Bytes(), []byte("borq-manifest.json")))
			if failure == "missing-base" {
				assert.True(strings.Contains(err.Error(), "data archive is missing"))
			}
			if failure == "missing-wal" {
				assert.True(strings.Contains(err.Error(), "WAL segments are missing"))
			}
		})
	}
}

// TestArchiveRejectsUntrustedStreams owns restore path safety and failure receipts at the import boundary.
func TestArchiveRejectsUntrustedStreams(t *testing.T) {
	fixture := newArchiveS3(t)
	seedArchive(fixture)
	var output bytes.Buffer
	if err := Export(t.Context(), fixture.config("source"), &output); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"truncated", "digest", "duplicate", "unsafe", "symlink", "no-manifest", "trailing"} {
		t.Run(failure, func(t *testing.T) {
			assert := is.New(t)
			var edited bytes.Buffer
			input := tar.NewReader(bytes.NewReader(output.Bytes()))
			writer := tar.NewWriter(&edited)
			for index := 0; ; index++ {
				header, err := input.Next()
				if err == io.EOF {
					break
				}
				assert.NoErr(err)
				body, err := io.ReadAll(input)
				assert.NoErr(err)
				if failure == "no-manifest" && header.Name == "borq-manifest.json" {
					break
				}
				if index == 0 {
					if failure == "unsafe" {
						header.Name = "../outside"
					}
					if failure == "symlink" {
						header.Typeflag = tar.TypeSymlink
						header.Size = 0
						body = nil
					}
					if failure == "digest" && len(body) > 0 {
						body[0] ^= 1
					}
				}
				assert.NoErr(writer.WriteHeader(header))
				_, err = writer.Write(body)
				assert.NoErr(err)
				if failure == "duplicate" && index == 0 {
					assert.NoErr(writer.WriteHeader(header))
					_, err = writer.Write(body)
					assert.NoErr(err)
				}
			}
			assert.NoErr(writer.Close())
			data := edited.Bytes()
			if failure == "truncated" {
				data = data[:len(data)/2]
			}
			if failure == "trailing" {
				data = append(data, []byte("unexpected")...)
			}
			_, err := Restore(t.Context(), fixture.config("reject-"+failure), SourceID(fixture.config("source")), bytes.NewReader(data))
			assert.True(err != nil)
			switch failure {
			case "unsafe", "duplicate":
				assert.True(strings.Contains(err.Error(), "unsafe, duplicate or unsorted"))
			case "symlink":
				assert.True(strings.Contains(err.Error(), "unsupported tar"))
			case "digest":
				assert.True(strings.Contains(err.Error(), "digest or order"))
			case "no-manifest":
				assert.True(strings.Contains(err.Error(), "no completion manifest"))
			case "trailing":
				assert.True(strings.Contains(err.Error(), "trailing data"))
			}
		})
	}
}

func archiveServer() string {
	id := strings.Repeat("b", 32)
	return "organizations/" + strings.Repeat("a", 32) + "/postgres/" + id + "/archive-" + id
}
func seedArchive(fixture *archiveS3) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	server := archiveServer()
	fixture.objects["source/"+server+"/base/20261024T120000/backup.info"] = []byte("status=DONE\nversion=180006\nsize=1048576\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-10-24 12:00:00+00:00\ntablespaces=None\n")
	fixture.objects["source/"+server+"/base/20261024T120000/data.tar.gz"] = bytes.Repeat([]byte("physical-data"), 1024)
	fixture.objects["source/"+server+"/wals/0000000100000000/000000010000000000000001.gz"] = []byte("wal1")
	fixture.objects["source/"+server+"/wals/0000000100000000/000000010000000000000002.gz"] = []byte("wal2")
	fixture.objects["source/"+server+"/wals/00000002.history"] = []byte("timeline-history")
	fixture.objects["source/"+server+"/base/20261025T120000/backup.info"] = []byte("status=STARTED\n")
	fixture.objects["source/"+server+"/base/20261025T120000/data.tar.gz"] = []byte("concurrent-incomplete")
	fixture.objects["source/organizations/"+strings.Repeat("a", 32)+"/future/object"] = []byte("unrelated")
	fixture.objects["source/loki/chunk"] = []byte("mutable-logs")
}

type archiveS3 struct {
	mutex            sync.Mutex
	objects          map[string][]byte
	uploads          map[string]map[int][]byte
	server           *httptest.Server
	changeOnGet      bool
	truncateOnGet    bool
	reportedSizes    map[string]int64
	getCount         int
	modified         map[string]time.Time
	failComplete     string
	encryption       map[string]string
	uploadEncryption map[string]string
}

func newArchiveS3(t *testing.T) *archiveS3 {
	t.Helper()
	fixture := &archiveS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, encryption: map[string]string{}, uploadEncryption: map[string]string{}}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}
func (fixture *archiveS3) config(prefix string) objectstore.Config {
	return objectstore.Config{Endpoint: fixture.server.URL, Region: "us-east-1", Bucket: "bucket", Prefix: prefix, PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "access", SecretKey: "secret"}}
}
func etag(data []byte) string {
	digest := sha256.Sum256(data)
	return `"` + hex.EncodeToString(digest[:]) + `"`
}
func (fixture *archiveS3) serve(writer http.ResponseWriter, request *http.Request) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	key := strings.TrimPrefix(request.URL.Path, "/bucket/")
	query := request.URL.Query()
	writer.Header().Set("Content-Type", "application/xml")
	if query.Get("list-type") == "2" {
		type entry struct {
			Key          string
			Size         int
			ETag         string
			LastModified string
		}
		listing := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Contents    []entry
			IsTruncated bool
		}{}
		keys := []string{}
		for key := range fixture.objects {
			if strings.HasPrefix(key, query.Get("prefix")) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			size := len(fixture.objects[key])
			if reported, exists := fixture.reportedSizes[key]; exists {
				size = int(reported)
			}
			modified := time.Now().UTC()
			if value, exists := fixture.modified[key]; exists {
				modified = value
			}
			listing.Contents = append(listing.Contents, entry{key, size, etag(fixture.objects[key]), modified.Format(time.RFC3339)})
		}
		_ = xml.NewEncoder(writer).Encode(listing)
		return
	}
	if request.Method == http.MethodPost && query.Has("uploads") {
		id := key
		fixture.uploads[id] = map[int][]byte{}
		fixture.uploadEncryption[id] = request.Header.Get("X-Amz-Server-Side-Encryption")
		writer.Header().Set("X-Amz-Server-Side-Encryption", fixture.uploadEncryption[id])
		_, _ = fmt.Fprintf(writer, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
		return
	}
	if upload := query.Get("uploadId"); upload != "" {
		if request.Method == http.MethodDelete {
			delete(fixture.uploads, upload)
			writer.WriteHeader(204)
			return
		}
		if request.Method == http.MethodPut {
			part, _ := strconv.Atoi(query.Get("partNumber"))
			body, _ := io.ReadAll(request.Body)
			fixture.uploads[upload][part] = body
			writer.Header().Set("ETag", etag(body))
			return
		}
		if request.Method == http.MethodPost {
			if fixture.failComplete != "" && strings.HasSuffix(key, fixture.failComplete) {
				writer.WriteHeader(http.StatusForbidden)
				return
			}
			if _, exists := fixture.objects[key]; exists && request.Header.Get("If-None-Match") == "*" {
				writer.WriteHeader(412)
				return
			}
			parts := fixture.uploads[upload]
			body := []byte{}
			for part := 1; part <= len(parts); part++ {
				body = append(body, parts[part]...)
			}
			fixture.objects[key] = body
			fixture.encryption[key] = fixture.uploadEncryption[upload]
			delete(fixture.uploads, upload)
			_, _ = fmt.Fprintf(writer, "<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>", key, etag(body))
			return
		}
	}
	switch request.Method {
	case http.MethodGet:
		fixture.getCount++
		body, exists := fixture.objects[key]
		if !exists {
			writer.WriteHeader(404)
			return
		}
		if fixture.changeOnGet && strings.HasSuffix(key, "data.tar.gz") {
			writer.WriteHeader(412)
			return
		}
		if strings.HasPrefix(key, "source/") && request.Header.Get("If-Match") == "" {
			writer.WriteHeader(400)
			return
		}
		if match := request.Header.Get("If-Match"); match != "" && match != etag(body) {
			writer.WriteHeader(412)
			return
		}
		writer.Header().Set("ETag", etag(body))
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if fixture.truncateOnGet && strings.HasSuffix(key, "data.tar.gz") {
			body = body[:len(body)/2]
		}
		_, _ = writer.Write(body)
	case http.MethodDelete:
		delete(fixture.objects, key)
		writer.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		if _, exists := fixture.objects[key]; exists && request.Header.Get("If-None-Match") == "*" {
			writer.WriteHeader(412)
			return
		}
		body, _ := io.ReadAll(request.Body)
		fixture.objects[key] = body
		fixture.encryption[key] = request.Header.Get("X-Amz-Server-Side-Encryption")
		writer.Header().Set("X-Amz-Server-Side-Encryption", fixture.encryption[key])
		writer.Header().Set("ETag", etag(body))
	default:
		writer.WriteHeader(405)
	}
}

// TestArchiveLargeObjectHeader protects the transport's >=8 GiB physical backup sizes without allocating their contents.
func TestArchiveLargeObjectHeader(t *testing.T) {
	assert := is.New(t)
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	header := archiveHeader(archiveServer()+"/base/20261024T120000/data.tar.gz", 8<<30)
	assert.NoErr(writer.WriteHeader(&header))
	decoded, err := tar.NewReader(bytes.NewReader(output.Bytes())).Next()
	assert.NoErr(err)
	assert.Equal(decoded.Size, int64(8<<30))
	assert.Equal(decoded.Name, header.Name)
}

// TestCaptureMetadataBudget rejects PostgreSQL and native metadata before downloading or allocating their bodies.
func TestCaptureMetadataBudget(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-%t", native), func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			fixture.reportedSizes = map[string]int64{}
			count, size := 33, int64(1<<20)
			if native {
				count, size = 513, 64<<10
			}
			for index := range count {
				key := fmt.Sprintf("source/%s/base/202610%02dT120000/backup.info", archiveServer(), index)
				if native {
					key = fmt.Sprintf("source/management/sqlite/%032x/complete.json", index)
				}
				fixture.objects[key] = []byte("metadata")
				fixture.reportedSizes[key] = size
			}
			var output bytes.Buffer
			err := Export(t.Context(), fixture.config("source"), &output)
			assert.True(err != nil && strings.Contains(err.Error(), "metadata"))
			assert.Equal(fixture.getCount, 0)
			assert.Equal(output.Len(), 0)
		})
	}
}

// TestRecoveryRejectsFailedProducer requires the dump process to succeed even after it emits a valid complete archive.
func TestRecoveryRejectsFailedProducer(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	seedArchive(fixture)
	source := fixture.config("source")
	var archive bytes.Buffer
	assert.NoErr(Export(t.Context(), source, &archive))
	directory := t.TempDir()
	file := filepath.Join(directory, "objects.tar")
	assert.NoErr(os.WriteFile(file, archive.Bytes(), 0600))
	snapshot := strings.Repeat("1", 64)
	script := "#!/bin/sh\nshift 2\ncase \"$1\" in\n snapshots) printf '%s' '[{\"id\":\"" + snapshot + "\"}]' ;;\n dump) /bin/cat '" + strings.ReplaceAll(file, "'", "'\"'\"'") + "'; exit 17 ;;\n *) exit 1 ;;\nesac\n"
	assert.NoErr(os.WriteFile(filepath.Join(directory, "restic"), []byte(script), 0700))
	t.Setenv("PATH", directory)
	destination := StorageConfig{Endpoint: source.Endpoint, Region: source.Region, Bucket: source.Bucket, Prefix: "failed-producer", PathStyle: true, AccessKey: "access", SecretKey: "secret"}
	_, err := Recover(t.Context(), RecoveryConfig{Repository: Repository{URL: "s3:https://offsite.example/backups/borq", Password: "retained-password", AccessKey: "access", SecretKey: "secret", Region: "region"}, Source: SourceID(source), Destination: destination}, snapshot)
	assert.True(err != nil && strings.Contains(err.Error(), "producer failed"))
	// Verified imported objects still cannot make a failed producer a successful recovery.
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	assert.True(len(fixture.objects["failed-producer/"+archiveServer()+"/base/20261024T120000/backup.info"]) > 0)
}

// TestStreamingUploadCreatesOnly owns conditional multipart completion and abort cleanup at the real S3 HTTP boundary.
func TestStreamingUploadCreatesOnly(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	client, err := objectstore.New(fixture.config("destination"))
	assert.NoErr(err)
	assert.NoErr(client.PutNew(t.Context(), "existing", strings.NewReader("original"), 8))
	err = client.PutNew(t.Context(), "existing", strings.NewReader("replacement"), 11)
	assert.True(err != nil && strings.Contains(err.Error(), "complete new object"))
	err = client.PutNew(t.Context(), "truncated", strings.NewReader("short"), 100)
	assert.True(err != nil && strings.Contains(err.Error(), "source is truncated"))
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	assert.Equal(string(fixture.objects["destination/existing"]), "original")
	_, exists := fixture.objects["destination/truncated"]
	assert.True(!exists)
	assert.Equal(len(fixture.uploads), 0)
}

// TestNativeManagementOffsiteRoundTrip protects management-only and mixed captures, excludes partials and performs downstream SQLite recovery without the original.
func TestNativeManagementOffsiteRoundTrip(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed-%t", mixed), func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			if mixed {
				seedArchive(fixture)
			}
			source := fixture.config("source")
			client, err := objectstore.New(source)
			assert.NoErr(err)
			directory := t.TempDir()
			database, err := sqlite.New(t.Context(), filepath.Join(directory, "original.sqlite"))
			assert.NoErr(err)
			assert.NoErr(database.Commit(t.Context(), []sqlite.Write{{Collection: "organization", Key: "recovered-org", Value: []byte(`{"name":"recovered-org"}`)}}))
			snapshot := filepath.Join(directory, "snapshot.sqlite")
			assert.NoErr(database.Snapshot(t.Context(), snapshot))
			assert.NoErr(database.Close())
			bundle := filepath.Join(directory, "bundle.tar")
			assert.NoErr(managementbackup.BundleSQLite(t.Context(), snapshot, []managementbackup.Connection{}, bundle))
			identity, err := age.GenerateX25519Identity()
			assert.NoErr(err)
			id, err := managementbackup.NewID()
			assert.NoErr(err)
			artifact, err := managementbackup.Publish(t.Context(), client, managementbackup.Artifact{Version: 1, Kind: "sqlite", ID: id, Recipient: identity.Recipient().String(), CapturedAt: time.Now().UTC()}, bundle)
			assert.NoErr(err)
			assert.NoErr(client.Put(t.Context(), "management/sqlite/"+strings.Repeat("c", 32)+"/payload", []byte("partial-private-bytes")))
			var output bytes.Buffer
			assert.NoErr(Export(t.Context(), source, &output))
			destination := fixture.config("rehydrated")
			manifest, err := Restore(t.Context(), destination, SourceID(source), bytes.NewReader(output.Bytes()))
			assert.NoErr(err)
			assert.Equal(len(manifest.Native), 1)
			assert.Equal(manifest.Native[0], artifact)
			for _, object := range manifest.Objects {
				assert.True(!strings.Contains(object.Key, strings.Repeat("c", 32)))
			}
			assert.NoErr(os.RemoveAll(directory))
			destinationClient, err := objectstore.New(destination)
			assert.NoErr(err)
			unrelated := artifact
			unrelated.Kind = "openbao"
			unrelated.ID = strings.Repeat("d", 32)
			unrelatedMarker, err := json.Marshal(unrelated)
			assert.NoErr(err)
			assert.NoErr(destinationClient.Put(t.Context(), managementbackup.CompletionKey(unrelated), unrelatedMarker))
			restoredDir := filepath.Join(t.TempDir(), "recovered")
			assert.NoErr(managementbackup.Restore(t.Context(), managementbackup.RestoreInput{Storage: destination, Kind: "sqlite", ID: id, Identity: identity.String(), DataDir: restoredDir}))
			recovered, err := sqlite.New(t.Context(), filepath.Join(restoredDir, "borq.sqlite"))
			assert.NoErr(err)
			defer recovered.Close()
			record, err := recovered.Get(t.Context(), "organization", "recovered-org")
			assert.NoErr(err)
			assert.True(bytes.Contains(record.Value, []byte("recovered-org")))
			wrong, err := age.GenerateX25519Identity()
			assert.NoErr(err)
			wrongPath := filepath.Join(t.TempDir(), "wrong-openbao")
			assert.True(managementbackup.Restore(t.Context(), managementbackup.RestoreInput{Storage: destination, Kind: "sqlite", ID: id, Identity: wrong.String(), DataDir: wrongPath}) != nil)
			_, err = os.Stat(wrongPath)
			assert.True(os.IsNotExist(err))
			fixture.mutex.Lock()
			payloadKey := "rehydrated/" + managementbackup.PayloadKey(artifact)
			corrupted := append([]byte(nil), fixture.objects[payloadKey]...)
			corrupted[len(corrupted)-1] ^= 1
			fixture.objects[payloadKey] = corrupted
			digest := sha256.Sum256(corrupted)
			artifact.SHA256 = hex.EncodeToString(digest[:])
			marker, _ := json.Marshal(artifact)
			fixture.objects["rehydrated/"+managementbackup.CompletionKey(artifact)] = marker
			fixture.mutex.Unlock()
			nativeCalls := 0
			vault := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				nativeCalls++
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			defer vault.Close()
			err = managementbackup.Restore(t.Context(), managementbackup.RestoreInput{Storage: destination, Kind: "sqlite", ID: id, Identity: identity.String(), DataDir: wrongPath})
			assert.True(err != nil && strings.Contains(err.Error(), "authentication"))
			_, err = os.Stat(wrongPath)
			assert.True(os.IsNotExist(err))
			// The same corrupt ciphertext labelled as Raft must be rejected before the recovery endpoint is contacted.
			artifact.Kind = "openbao"
			marker, _ = json.Marshal(artifact)
			assert.NoErr(destinationClient.Put(t.Context(), managementbackup.PayloadKey(artifact), corrupted))
			assert.NoErr(destinationClient.Put(t.Context(), managementbackup.CompletionKey(artifact), marker))
			err = managementbackup.Restore(t.Context(), managementbackup.RestoreInput{Storage: destination, Kind: "openbao", ID: id, Identity: identity.String(), OpenBao: managementbackup.OpenBaoRecovery{Address: vault.URL, OriginalToken: "original", UnsealKeys: []string{"retained"}}})
			assert.True(err != nil && strings.Contains(err.Error(), "authentication"))
			assert.Equal(nativeCalls, 0)

		})
	}
}

// TestNativeRetention preserves the other component and partial uploads while removing old completed payloads and markers.
func TestNativeRetention(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	config := fixture.config("source")
	client, err := objectstore.New(config)
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "native")
	assert.NoErr(os.WriteFile(file, []byte("native payload transport fixture"), 0600))
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	artifacts := []managementbackup.Artifact{}
	for index, kind := range []string{"sqlite", "openbao", "openbao", "openbao"} {
		artifact, err := managementbackup.Publish(t.Context(), client, managementbackup.Artifact{Version: 1, Recipient: identity.Recipient().String(), Kind: kind, ID: fmt.Sprintf("%032x", index), CapturedAt: time.Unix(int64(index+1), 0).UTC()}, file)
		assert.NoErr(err)
		artifacts = append(artifacts, artifact)
	}
	partial := "management/openbao/" + strings.Repeat("f", 32) + "/payload"
	assert.NoErr(client.Put(t.Context(), partial, []byte("incomplete")))
	stale := "management/openbao/" + strings.Repeat("e", 32) + "/payload"
	assert.NoErr(client.Put(t.Context(), stale, []byte("abandoned")))
	fixture.mutex.Lock()
	fixture.modified = map[string]time.Time{"source/" + stale: time.Now().Add(-48 * time.Hour)}
	fixture.mutex.Unlock()
	assert.NoErr(managementbackup.Prune(t.Context(), config, "openbao", 1))
	entries, err := managementbackup.List(t.Context(), config)
	assert.NoErr(err)
	assert.Equal(len(entries), 2)
	assert.Equal(entries[0].Artifact, artifacts[0])
	assert.Equal(entries[1].Artifact, artifacts[3])
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	for _, artifact := range artifacts[1:3] {
		_, payload := fixture.objects["source/"+managementbackup.PayloadKey(artifact)]
		_, marker := fixture.objects["source/"+managementbackup.CompletionKey(artifact)]
		assert.True(!payload && !marker)
	}
	assert.True(len(fixture.objects["source/"+partial]) > 0)
	_, staleExists := fixture.objects["source/"+stale]
	assert.True(!staleExists)
}

func TestNativeFailedPublicationCleanup(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	fixture.failComplete = "/complete.json"
	client, err := objectstore.New(fixture.config("source"))
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "native")
	assert.NoErr(os.WriteFile(file, []byte("native payload"), 0600))
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	_, err = managementbackup.Publish(t.Context(), client, managementbackup.Artifact{Version: 1, Recipient: identity.Recipient().String(), Kind: "sqlite", ID: strings.Repeat("a", 32), CapturedAt: time.Now()}, file)
	assert.True(err != nil)
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.objects) != 0 || len(fixture.uploads) != 0 {
		t.Fatalf("failed native publication retained %d completed objects and %d multipart uploads", len(fixture.objects), len(fixture.uploads))
	}
}
