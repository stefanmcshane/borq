package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestManifests isolates recovery reads from destination archives and protects the upstream plugin wire contract.
func TestManifests(t *testing.T) {
	check := is.New(t)
	input := Input{BackupEnabled: true, Namespace: "space", Name: "pg-destination", Owner: "destination-id", Image: "ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie", Instances: 1, StorageSize: "1Gi", Database: "app", Username: "app", RetentionDays: 30, BackupSchedule: "0 0 0 * * *", SnapshotSchedule: "0 0 * * * *", SnapshotClass: "csi-snapshots", Storage: Storage{Bucket: "backups", Prefix: "destination-prefix", ServerName: "destination-server", PathStyle: true}, Recovery: &Recovery{Storage: Storage{Bucket: "backups", Prefix: "source-prefix", ServerName: "original-server"}, BackupID: "20261024T000000"}, Check: Check{Enabled: true, Schedule: "0 3 * * 0", ApiURL: "https://borq.test/api/v1/resource/restore-check", TimeoutSeconds: 1800}}
	manifests := Manifests(input)
	objects := map[string]map[string]any{}
	for _, manifest := range manifests {
		metadata := manifest["metadata"].(map[string]any)
		objects[metadata["name"].(string)] = manifest
	}
	destination := objects["pg-destination-store"]
	source := objects["pg-destination-source"]
	value, _, err := unstructured.NestedString(destination, "spec", "configuration", "destinationPath")
	check.NoErr(err)
	check.Equal(value, "s3://backups/destination-prefix")
	value, _, err = unstructured.NestedString(destination, "spec", "configuration", "s3Credentials", "sessionToken", "key")
	check.NoErr(err)
	check.Equal(value, "SESSION_TOKEN")
	value, _, err = unstructured.NestedString(source, "spec", "configuration", "destinationPath")
	check.NoErr(err)
	check.Equal(value, "s3://backups/source-prefix")
	_, exists, err := unstructured.NestedString(destination, "spec", "configuration", "serverName")
	check.NoErr(err)
	check.True(!exists)

	for _, store := range []map[string]any{destination, source} {
		for _, archive := range []string{"data", "wal"} {
			encryption, _, err := unstructured.NestedString(store, "spec", "configuration", archive, "encryption")
			check.NoErr(err)
			check.Equal(encryption, "AES256")
		}
	}
	cluster := objects["pg-destination"]
	environment, _, err := unstructured.NestedSlice(cluster, "spec", "env")
	check.NoErr(err)
	check.Equal(environment, []any{map[string]any{"name": "BORQ_BACKUP_ARCHIVE", "value": "destination-server"}})

	value, _, err = unstructured.NestedString(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID")
	check.NoErr(err)
	check.Equal(value, "20261024T000000")
	immediate, found, err := unstructured.NestedBool(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "targetImmediate")
	check.NoErr(err)
	check.True(!found && !immediate)
	external, _, err := unstructured.NestedSlice(cluster, "spec", "externalClusters")
	check.NoErr(err)
	sourcePlugin := external[0].(map[string]any)["plugin"].(map[string]any)
	check.Equal(sourcePlugin["parameters"].(map[string]any)["serverName"], "original-server")
	schedule := objects[backupScheduleName(input)]
	value, _, err = unstructured.NestedString(schedule, "spec", "method")
	check.NoErr(err)
	check.Equal(value, "plugin")
	value, _, err = unstructured.NestedString(schedule, "spec", "pluginConfiguration", "name")
	check.NoErr(err)
	check.Equal(value, "barman-cloud.cloudnative-pg.io")
	suspended, _, err := unstructured.NestedBool(schedule, "spec", "suspend")
	check.NoErr(err)
	check.True(suspended)
	immediate, _, err = unstructured.NestedBool(schedule, "spec", "immediate")
	check.NoErr(err)
	check.True(!immediate)

	value, _, err = unstructured.NestedString(objects["pg-destination-aws"], "data", "config")
	check.NoErr(err)
	check.True(strings.Contains(value, "addressing_style = path"))
	cron := objects["pg-destination-check"]
	value, _, err = unstructured.NestedString(cron, "spec", "concurrencyPolicy")
	check.NoErr(err)
	check.Equal(value, "Forbid")
	suspended, _, err = unstructured.NestedBool(cron, "spec", "suspend")
	check.NoErr(err)
	check.True(suspended)

	deadline, _, err := unstructured.NestedInt64(cron, "spec", "jobTemplate", "spec", "activeDeadlineSeconds")
	check.NoErr(err)
	check.Equal(deadline, int64(2460))
	for _, isolated := range []bool{false, true} {
		input.Isolated = isolated
		input.BackupEnabled = false
		input.Check.Enabled = false
		input.SnapshotClass = ""
		sawSource := false
		for _, manifest := range Manifests(input) {
			check.True(manifest["kind"] != "ScheduledBackup" && manifest["kind"] != "CronJob")
			metadata := manifest["metadata"].(map[string]any)
			check.True(metadata["name"] != "pg-destination-store")
			if metadata["name"] == "pg-destination-source" {
				sawSource = true
			}
			if manifest["kind"] == "Cluster" {
				plugins, _, _ := unstructured.NestedSlice(manifest, "spec", "plugins")
				check.Equal(len(plugins), 0)
				backup, exists, err := unstructured.NestedMap(manifest, "spec", "backup")
				check.NoErr(err)
				check.True(exists && backup != nil && len(backup) == 0)
				source, _, _ := unstructured.NestedSlice(manifest, "spec", "externalClusters")
				check.Equal(len(source), 1)
			}
		}
		check.True(sawSource)
	}

}

// TestBackupMetadata rejects incomplete physical archives and treats backup timestamps as database metadata.
func TestBackupMetadata(t *testing.T) {
	check := is.New(t)
	body := []byte("status=DONE\nversion=180006\nsize=1048576\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-10-24 12:00:00.123456+00:00\ntablespaces=None\n")
	metadata, err := ParseBackupMetadata(body)
	check.NoErr(err)
	check.True(metadata.Eligible)
	check.Equal(metadata.MajorVersion, 18)
	check.True(metadata.CompletedAt.Equal(time.Date(2026, 10, 24, 12, 0, 0, 123456000, time.UTC)))
	metadata, err = ParseBackupMetadata([]byte(strings.ReplaceAll(string(body), "size=1048576", "size=None")))
	check.NoErr(err)
	check.True(metadata.Eligible)
	check.Equal(metadata.SizeBytes, int64(0))
	for _, bad := range []string{string(body) + "status=DONE\n", strings.ReplaceAll(string(body), "end_wal=000000010000000000000002", "end_wal=../../other"), strings.ReplaceAll(string(body), "xlog_segment_size=16777216", "xlog_segment_size=0"), strings.ReplaceAll(string(body), "end_time=2026-10-24 12:00:00.123456+00:00", "end_time=not-a-timestamp")} {
		_, err := ParseBackupMetadata([]byte(bad))
		check.True(err != nil)
	}
	metadata, err = ParseBackupMetadata([]byte(strings.ReplaceAll(string(body), "status=DONE", "status=STARTED")))
	check.NoErr(err)
	check.True(!metadata.Eligible)
	metadata, err = ParseBackupMetadata([]byte(strings.ReplaceAll(string(body), "tablespaces=None", "tablespaces=[(123, 'unverified', '/external')]")))
	check.NoErr(err)
	check.True(!metadata.Eligible)
}

// TestWalContinuity requires every segment and handles PostgreSQL's log rollover using its actual segment size.
func TestWalContinuity(t *testing.T) {
	check := is.New(t)
	inventory := map[string]int64{"source/wals/0000000100000000/0000000100000000000000FF.gz": 100, "source/wals/0000000100000001/000000010000000100000000.gz": 100}
	check.True(walRangePresent(inventory, "source", "0000000100000000000000FF", "000000010000000100000000", 16777216))
	delete(inventory, "source/wals/0000000100000001/000000010000000100000000.gz")
	check.True(!walRangePresent(inventory, "source", "0000000100000000000000FF", "000000010000000100000000", 16777216))
	check.True(!walRangePresent(inventory, "source", "0000000100000000000000FF", "000000020000000100000000", 16777216))
}

// TestCapturedCatalogueTiedCompletions orders same-time backups by identity so exported manifests remain stable.
func TestCapturedCatalogueTiedCompletions(t *testing.T) {
	check := is.New(t)
	body := []byte("status=DONE\nversion=180006\nsize=1048576\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-10-24 12:00:00+00:00\ntablespaces=None\n")
	objects := []objectstore.Object{{Key: "server/wals/0000000100000000/000000010000000000000001.gz", Size: 10}}
	metadata := map[string][]byte{}
	for _, id := range []string{"20261023T120000", "20261024T120000"} {
		key := "server/base/" + id + "/backup.info"
		metadata[key] = body
		objects = append(objects, objectstore.Object{Key: key, Size: int64(len(body))}, objectstore.Object{Key: "server/base/" + id + "/data.tar.gz", Size: 10})
	}
	for range 32 {
		backups, err := EvaluateCatalogue(objects, metadata, "server")
		check.NoErr(err)
		check.Equal(len(backups), 2)
		check.Equal(backups[0].ID, "20261023T120000")
		check.Equal(backups[1].ID, "20261024T120000")
	}
}
