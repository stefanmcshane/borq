package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOffsiteSourceDispatch freezes all sources before one fresh vault checkpoint and attempts sources independently.
// The Restic protocol fixture proves dispatch and aggregate status; archive recovery is tested separately.
func TestOffsiteSourceDispatch(t *testing.T) {
	for _, failingSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-source-%t", failingSource), func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementStorageFixture(t)
			source := newOffsiteDispatchStorage(t)
			defer source.Close()
			input := componentManagerInput(t, fixture.kubeconfig)
			input.PlatformSettings.Namespace, input.PlatformSettings.Profile = "borq", "dev"
			manager, err := NewManager(t.Context(), input)
			assert.NoErr(err)
			defer manager.Close()
			storage, err := manager.prepareManagementS3(t.Context(), ManagementS3Input{Endpoint: source.URL, Region: "us-east-1", Bucket: "empty-management", PathStyle: true, AccessKey: "fixture", SecretKey: "fixture-secret"})
			assert.NoErr(err)
			write, err := documentWrite("management-s3", "shared", storage, 0)
			assert.NoErr(err)
			writes := []sqlite.Write{write}
			buckets := []string{"sqlite"}
			if failingSource {
				buckets = append([]string{"failed"}, buckets...)
			}
			for index, bucket := range buckets {
				retained := storage
				retained.Bucket = bucket
				write, err := documentWrite("space-storage", fmt.Sprintf("space-%d", index), spaceStorageRecord{Storage: retained, Status: SpaceBackupStorage{State: "ready"}}, 0)
				assert.NoErr(err)
				writes = append(writes, write)
			}
			policy, err := manager.prepareOffsite(t.Context(), ManagementOffsiteInput{Repository: "s3:" + fixture.s3.URL + "/offsite/archive", Region: "us-east-1", Password: "fixture-password", AccessKey: "fixture", SecretKey: "fixture-secret"}, storage)
			assert.NoErr(err)
			policy.Enabled = true
			write, err = documentWrite("management-offsite", "shared", policy, 0)
			assert.NoErr(err)
			writes = append(writes, write)
			assert.NoErr(manager.database.Commit(t.Context(), writes))
			directory := t.TempDir()
			receipt := filepath.Join(directory, "dispatch")
			configs := filepath.Join(directory, "configs")
			script := "#!/bin/sh\nshift 2\ncase \"$1\" in\n cat|forget) exit 0 ;;\n backup) printf '%s\\n' \"$@\" >> '" + strings.ReplaceAll(receipt, "'", "'\"'\"'") + "'; for last; do :; done; /bin/cat \"$last\" >> '" + configs + "'; printf '\\n' >> '" + configs + "'; printf '%s' '{\"snapshot_id\":\"fixture-snapshot\"}' ;;\n *) exit 1 ;;\nesac\n"
			assert.NoErr(os.WriteFile(filepath.Join(directory, "restic"), []byte(script), 0700))
			t.Setenv("PATH", directory)
			manager.runOffsiteDue(t.Context())
			attempted, err := os.ReadFile(receipt)
			assert.NoErr(err)
			storage.Bucket = "sqlite"
			assert.True(strings.Contains(string(attempted), "borq-source-"+offsite.SourceID(storageClientConfig(storage, objectstore.Credentials{}))))
			exported, err := os.ReadFile(configs)
			assert.NoErr(err)
			lines := strings.Split(strings.TrimSpace(string(exported)), "\n")
			assert.Equal(len(lines), 2)
			checkpointID := ""
			for _, line := range lines {
				var exported offsite.ExportInput
				assert.NoErr(json.Unmarshal([]byte(line), &exported))
				fixture.mutex.Lock()
				assert.True(!fixture.snapshotAt.IsZero() && fixture.snapshotAt.After(exported.Inventory.CapturedAt))
				fixture.mutex.Unlock()
				assert.True(!exported.Checkpoint.Artifact.CapturedAt.Before(exported.Inventory.CapturedAt))
				if checkpointID == "" {
					checkpointID = exported.Checkpoint.Artifact.ID
				}
				assert.Equal(exported.Checkpoint.Artifact.ID, checkpointID)
				// The primary root was empty when frozen; its required checkpoint is added only by the exporter.
				if exported.Storage.Bucket == "empty-management" {
					assert.Equal(len(exported.Inventory.Objects), 0)
				}
				_, err := os.Stat(exported.Checkpoint.File)
				assert.True(os.IsNotExist(err))
			}
			status, err := manager.GetManagementOffsiteStatus(t.Context())
			assert.NoErr(err)
			if failingSource {
				assert.Equal(status.State, "failed")
				assert.True(status.LastSuccess.IsZero())
			} else {
				assert.Equal(status.State, "succeeded")
			}
		})
	}
}

// TestOffsitePolicyPersistence proves real apply, vault-only secrets, omission, disable, close and reopen.
func TestOffsitePolicyPersistence(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:" + fixture.s3.URL + "/backups/borq", Region: "region-one", Password: "retained-private-password", AccessKey: "retained-private-access", SecretKey: "retained-private-secret"}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	configuration, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.True(validOffsiteRef(configuration.Offsite.CredentialsRef))
	reference := configuration.Offsite.CredentialsRef
	for _, collection := range []string{"management-task", "management-operation", "management-offsite", "management-offsite-status"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		for _, record := range records {
			assert.True(!strings.Contains(string(record.Value), "retained-private"))
		}
	}
	operation, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	configuration, err = manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(configuration.Offsite.CredentialsRef, reference)
	assert.Equal(configuration.Offsite.Interval, "6h")
	assert.Equal(configuration.Offsite.KeepLast, 14)
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "retained-private"))
	resolved, err := manager.resolveOffsite(t.Context(), offsitePolicy{Repository: configuration.Offsite.Repository, Region: configuration.Offsite.Region, CredentialsRef: reference})
	assert.NoErr(err)
	assert.Equal(resolved.Password, "retained-private-password")
	assert.Equal(resolved.AccessKey, "retained-private-access")
	assert.Equal(resolved.SecretKey, "retained-private-secret")
	operation, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	configuration, err = manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(configuration.Offsite.CredentialsRef, reference)
	assert.True(!configuration.Offsite.Enabled)
}

// TestOffsiteWorkerResumesInterruptedAttempt preserves the last successful source checkpoint on a real restarted worker failure.
func TestOffsiteWorkerResumesInterruptedAttempt(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	policy := ManagementOffsiteInput{Enabled: false, Repository: "s3:" + fixture.s3.URL + "/backups/borq", Region: "region-one", Password: "private-password", AccessKey: "private-access", SecretKey: "private-secret"}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Offsite: &policy})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	config, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	previous := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	status := ManagementOffsiteStatus{State: "running", LastSuccess: previous, LastAttempt: previous, NextAttempt: time.Now().Add(time.Hour), SnapshotID: strings.Repeat("1", 64), Source: strings.Repeat("2", 32), Repository: "s3:https://old.example/old/repo", CredentialsRef: "old-reference"}
	assert.NoErr(manager.saveOffsiteStatus(t.Context(), status))
	enabled := *config.Offsite
	enabled.Enabled = true
	assert.NoErr(manager.activateOffsite(t.Context(), enabled))
	assert.NoErr(manager.Close())
	assert.NoErr(os.Remove(fixture.kubeconfig))
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := manager.GetManagementOffsiteStatus(t.Context())
		assert.NoErr(err)
		if current.State == "failed" && current.LastAttempt.After(previous) {
			assert.Equal(current.SnapshotID, status.SnapshotID)
			assert.Equal(current.Source, status.Source)
			assert.Equal(current.Repository, status.Repository)
			assert.Equal(current.CredentialsRef, status.CredentialsRef)
			assert.True(current.LastSuccess.Equal(previous))
			assert.True(current.NextAttempt.After(time.Now()))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted offsite worker did not finish the due interrupted attempt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	current, err := manager.GetManagementOffsiteStatus(t.Context())
	assert.NoErr(err)
	assert.True(current.LastSuccess.Equal(previous))
}

// TestProductionBuiltInS3 keeps explicit locations authoritative and requires a routable advertised default.
func TestProductionBuiltInS3(t *testing.T) {
	assert := is.New(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PlatformSettings: platform.Settings{ClusterName: "ocean", Profile: "production", ApiUrl: "https://management.example", Namespace: "borq"}})
	assert.NoErr(err)
	defer manager.Close()
	storage, err := manager.GetManagementS3(t.Context())
	assert.NoErr(err)
	assert.Equal(storage.Endpoint, "http://management.example:30900")
	assert.Equal(storage.Bucket, "borq-ocean-data")
	assert.Equal(storage.CredentialsRef, "borq/platform/rustfs")
	assert.NoErr(manager.validateProductionStorage(storage))
	local := storage
	local.Endpoint = "http://rustfs-svc.borq.svc:9000"
	assert.NoErr(manager.validateManagementStorage(local))
	local.Endpoint = "http://rustfs-svc.borq.svc.cluster.local:9000"
	assert.NoErr(manager.validateManagementStorage(local))
	local.CredentialsRef = "borq/management/s3/" + strings.Repeat("a", 32)
	assert.True(errors.Is(manager.validateManagementStorage(local), ErrInvalid))
	for _, endpoint := range []string{"http://localhost:9000", "http://127.0.0.1:9000", "http://[::1]:9000", "http://rustfs-svc.borq.svc:9000"} {
		invalid := storage
		invalid.Endpoint = endpoint
		assert.True(manager.validateProductionStorage(invalid) != nil)
		if !strings.Contains(endpoint, "rustfs-svc.borq.svc") {
			assert.True(errors.Is(manager.validateManagementStorage(invalid), ErrInvalid))
		}
	}
	components, err := documentWrite("components", "management", componentRecord{Installation: ComponentInstallation{Result: platform.Result{Components: []platform.ComponentState{{Name: "loki", Status: "deployed"}}}}}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{components}))
	dependency, err := manager.managementStorageDependency(t.Context())
	assert.NoErr(err)
	assert.True(!dependency)
	settings, err := manager.GetPlatformSettings(t.Context())
	assert.NoErr(err)
	assert.Equal(settings.S3.CredentialsRef, "")
}

// TestOffsiteDisableDuringAttempt stops future starts while preserving the bound prior checkpoint after the active attempt fails.
func TestOffsiteDisableDuringAttempt(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	fixture := newManagementStorageFixture(t)
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:" + fixture.s3.URL + "/backups/repo", Region: "region", Password: "private-password", AccessKey: "private-access", SecretKey: "private-secret"}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	configuration, err := manager.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	fixture.mutex.Lock()
	fixture.vaultGate, fixture.vaultStarted = gate, started
	fixture.mutex.Unlock()
	previous := ManagementOffsiteStatus{State: "succeeded", LastSuccess: time.Now().Add(-time.Hour).UTC(), SnapshotID: strings.Repeat("1", 64), Source: strings.Repeat("2", 32), Repository: "s3:https://previous.example/prior/repo", CredentialsRef: "previous-reference"}
	assert.NoErr(manager.saveOffsiteStatus(ctx, previous))
	enabled := *configuration.Offsite
	enabled.Enabled = true
	assert.NoErr(manager.activateOffsite(ctx, enabled))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker did not accept the active attempt")
	}
	operation, err = manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	fixture.mutex.Lock()
	fixture.vaultDenied = true
	fixture.mutex.Unlock()
	close(gate)
	var finished ManagementOffsiteStatus
	for {
		finished, err = manager.GetManagementOffsiteStatus(ctx)
		assert.NoErr(err)
		if finished.State == "failed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("active attempt did not finish after disable")
		case <-time.After(10 * time.Millisecond):
		}
	}
	assert.Equal(finished.SnapshotID, previous.SnapshotID)
	assert.Equal(finished.Source, previous.Source)
	assert.Equal(finished.Repository, previous.Repository)
	assert.Equal(finished.CredentialsRef, previous.CredentialsRef)
	time.Sleep(1100 * time.Millisecond)
	current, err := manager.GetManagementOffsiteStatus(ctx)
	assert.NoErr(err)
	assert.True(current.LastAttempt.Equal(finished.LastAttempt))
	configuration, err = manager.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	assert.True(!configuration.Offsite.Enabled)
}

// TestOffsiteProspectiveStorage validates a combined apply against the S3 bucket activated by that same operation.
func TestOffsiteProspectiveStorage(t *testing.T) {
	for _, test := range []struct {
		name, repositoryBucket string
		valid                  bool
	}{{"old-source-is-safe", "backups", true}, {"new-source-overlaps", "new-source", false}} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementStorageFixture(t)
			manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PollInterval: time.Millisecond})
			assert.NoErr(err)
			defer manager.Close()
			initial, err := manager.PutManagementS3(t.Context(), ManagementS3Input{Endpoint: fixture.s3.URL, Region: "region", Bucket: "backups", PathStyle: true, AccessKey: "old-source-access", SecretKey: "old-source-secret"})
			assert.NoErr(err)
			waitManagementResult(t, manager, initial.ID, "succeeded")
			operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{S3: &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "region", Bucket: "new-source", PathStyle: true, AccessKey: "new-source-access", SecretKey: "new-source-secret"}, Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:" + fixture.s3.URL + "/" + test.repositoryBucket + "/repo", Region: "region", Password: "retained-password", AccessKey: "external-access", SecretKey: "external-secret"}})
			if !test.valid {
				assert.True(err != nil && strings.Contains(err.Error(), "separate external S3 bucket"))
				return
			}
			assert.NoErr(err)
			waitManagementResult(t, manager, operation.ID, "succeeded")
			configuration, err := manager.GetManagementConfiguration(t.Context())
			assert.NoErr(err)
			assert.Equal(configuration.S3.Bucket, "new-source")
			assert.Equal(configuration.Offsite.Repository, "s3:"+fixture.s3.URL+"/backups/repo")
		})
	}
}

// TestOffsiteTestBeforeSave keeps both saved connections active while an offsite
// test runs and when it fails, including a combined primary and offsite update.
func TestOffsiteTestBeforeSave(t *testing.T) {
	assert := is.New(t)
	source := newManagementStorageFixture(t)
	destination := newManagementStorageFixture(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: source.kubeconfig, PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	initial, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{
		S3:      &ManagementS3Input{Endpoint: source.s3.URL, Region: "region", Bucket: "source", Prefix: "saved", PathStyle: true, AccessKey: "source-access", SecretKey: "source-secret"},
		Offsite: &ManagementOffsiteInput{Repository: "s3:" + destination.s3.URL + "/offsite/saved", Region: "region", Password: "retained-password", AccessKey: "offsite-access", SecretKey: "offsite-secret"},
	})
	assert.NoErr(err)
	waitManagementResult(t, manager, initial.ID, "succeeded")
	saved, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	gate := make(chan struct{})
	destination.mutex.Lock()
	destination.listGate, destination.gateAt = gate, destination.listCount+1
	blockedAt := destination.gateAt
	destination.mutex.Unlock()
	primary, offsite := *saved.S3, *saved.Offsite
	primary.Prefix = "replacement"
	offsite.Repository = "s3:" + destination.s3.URL + "/offsite/replacement"
	requestContext, cancelRequest := context.WithTimeout(t.Context(), time.Second)
	operation, err := manager.ApplyManagementConfiguration(requestContext, ManagementConfiguration{S3: &primary, Offsite: &offsite})
	cancelRequest()
	assert.NoErr(err)
	deadline := time.Now().Add(5 * time.Second)
	for {
		destination.mutex.Lock()
		blocked := destination.listCount >= blockedAt
		destination.mutex.Unlock()
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offsite connection test did not contact the destination")
		}
		time.Sleep(time.Millisecond)
	}
	pending, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.Equal(pending.Phase, "test-offsite")
	active, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(active.S3, saved.S3)
	assert.Equal(active.Offsite, saved.Offsite)
	destination.mutex.Lock()
	destination.denied = true
	destination.mutex.Unlock()
	close(gate)
	waitManagementResult(t, manager, operation.ID, "failed")
	failed, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.True(strings.Contains(failed.Message, "Offsite S3 connection test failed"))
	active, err = manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(active.S3, saved.S3)
	assert.Equal(active.Offsite, saved.Offsite)
}

// newOffsiteDispatchStorage rejects checkpoint publication into any workload or native S3 catalogue.
func newOffsiteDispatchStorage(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("offsite capture unexpectedly wrote the source catalogue")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/failed" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/sqlite" && r.URL.Query().Get("prefix") == "sqlite/" {
			_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>sqlite/0123456789abcdef0123456789abcdef/web/generations/0123456789abcdef/snapshots/00000000.snapshot.lz4</Key><Size>1</Size><ETag>"fixture"</ETag></Contents></ListBucketResult>`))
			return
		}
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
}

// TestOffsiteRequiresRetainedKeys prevents a fresh checkpoint from replacing recoverable
// history after the vault has lost a deleted workload's retained archive identity.
func TestOffsiteRequiresRetainedKeys(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer manager.Close()
	cluster := newManagerCluster(t, managerClusterOptions{})
	target := createManagerEnvironment(t, manager, cluster, "offsite-keys")
	_, err := manager.sqliteArchiveKey(t.Context(), target, "retired-app")
	assert.NoErr(err)
	assert.NoErr(manager.checkOffsiteArchiveKeys(t.Context()))
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", "borq/sqlite-archives/"+target.ID+"/retired-app"))
	err = manager.checkOffsiteArchiveKeys(t.Context())
	assert.True(err != nil && strings.Contains(err.Error(), "restore the original vault entry"))
}
