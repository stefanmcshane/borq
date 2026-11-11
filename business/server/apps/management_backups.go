package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/client-go/tools/clientcmd"
)

// ManagementBackupInput declares the independent local native backup cadence.
type ManagementBackupInput struct {
	// Enabled starts local captures independently of the offsite cadence.
	Enabled bool `json:"enabled"`
	// Interval is a duration of at least one minute; the default is 1h.
	Interval string `json:"interval,omitempty"`
	// KeepLast retains this many completed artifacts per component.
	KeepLast int `json:"keepLast,omitempty"`
	// EncryptionKey reports the managed public archive key. Its identity is reconstructed from the exported OpenBao seal key.
	EncryptionKey string `json:"encryptionKey,omitempty"`
	// Status exposes safe independent results and is ignored during apply.
	Status *ManagementBackupStatus `json:"status,omitempty"`
}

// ManagementBackupStatus records one schedule and independent component results, without claiming atomic capture.
type ManagementBackupStatus struct {
	// LastAttempt and NextAttempt describe the schedule, not a shared snapshot timestamp.
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	NextAttempt time.Time `json:"nextAttempt,omitempty"`
	// Components preserves each component's last successful checkpoint independently.
	Components map[string]ManagementBackupComponent `json:"components"`
}

// ManagementBackupComponent reports one component attempt and its independent retained checkpoint.
type ManagementBackupComponent struct {
	// State is never, running, succeeded, failed, or retention-failed.
	State string `json:"state"`
	// LastAttempt is this component's actual capture start.
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	// LastSuccess is the last complete artifact's capture time.
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	// Artifact identifies the last successful encrypted native payload.
	Artifact managementbackup.Artifact `json:"artifact"`
	// Storage binds the retained artifact to its original safe source and immutable credential reference.
	Storage ManagementS3 `json:"storage"`
	// Error is a fixed safe failure message, never native service output or secret material.
	Error string `json:"error,omitempty"`
}
type backupSchedule struct {
	LastAttempt    time.Time `json:"lastAttempt"`
	NextAttempt    time.Time `json:"nextAttempt"`
	PolicyRevision int64     `json:"policyRevision"`
}

func prepareManagementBackups(input ManagementBackupInput) (ManagementBackupInput, error) {
	input.Status = nil
	if input.Interval == "" {
		input.Interval = "1h"
	}
	if input.KeepLast == 0 {
		input.KeepLast = 24
	}
	interval, err := time.ParseDuration(input.Interval)
	if err != nil || interval < time.Minute || interval > 365*24*time.Hour || input.KeepLast < 1 || input.KeepLast > 10000 {
		return ManagementBackupInput{}, fmt.Errorf("%w: local backup interval must be 1m to 8760h and keepLast 1 to 10000", ErrInvalid)
	}
	if input.EncryptionKey != "" {
		if err := managementbackup.ValidateRecipient(input.EncryptionKey); err != nil {
			return ManagementBackupInput{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	return input, nil
}
func (manager Manager) activateManagementBackups(ctx context.Context, input ManagementBackupInput) error {
	manager.runtime.backupMutex.Lock()
	defer manager.runtime.backupMutex.Unlock()
	_, record, err := loadDocument[ManagementBackupInput](ctx, manager.database, "management-backups", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	input.Status = nil
	if input.Enabled {
		input.EncryptionKey, err = platform.ManagementBackupRecipient(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return err
		}
	}
	write, err := documentWrite("management-backups", "shared", input, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}
func (manager Manager) managementBackupStatus(ctx context.Context) (ManagementBackupStatus, error) {
	schedule, _, err := loadDocument[backupSchedule](ctx, manager.database, "management-backup-schedule", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementBackupStatus{}, err
	}
	status := ManagementBackupStatus{LastAttempt: schedule.LastAttempt, NextAttempt: schedule.NextAttempt, Components: map[string]ManagementBackupComponent{}}
	for _, kind := range []string{"sqlite", "openbao"} {
		component, _, err := loadDocument[ManagementBackupComponent](ctx, manager.database, "management-backup-result", kind)
		if errors.Is(err, ErrNotFound) {
			component.State = "never"
		} else if err != nil {
			return ManagementBackupStatus{}, err
		}
		status.Components[kind] = component
	}
	return status, nil
}
func (manager Manager) startManagementBackupWorker() {
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		ticker := time.NewTicker(max(time.Second, manager.runtime.pollInterval))
		defer ticker.Stop()
		for {
			if manager.runtime.context.Err() != nil {
				return
			}
			manager.runManagementBackupsDue(manager.runtime.context)
			select {
			case <-manager.runtime.context.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (manager Manager) runManagementBackupsDue(ctx context.Context) {
	manager.runtime.backupMutex.Lock()
	policy, policyRecord, err := loadDocument[ManagementBackupInput](ctx, manager.database, "management-backups", "shared")
	if err != nil || !policy.Enabled {
		manager.runtime.backupMutex.Unlock()
		return
	}
	schedule, record, err := loadDocument[backupSchedule](ctx, manager.database, "management-backup-schedule", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		manager.runtime.backupMutex.Unlock()
		return
	}
	interrupted := false
	components := map[string]ManagementBackupComponent{}
	componentRecords := map[string]sqlite.Record{}
	for _, kind := range []string{"sqlite", "openbao"} {
		component, componentRecord, err := loadDocument[ManagementBackupComponent](ctx, manager.database, "management-backup-result", kind)
		if err != nil && !errors.Is(err, ErrNotFound) {
			manager.runtime.backupMutex.Unlock()
			return
		}
		components[kind], componentRecords[kind] = component, componentRecord
		if err == nil && component.State == "running" {
			interrupted = true
		}
	}
	now := time.Now().UTC()
	interval, err := time.ParseDuration(policy.Interval)
	if err != nil || (!interrupted && schedule.PolicyRevision == policyRecord.Revision && schedule.NextAttempt.After(now)) {
		manager.runtime.backupMutex.Unlock()
		return
	}
	schedule = backupSchedule{LastAttempt: now, NextAttempt: now.Add(interval), PolicyRevision: policyRecord.Revision}
	write, err := documentWrite("management-backup-schedule", "shared", schedule, record.Revision)
	writes := []sqlite.Write{write}
	for _, kind := range []string{"sqlite", "openbao"} {
		if err != nil {
			break
		}
		component := components[kind]
		component.State, component.LastAttempt, component.Error = "running", now, ""
		components[kind] = component
		write, err = documentWrite("management-backup-result", kind, component, componentRecords[kind].Revision)
		writes = append(writes, write)
	}
	if err == nil {
		err = manager.database.Commit(ctx, writes)
	}
	manager.runtime.backupMutex.Unlock()
	if err != nil {
		return
	}
	var captures sync.WaitGroup
	for _, kind := range []string{"sqlite", "openbao"} {
		captures.Add(1)
		go func() { defer captures.Done(); manager.captureManagementComponent(ctx, kind, policy, components[kind]) }()
	}
	captures.Wait()
	if ctx.Err() != nil {
		schedule.NextAttempt = time.Now().UTC()
		final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, record, err := loadDocument[backupSchedule](final, manager.database, "management-backup-schedule", "shared")
		if err == nil {
			write, err := documentWrite("management-backup-schedule", "shared", schedule, record.Revision)
			if err == nil {
				_ = manager.database.Commit(final, []sqlite.Write{write})
			}
		}
	}
}
func (manager Manager) captureManagementComponent(ctx context.Context, kind string, policy ManagementBackupInput, status ManagementBackupComponent) {
	status.LastAttempt = time.Now().UTC()
	attempt, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	artifact, storage, config, err := manager.captureNativeSnapshot(attempt, kind, status.LastAttempt)
	status.State = "succeeded"
	if err == nil {
		status.Artifact, status.LastSuccess = artifact, artifact.CapturedAt
		status.Storage = storage
		if managementbackup.Prune(attempt, config, kind, policy.KeepLast) != nil {
			status.State, status.Error = "retention-failed", "native snapshot completed; retention failed"
		}
	} else {
		status.State, status.Error = "failed", "native management snapshot failed; previous component checkpoint is retained"
	}
	final, cancelFinal := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelFinal()
	_, record, loadErr := loadDocument[ManagementBackupComponent](final, manager.database, "management-backup-result", kind)
	if loadErr == nil {
		write, err := documentWrite("management-backup-result", kind, status, record.Revision)
		if err == nil {
			_ = manager.database.Commit(final, []sqlite.Write{write})
		}
	}
}
func (manager Manager) captureNativeSnapshot(ctx context.Context, kind string, capturedAt time.Time) (managementbackup.Artifact, ManagementS3, objectstore.Config, error) {
	recipient, err := platform.ManagementBackupRecipient(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
	}
	directory, err := os.MkdirTemp("", "borq-native-capture-")
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, errors.New("create private native snapshot workspace")
	}
	defer os.RemoveAll(directory)
	file := filepath.Join(directory, "native")
	if kind == "sqlite" {
		snapshot := filepath.Join(directory, "snapshot.sqlite")
		if err := manager.database.Snapshot(ctx, snapshot); err != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
		}
		connections, err := manager.snapshotConnections(ctx, snapshot)
		if err != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
		}
		if err := managementbackup.BundleSQLite(ctx, snapshot, connections, file); err != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
		}
	} else {
		vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, errors.New("open native snapshot vault")
		}
		output, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, errors.New("create private Raft snapshot")
		}
		captureErr := vault.SaveRaftSnapshot(ctx, &nativeSnapshotWriter{Writer: output, remaining: managementbackup.MaxPlaintextSize})
		closeErr := output.Close()
		if captureErr != nil || closeErr != nil {
			return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, errors.New("capture complete native Raft snapshot")
		}
	}
	storage, err := manager.GetManagementS3(ctx)
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
	}
	config, err := manager.ResolveManagementS3(ctx, storage)
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
	}
	client, err := objectstore.New(config)
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
	}
	id, err := managementbackup.NewID()
	if err != nil {
		return managementbackup.Artifact{}, ManagementS3{}, objectstore.Config{}, err
	}
	artifact, err := managementbackup.Publish(ctx, client, managementbackup.Artifact{Version: 1, Kind: kind, ID: id, Recipient: recipient, CapturedAt: capturedAt}, file)
	return artifact, storage, config, err
}
func (manager Manager) snapshotConnections(ctx context.Context, snapshot string) ([]managementbackup.Connection, error) {
	database, err := sqlite.New(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	records, err := database.ListBounded(ctx, "cluster", managementbackup.MaxConnections, 32<<20)
	if err != nil {
		return nil, err
	}
	if len(records) >= managementbackup.MaxConnections {
		return nil, errors.New("too many saved recovery connections")
	}
	connections := []managementbackup.Connection{}
	metadataBytes := 2
	appendConnection := func(key, task string, encoded []byte) error {
		if len(connections) >= managementbackup.MaxConnections || len(encoded) > managementbackup.MaxConnectionSize || len(key) > 1024 || len(task) > 1024 {
			return errors.New("saved recovery connection exceeds native backup limits")
		}
		connection := managementbackup.Connection{Key: key, Task: task, Kubeconfig: string(encoded)}
		document, _ := json.Marshal(connection)
		metadataBytes += len(document) + 1
		if metadataBytes > managementbackup.MaxConnectionsSize {
			return errors.New("saved recovery connections exceed native backup limits")
		}
		connections = append(connections, connection)
		return nil
	}
	for _, record := range records {
		var cluster Cluster
		if json.Unmarshal(record.Value, &cluster) != nil {
			return nil, errors.New("invalid snapshot cluster record")
		}
		config, err := exportClusterConfiguration(cluster)
		if err != nil {
			return nil, errors.New("snapshot cluster connection is not self-contained; external credential plugins require relinking")
		}
		encoded, err := clientcmd.Write(config)
		if err != nil {
			return nil, errors.New("encode snapshot cluster connection")
		}
		if err := appendConnection(record.Key, "", encoded); err != nil {
			return nil, err
		}
	}
	tasks, err := database.ListBounded(ctx, "management-task", managementbackup.MaxConnections, 32<<20)
	if err != nil {
		return nil, err
	}
	operationRecords, err := database.ListBounded(ctx, "management-operation", 10000, 32<<20)
	if err != nil {
		return nil, err
	}
	operations := map[string]Operation{}
	for _, record := range operationRecords {
		var operation Operation
		if json.Unmarshal(record.Value, &operation) != nil {
			return nil, errors.New("invalid snapshot management operation")
		}
		operations[record.Key] = operation
	}
	for _, record := range tasks {
		operation, exists := operations[record.Key]
		if !exists {
			return nil, errors.New("snapshot management task has no operation")
		}
		if operationFinished(operation.Status) {
			continue
		}
		var task managementTask
		if json.Unmarshal(record.Value, &task) != nil {
			return nil, errors.New("invalid snapshot management task")
		}
		for _, organization := range task.Configuration.Organizations {
			for _, cluster := range organization.Clusters {
				config, err := exportClusterConfiguration(cluster)
				if err != nil {
					return nil, errors.New("staged management connection is not self-contained")
				}
				encoded, err := clientcmd.Write(config)
				if err != nil {
					return nil, errors.New("encode staged management connection")
				}
				if err := appendConnection(organization.Name+"/"+cluster.Name, record.Key, encoded); err != nil {
					return nil, err
				}
			}
		}
	}
	if manager.runtime.managementKubeconfigPath != "" {
		config, err := exportClusterConfiguration(Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath})
		if err != nil {
			return nil, errors.New("management recovery connection is not self-contained")
		}
		encoded, err := clientcmd.Write(config)
		if err != nil {
			return nil, errors.New("encode management recovery connection")
		}
		if err := appendConnection("@management", "", encoded); err != nil {
			return nil, err
		}
	}
	return connections, nil
}

type nativeSnapshotWriter struct {
	io.Writer
	remaining int64
}

func (writer *nativeSnapshotWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("native Raft snapshot exceeds the encrypted payload limit")
	}
	count, err := writer.Writer.Write(data)
	writer.remaining -= int64(count)
	return count, err
}
