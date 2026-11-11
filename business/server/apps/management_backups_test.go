package apps

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"golang.org/x/crypto/bcrypt"
)

// TestManagementBackupRequestRetry keeps accepted requests retryable without
// treating a changed encryption key as identical.
func TestManagementBackupRequestRetry(t *testing.T) {
	assert := is.New(t)
	const previous = `{"borqPublicUrl":"https://borq.test","backups":{"enabled":true,"interval":"2h","keepLast":5,"encryptionKey":"age1public"},"offsite":{"enabled":false}}`
	var configuration ManagementConfiguration
	assert.NoErr(json.Unmarshal([]byte(previous), &configuration))
	digest := sha256.Sum256([]byte(previous))
	hash, err := bcrypt.GenerateFromPassword(digest[:], bcrypt.MinCost)
	assert.NoErr(err)
	task := managementTask{RequestHash: string(hash)}
	current, err := managementRequestDigest(configuration)
	assert.NoErr(err)
	matched, err := task.matchesRequest(current)
	assert.NoErr(err)
	assert.True(matched)
	configuration.Backups.EncryptionKey = "age1changed"
	changed, err := managementRequestDigest(configuration)
	assert.NoErr(err)
	matched, err = task.matchesRequest(changed)
	assert.NoErr(err)
	assert.True(!matched)
}

// TestManagementBackupPolicyPersistence owns apply/export omission, disable semantics and safe private-key exclusion across reopen.
func TestManagementBackupPolicyPersistence(t *testing.T) {
	assert := is.New(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Backups: &ManagementBackupInput{Enabled: false, EncryptionKey: identity.Recipient().String(), Interval: "2h", KeepLast: 5}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	operation, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	configuration, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(configuration.Backups.EncryptionKey, identity.Recipient().String())
	assert.Equal(configuration.Backups.Interval, "2h")
	operation, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Backups: &ManagementBackupInput{Enabled: false}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	configuration, err = manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(configuration.Backups.EncryptionKey, identity.Recipient().String())
	assert.Equal(configuration.Backups.KeepLast, 5)
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), identity.String()))
	_, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Backups: &ManagementBackupInput{Enabled: true, EncryptionKey: identity.String()}})
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), identity.String()))
}

// TestManagementBackupInterruptedReopen resumes a genuinely persisted running attempt without waiting for a future interval or losing prior checkpoints.
func TestManagementBackupInterruptedReopen(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	file := filepath.Join(directory, "borq.sqlite")
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	database, err := sqlite.New(t.Context(), file)
	assert.NoErr(err)
	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
	}{
		{"management-backups", "shared", ManagementBackupInput{Enabled: true, EncryptionKey: identity.Recipient().String(), Interval: "1h", KeepLast: 2}},
		{"management-backup-schedule", "shared", backupSchedule{LastAttempt: old, NextAttempt: time.Now().Add(time.Hour), PolicyRevision: 1}},
		{"management-backup-result", "sqlite", ManagementBackupComponent{State: "running", LastAttempt: old, LastSuccess: old}},
		{"management-backup-result", "openbao", ManagementBackupComponent{State: "running", LastAttempt: old, LastSuccess: old}},
	} {
		data, _ := json.Marshal(item.value)
		writes = append(writes, sqlite.Write{Collection: item.collection, Key: item.key, Value: data})
	}
	assert.NoErr(database.Commit(t.Context(), writes))
	assert.NoErr(database.Close())
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: file, ManagementKubeconfigPath: filepath.Join(directory, "missing-management-connection"), PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := manager.managementBackupStatus(t.Context())
		assert.NoErr(err)
		if status.Components["sqlite"].State == "failed" && status.Components["openbao"].State == "failed" {
			for _, component := range status.Components {
				assert.True(component.LastAttempt.After(old))
				assert.True(component.LastSuccess.Equal(old))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted local backup did not resume after database reopen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRestoredManagerReplayBarrier stops startup before any queued resource operation can run.
func TestRestoredManagerReplayBarrier(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	assert.NoErr(os.WriteFile(filepath.Join(directory, "RECOVERY_REQUIRED"), []byte("review required"), 0600))
	_, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(directory, "borq.sqlite")})
	assert.True(err != nil && strings.Contains(err.Error(), "offline review"))
	_, err = os.Stat(filepath.Join(directory, "borq.sqlite"))
	assert.True(os.IsNotExist(err))
}

func TestQueuedManagementConnectionRecovery(t *testing.T) {
	assert := is.New(t)
	original := t.TempDir()
	database, err := sqlite.New(t.Context(), filepath.Join(original, "borq.sqlite"))
	assert.NoErr(err)
	manager := Manager{database: database, runtime: &managerRuntime{credentialsDir: filepath.Join(original, "clusters")}}
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: remote\n  cluster:\n    server: https://cluster.example\ncontexts:\n- name: remote\n  context:\n    cluster: remote\n    user: remote\ncurrent-context: remote\nusers:\n- name: remote\n  user:\n    token: queued-connection-token\n"
	configuration, err := manager.stageManagementConfiguration(t.Context(), ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "org", Clusters: []ClusterInput{{Name: "remote", Kubeconfig: kubeconfig}}}}})
	assert.NoErr(err)
	taskWrite, err := documentWrite("management-task", "queued", managementTask{Configuration: configuration}, 0)
	assert.NoErr(err)
	operationWrite, err := documentWrite("management-operation", "queued", Operation{ID: "queued", Status: "queued"}, 0)
	assert.NoErr(err)
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{taskWrite, operationWrite}))
	directory := t.TempDir()
	snapshot := filepath.Join(directory, "snapshot.sqlite")
	assert.NoErr(database.Snapshot(t.Context(), snapshot))
	connections, err := manager.snapshotConnections(t.Context(), snapshot)
	assert.NoErr(err)
	bundle := filepath.Join(directory, "bundle.tar")
	assert.NoErr(managementbackup.BundleSQLite(t.Context(), snapshot, connections, bundle))
	assert.NoErr(database.Close())
	assert.NoErr(os.RemoveAll(original))
	destination := filepath.Join(directory, "recovered")
	assert.NoErr(managementbackup.RestoreSQLite(t.Context(), bundle, destination))
	recovered, err := sqlite.New(t.Context(), filepath.Join(destination, "borq.sqlite"))
	assert.NoErr(err)
	task, _, err := loadDocument[managementTask](t.Context(), recovered, "management-task", "queued")
	assert.NoErr(err)
	assert.NoErr(recovered.Close())
	connection := task.Configuration.Organizations[0].Clusters[0]
	if !strings.HasPrefix(connection.KubeconfigPath, destination+"/clusters/") {
		t.Fatalf("queued task still references lost source connection %q", connection.KubeconfigPath)
	}
	bytes, err := os.ReadFile(connection.KubeconfigPath)
	assert.NoErr(err)
	assert.True(strings.Contains(string(bytes), "queued-connection-token"))
}
