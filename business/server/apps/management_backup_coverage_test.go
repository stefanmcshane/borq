package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/robfig/cron/v3"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementBackupCoverage owns the expected workload inventory and live
// archive inspection. A successful offsite run must not hide missing databases.
func TestManagementBackupCoverage(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	const spaceID = "0123456789abcdef0123456789abcdef"
	const providerID = "abcdef0123456789abcdef0123456789"
	snapshot := "/generations/0123456789abcdef/snapshots/00000000.snapshot.lz4"
	wal := "/generations/0123456789abcdef/wal/00000000_00000000.wal.lz4"
	objects := map[string]coverageObject{
		"sqlite/" + spaceID + "/current" + snapshot:                {body: []byte("encrypted-snapshot"), modified: now},
		"sqlite/" + spaceID + "/stale" + snapshot:                  {body: []byte("encrypted-snapshot"), modified: now.Add(-72 * time.Hour)},
		"sqlite/" + spaceID + "/wal-only" + wal:                    {body: []byte("encrypted-wal"), modified: now},
		"sqlite/" + spaceID + "/pocketid/" + providerID + snapshot: {body: []byte("encrypted-identity"), modified: now},
	}
	var mutex sync.Mutex
	source := newCoverageStorage(t, &mutex, objects)
	input := componentManagerInput(t, fixture.kubeconfig)
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	storage, err := manager.prepareManagementS3(t.Context(), ManagementS3Input{Endpoint: source.URL, Bucket: "archives", Region: "us-east-1", PathStyle: true, AccessKey: "private-access", SecretKey: "private-secret"})
	assert.NoErr(err)
	disabled := false
	space := spaceRecord{Space: Space{ID: spaceID, Organization: "org", Name: "space"}, SqliteStorage: storage}
	for _, name := range []string{"current", "stale", "wal-only", "missing", "disabled"} {
		space.Space.Configuration.Apps = append(space.Space.Configuration.Apps, App{Name: name, Sqlite: SqliteSettings{Enabled: true, Backups: name != "disabled"}})
	}
	values := []struct {
		collection, key string
		value           any
	}{
		{"space", "org/space", space},
		{"space-storage", spaceID, spaceStorageRecord{Storage: storage, Status: SpaceBackupStorage{State: "ready"}}},
		{"organization-pocketid", "org", organizationPocketIdRecord{Organization: "org", ID: providerID}},
		{"space-pocketid", "org/space/id", spacePocketIdRecord{Organization: "org", Space: "space", SpaceID: spaceID, PocketId: PocketId{Name: "id", ID: providerID, State: "ready"}}},
		{"space-pocketid", "org/space/removed", spacePocketIdRecord{PocketId: PocketId{Name: "removed", State: "removed"}}},
		{"postgres", "org/space/pg", postgresRecord{Organization: "org", Space: "space", Postgres: Postgres{Name: "pg", Specification: PostgresInput{Backup: PostgresBackupPolicy{Enabled: &disabled}}}}},
		{"postgres", "org/space/pending", postgresRecord{Organization: "org", Space: "space", Postgres: Postgres{Name: "pending"}}},
		{"management-offsite", "shared", offsitePolicy{Enabled: true, Interval: "6h"}},
		{"management-offsite-status", "shared", ManagementOffsiteStatus{State: "succeeded", LastSuccess: now}},
	}
	writes := []sqlite.Write{}
	for _, value := range values {
		write, err := documentWrite(value.collection, value.key, value.value, 0)
		assert.NoErr(err)
		writes = append(writes, write)
	}
	assert.NoErr(manager.database.Commit(t.Context(), writes))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	before := []sqlite.Record{}
	for _, write := range writes {
		record, err := manager.database.Get(t.Context(), write.Collection, write.Key)
		assert.NoErr(err)
		before = append(before, record)
	}
	report, err := manager.GetManagementBackupCoverage(t.Context())
	assert.NoErr(err)
	assert.Equal(report.State, "attention")
	states := map[string]string{}
	for _, component := range report.Components {
		states[component.Kind+"/"+component.Name] = component.State
		if component.Name == "org/space/current" {
			assert.Equal(component.LastSuccess, now)
			assert.Equal(component.StaleAfter, now.Add(48*time.Hour))
		}
	}
	assert.Equal(states, map[string]string{
		"management/sqlite": "disabled", "management/openbao": "disabled", "offsite/export": "available",
		"sqlite/org/space/current": "available", "sqlite/org/space/stale": "stale", "sqlite/org/space/wal-only": "missing", "sqlite/org/space/missing": "missing", "sqlite/org/space/disabled": "disabled",
		"pocketid/org/identity": "missing", "pocketid/org/space/id": "available", "postgres/org/space/pg": "disabled", "postgres/org/space/pending": "missing",
	})
	// Status must neither repair missing configuration nor rewrite durable state.
	for _, previous := range before {
		record, err := manager.database.Get(t.Context(), previous.Collection, previous.Key)
		assert.NoErr(err)
		assert.Equal(record, previous)
	}
	for _, collection := range []string{"management-backups", "sqlite-archive-key"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		assert.Equal(len(records), 0)
	}
	encoded, err := json.Marshal(report)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "private-access"))
	assert.True(!strings.Contains(string(encoded), "private-secret"))
	assert.True(!strings.Contains(string(encoded), source.URL))
	mutex.Lock()
	delete(objects, "sqlite/"+spaceID+"/current"+snapshot)
	mutex.Unlock()
	report, err = manager.GetManagementBackupCoverage(t.Context())
	assert.NoErr(err)
	for _, component := range report.Components {
		if component.Name == "org/space/current" {
			assert.Equal(component.State, "missing")
		}
	}
	fixture.mutex.Lock()
	fixture.vaultDenied = true
	fixture.mutex.Unlock()
	report, err = manager.GetManagementBackupCoverage(t.Context())
	assert.NoErr(err)
	for _, component := range report.Components {
		if component.Name == "org/space/current" {
			assert.Equal(component.State, "unknown")
		}
	}
}

// TestManagementNativeBackupCoverage detects deleted native payloads despite
// persisted success and preserves failed and stale states across inspections.
func TestManagementNativeBackupCoverage(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	payload := []byte("native-encrypted-payload")
	digest := sha256.Sum256(payload)
	artifact := managementbackup.Artifact{Version: 1, Kind: "sqlite", ID: strings.Repeat("a", 32), Recipient: identity.Recipient().String(), CapturedAt: now, Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
	marker, err := json.Marshal(artifact)
	assert.NoErr(err)
	objects := map[string]coverageObject{managementbackup.PayloadKey(artifact): {body: payload, modified: now}, managementbackup.CompletionKey(artifact): {body: marker, modified: now}}
	var mutex sync.Mutex
	source := newCoverageStorage(t, &mutex, objects)
	manager, err := NewManager(t.Context(), componentManagerInput(t, fixture.kubeconfig))
	assert.NoErr(err)
	defer manager.Close()
	storage, err := manager.prepareManagementS3(t.Context(), ManagementS3Input{Endpoint: source.URL, Bucket: "archives", Region: "us-east-1", PathStyle: true, AccessKey: "private-access", SecretKey: "private-secret"})
	assert.NoErr(err)
	policy, err := documentWrite("management-backups", "shared", ManagementBackupInput{Enabled: true, Interval: "1h"}, 0)
	assert.NoErr(err)
	result, err := documentWrite("management-backup-result", "sqlite", ManagementBackupComponent{State: "succeeded", Artifact: artifact, LastSuccess: now, Storage: storage}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{policy, result}))
	components, err := manager.nativeBackupCoverage(t.Context(), now)
	assert.NoErr(err)
	assert.Equal(components[0].State, "available")
	assert.Equal(components[1].State, "missing")
	components, err = manager.nativeBackupCoverage(t.Context(), now.Add(3*time.Hour))
	assert.NoErr(err)
	assert.Equal(components[0].State, "stale")
	mutex.Lock()
	delete(objects, managementbackup.PayloadKey(artifact))
	mutex.Unlock()
	components, err = manager.nativeBackupCoverage(t.Context(), now)
	assert.NoErr(err)
	assert.Equal(components[0].State, "unknown")
	result, err = documentWrite("management-backup-result", "sqlite", ManagementBackupComponent{State: "failed", Artifact: artifact, Storage: storage, Error: "native capture failed"}, 1)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{result}))
	components, err = manager.nativeBackupCoverage(t.Context(), now)
	assert.NoErr(err)
	assert.Equal(components[0].State, "failed")
}

type coverageObject struct {
	body     []byte
	modified time.Time
}

// TestPostgresBackupCoverage requires complete base/WAL evidence in the current
// archive and uses the database schedule, rather than object write time, for age.
func TestPostgresBackupCoverage(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	completed := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	metadata := []byte("status=DONE\nversion=180006\nsize=1048576\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-11-04 12:00:00+00:00\ntablespaces=None\n")
	wal := "server/wals/0000000100000000/000000010000000000000001.gz"
	objects := map[string]coverageObject{
		"server/base/20261104T120000/backup.info": {body: metadata, modified: completed.Add(7 * 24 * time.Hour)},
		"server/base/20261104T120000/data.tar.gz": {body: []byte("database"), modified: completed},
		wal: {body: []byte("wal"), modified: completed},
	}
	var mutex sync.Mutex
	source := newCoverageStorage(t, &mutex, objects)
	manager, err := NewManager(t.Context(), componentManagerInput(t, fixture.kubeconfig))
	assert.NoErr(err)
	defer manager.Close()
	storage, err := manager.prepareManagementS3(t.Context(), ManagementS3Input{Endpoint: source.URL, Bucket: "archives", Region: "us-east-1", PathStyle: true, AccessKey: "private-access", SecretKey: "private-secret"})
	assert.NoErr(err)
	schedule, err := cron.ParseStandard("0 0 * * *")
	assert.NoErr(err)
	target := backupCoverageTarget{component: BackupCoverageComponent{Kind: "postgres", Name: "org/dev/pg"}, storage: storage, server: "server", major: 18, schedule: schedule}
	component := manager.inspectBackupTarget(t.Context(), target, completed.Add(time.Hour))
	assert.Equal(component.State, "available")
	assert.True(component.LastSuccess.Equal(completed))
	assert.True(component.StaleAfter.Equal(time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)))
	component = manager.inspectBackupTarget(t.Context(), target, completed.Add(24*time.Hour))
	assert.Equal(component.State, "stale")
	mutex.Lock()
	delete(objects, wal)
	mutex.Unlock()
	component = manager.inspectBackupTarget(t.Context(), target, completed.Add(time.Hour))
	assert.Equal(component.State, "missing")
}

// TestOffsiteBackupCoverage prevents stale, failed or previous-destination
// exports from being reported as protection for the currently selected policy.
func TestOffsiteBackupCoverage(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name, repository, credentials, execution, want string
		age                                            time.Duration
	}{
		{"current", "current", "current", "succeeded", "available", time.Hour},
		{"overdue", "current", "current", "succeeded", "stale", 13 * time.Hour},
		{"failed", "current", "current", "failed", "failed", time.Hour},
		{"first export failed", "", "", "failed", "failed", time.Hour},
		{"changed repository", "previous", "current", "succeeded", "missing", time.Hour},
		{"changed credentials", "current", "previous", "succeeded", "missing", time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementStorageFixture(t)
			manager, err := NewManager(t.Context(), componentManagerInput(t, fixture.kubeconfig))
			assert.NoErr(err)
			defer manager.Close()
			policy, err := documentWrite("management-offsite", "shared", offsitePolicy{Enabled: true, Interval: "6h", Repository: "current", CredentialsRef: "current"}, 0)
			assert.NoErr(err)
			status, err := documentWrite("management-offsite-status", "shared", ManagementOffsiteStatus{State: test.execution, PolicyRevision: 1, LastSuccess: now.Add(-test.age), Repository: test.repository, CredentialsRef: test.credentials, Error: "export failed"}, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{policy, status}))
			component, err := manager.offsiteBackupCoverage(t.Context(), now)
			assert.NoErr(err)
			assert.Equal(component.State, test.want)
			if test.want == "missing" {
				assert.True(component.LastSuccess.IsZero())
			}
		})
	}
}

// The S3 protocol fixture serves actual catalogue bytes; controller lifecycle
// and payload decryption have separate real-service acceptance tests.
func newCoverageStorage(t *testing.T, mutex *sync.Mutex, objects map[string]coverageObject) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if request.Method != http.MethodGet {
			t.Error("backup inspection attempted a storage mutation")
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Query().Get("list-type") == "2" {
			type entry struct {
				Key          string
				Size         int
				ETag         string
				LastModified string
			}
			listing := struct {
				XMLName  xml.Name `xml:"ListBucketResult"`
				Contents []entry
			}{}
			for key, object := range objects {
				if strings.HasPrefix(key, request.URL.Query().Get("prefix")) {
					listing.Contents = append(listing.Contents, entry{Key: key, Size: len(object.body), ETag: `"fixture"`, LastModified: object.modified.Format(time.RFC3339)})
				}
			}
			writer.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(writer).Encode(listing)
			return
		}
		object, found := objects[strings.TrimPrefix(request.URL.Path, "/archives/")]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("ETag", `"fixture"`)
		writer.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
		_, _ = writer.Write(object.body)
	}))
	t.Cleanup(server.Close)
	return server
}
