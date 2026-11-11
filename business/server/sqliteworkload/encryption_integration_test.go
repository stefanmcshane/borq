//go:build integration

package sqliteworkload

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	_ "modernc.org/sqlite"
)

// TestEncryptedReplicationIntegration exercises the exact managed Litestream
// image against real S3 storage, then restores its encrypted snapshot and WAL.
// Wrong and absent keys must fail instead of accepting a restored database.
func TestEncryptedReplicationIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	archive, err := NewArchiveKey()
	is.NoErr(err)
	wrong, err := NewArchiveKey()
	is.NoErr(err)
	docker := func(arguments ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("Docker fixture failed: %v: %s", err, strings.NewReplacer(archive.Identity, "[redacted]", wrong.Identity, "[redacted]").Replace(string(output)))
		}
		return output
	}
	suffix := uuid.NewString()[:8]
	network, storageName, replicateName := "borq-age-"+suffix, "borq-age-storage-"+suffix, "borq-age-replicate-"+suffix
	docker("network", "create", network)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", replicateName, storageName).Run()
		_ = exec.CommandContext(cleanup, "docker", "network", "rm", network).Run()
	})
	docker("run", "--detach", "--rm", "--name", storageName, "--network", network, "-p", "127.0.0.1::9000", "--env", "RUSTFS_ACCESS_KEY=archive-test-access", "--env", "RUSTFS_SECRET_KEY=archive-test-secret-key", "--env", "RUSTFS_CONSOLE_ENABLE=false", "--env", "RUSTFS_OBS_LOG_DIRECTORY=", "rustfs/rustfs:1.0.1", "/data")
	endpoint := "http://" + strings.TrimSpace(string(docker("port", storageName, "9000/tcp")))
	external := objectstore.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "encrypted-sqlite", Prefix: "archive", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "archive-test-access", SecretKey: "archive-test-secret-key"}}
	store, err := objectstore.New(external)
	is.NoErr(err)
	for {
		if err := store.EnsureBucket(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("S3 fixture did not become ready")
		case <-time.After(200 * time.Millisecond):
		}
	}
	directory := t.TempDir()
	is.NoErr(os.Chmod(directory, 0755))
	databasePath := filepath.Join(directory, "app.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	is.NoErr(err)
	defer database.Close()
	database.SetMaxOpenConns(1)
	_, err = database.ExecContext(ctx, "PRAGMA journal_mode=WAL; CREATE TABLE proof (value TEXT); INSERT INTO proof VALUES ('encrypted-snapshot-proof')")
	is.NoErr(err)
	configuration := Config("/data/app.sqlite", Destination{Endpoint: "http://" + storageName + ":9000", Region: external.Region, Bucket: external.Bucket, Prefix: external.Prefix, PathStyle: true, SyncInterval: "100ms"})
	encoded, err := json.Marshal(configuration)
	is.NoErr(err)
	is.NoErr(os.WriteFile(filepath.Join(directory, "litestream.yml"), encoded, 0644))
	writeEnvironment := func(filename string, key ArchiveKey) {
		t.Helper()
		contents := "AWS_ACCESS_KEY_ID=archive-test-access\nAWS_SECRET_ACCESS_KEY=archive-test-secret-key\nAWS_EC2_METADATA_DISABLED=true\nLITESTREAM_AGE_IDENTITY=" + key.Identity + "\nLITESTREAM_AGE_RECIPIENT=" + key.Recipient + "\n"
		is.NoErr(os.WriteFile(filepath.Join(directory, filename), []byte(contents), 0600))
	}
	writeEnvironment("archive.env", archive)
	docker("run", "--detach", "--rm", "--name", replicateName, "--network", network, "--env-file", filepath.Join(directory, "archive.env"), "--mount", "type=bind,src="+directory+",dst=/data", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), Image, "replicate", "-config", "/data/litestream.yml")
	var objects []objectstore.Object
	for {
		objects, err = store.List(ctx, "")
		is.NoErr(err)
		snapshot := false
		for _, object := range objects {
			snapshot = snapshot || strings.Contains(object.Key, "/snapshots/")
		}
		if snapshot {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Litestream did not replicate an encrypted snapshot")
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Create writes after the first remote snapshot so restoration also needs WAL.
	_, err = database.ExecContext(ctx, "INSERT INTO proof VALUES ('encrypted-wal-proof')")
	is.NoErr(err)
	// Graceful SIGTERM flushes the final WAL before its process exits.
	time.Sleep(time.Second)
	docker("stop", "--time", "20", replicateName)
	objects, err = store.List(ctx, "")
	is.NoErr(err)
	ciphertextCount := 0
	for _, object := range objects {
		if !strings.Contains(object.Key, "/snapshots/") && !strings.Contains(object.Key, "/wal/") {
			continue
		}
		encrypted, err := store.Read(ctx, object.Key)
		is.NoErr(err)
		is.True(bytes.HasPrefix(encrypted, []byte("age-encryption.org/v1\n")))
		is.True(!bytes.Contains(encrypted, []byte("encrypted-snapshot-proof")))
		ciphertextCount++
	}
	is.True(ciphertextCount >= 2)
	writeEnvironment("wrong.env", ArchiveKey{Identity: wrong.Identity, Recipient: archive.Recipient})
	writeEnvironment("missing.env", ArchiveKey{})
	for _, test := range []struct {
		name, environment string
		success           bool
	}{{"correct", "archive.env", true}, {"wrong", "wrong.env", false}, {"missing", "missing.env", false}} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			outputPath := "/data/" + test.name + ".sqlite"
			command := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network, "--env-file", filepath.Join(directory, test.environment), "--mount", "type=bind,src="+directory+",dst=/data", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), Image, "restore", "-config", "/data/litestream.yml", "-o", outputPath, "/data/app.sqlite")
			output, restoreErr := command.CombinedOutput()
			if !test.success {
				if restoreErr == nil {
					t.Fatal("restore without the original archive identity succeeded")
				}
				if strings.Contains(string(output), archive.Identity) || strings.Contains(string(output), wrong.Identity) {
					t.Fatal("restore diagnostic exposed an archive identity")
				}
				return
			}
			if restoreErr != nil {
				t.Fatalf("restore encrypted snapshot and WAL: %v", restoreErr)
			}
			restored, err := sql.Open("sqlite", filepath.Join(directory, test.name+".sqlite"))
			is.NoErr(err)
			defer restored.Close()
			rows, err := restored.QueryContext(ctx, "SELECT value FROM proof ORDER BY rowid")
			is.NoErr(err)
			defer rows.Close()
			values := []string{}
			for rows.Next() {
				var value string
				is.NoErr(rows.Scan(&value))
				values = append(values, value)
			}
			is.NoErr(rows.Err())
			is.Equal(fmt.Sprint(values), "[encrypted-snapshot-proof encrypted-wal-proof]")
		})
	}
}
