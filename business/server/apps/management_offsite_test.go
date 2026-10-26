package apps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOffsitePolicyPersistence proves real apply, vault-only secrets, omission, disable, close and reopen.
func TestOffsitePolicyPersistence(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:https://offsite.example/backups/borq", Region: "region-one", Password: "retained-private-password", AccessKey: "retained-private-access", SecretKey: "retained-private-secret"}})
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
	policy := ManagementOffsiteInput{Enabled: false, Repository: "s3:https://offsite.example/backups/borq", Region: "region-one", Password: "private-password", AccessKey: "private-access", SecretKey: "private-secret"}
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
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PlatformSettings: platform.Settings{Profile: "production", ApiUrl: "https://management.example", Namespace: "borq"}})
	assert.NoErr(err)
	defer manager.Close()
	storage, err := manager.GetManagementS3(t.Context())
	assert.NoErr(err)
	assert.Equal(storage.Endpoint, "http://management.example:30900")
	assert.Equal(storage.CredentialsRef, "borq/platform/rustfs")
	assert.NoErr(manager.validateProductionStorage(storage))
	for _, endpoint := range []string{"http://localhost:9000", "http://127.0.0.1:9000", "http://[::1]:9000", "http://rustfs-svc.borq.svc:9000"} {
		invalid := storage
		invalid.Endpoint = endpoint
		assert.True(manager.validateProductionStorage(invalid) != nil)
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
	operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:https://offsite.example/backups/repo", Region: "region", Password: "private-password", AccessKey: "private-access", SecretKey: "private-secret"}})
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
			_, err = manager.PutManagementS3(t.Context(), ManagementS3Input{Endpoint: fixture.s3.URL, Region: "region", Bucket: "backups", PathStyle: true, AccessKey: "old-source-access", SecretKey: "old-source-secret"})
			assert.NoErr(err)
			operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{S3: &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "region", Bucket: "new-source", PathStyle: true, AccessKey: "new-source-access", SecretKey: "new-source-secret"}, Offsite: &ManagementOffsiteInput{Enabled: false, Repository: "s3:https://external.example/" + test.repositoryBucket + "/repo", Region: "region", Password: "retained-password", AccessKey: "external-access", SecretKey: "external-secret"}})
			if !test.valid {
				assert.True(err != nil && strings.Contains(err.Error(), "separate external S3 bucket"))
				return
			}
			assert.NoErr(err)
			waitManagementResult(t, manager, operation.ID, "succeeded")
			configuration, err := manager.GetManagementConfiguration(t.Context())
			assert.NoErr(err)
			assert.Equal(configuration.S3.Bucket, "new-source")
			assert.Equal(configuration.Offsite.Repository, "s3:https://external.example/backups/repo")
		})
	}
}
