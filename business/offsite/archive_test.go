package offsite

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
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
	mutex         sync.Mutex
	objects       map[string][]byte
	uploads       map[string]map[int][]byte
	server        *httptest.Server
	changeOnGet   bool
	truncateOnGet bool
	reportedSizes map[string]int64
	getCount      int
}

func newArchiveS3(t *testing.T) *archiveS3 {
	t.Helper()
	fixture := &archiveS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
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
			Key  string
			Size int
			ETag string
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
			listing.Contents = append(listing.Contents, entry{key, size, etag(fixture.objects[key])})
		}
		_ = xml.NewEncoder(writer).Encode(listing)
		return
	}
	if request.Method == http.MethodPost && query.Has("uploads") {
		id := key
		fixture.uploads[id] = map[int][]byte{}
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
	case http.MethodPut:
		if _, exists := fixture.objects[key]; exists && request.Header.Get("If-None-Match") == "*" {
			writer.WriteHeader(412)
			return
		}
		body, _ := io.ReadAll(request.Body)
		fixture.objects[key] = body
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

// TestCaptureMetadataBudget rejects aggregate backup.info sizes before downloading or allocating their bodies.
func TestCaptureMetadataBudget(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	fixture.reportedSizes = map[string]int64{}
	for index := range 33 {
		key := fmt.Sprintf("source/%s/base/202610%02dT120000/backup.info", archiveServer(), index)
		fixture.objects[key] = []byte("status=STARTED\n")
		fixture.reportedSizes[key] = 1 << 20
	}
	var output bytes.Buffer
	err := Export(t.Context(), fixture.config("source"), &output)
	assert.True(err != nil && strings.Contains(err.Error(), "metadata exceeds 32 MiB"))
	assert.Equal(fixture.getCount, 0)
	assert.Equal(output.Len(), 0)
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
