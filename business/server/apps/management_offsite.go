package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type offsitePolicy struct {
	Enabled        bool   `json:"enabled"`
	Repository     string `json:"repository"`
	Region         string `json:"region"`
	Interval       string `json:"interval"`
	KeepLast       int    `json:"keepLast"`
	CredentialsRef string `json:"credentialsRef"`
}

func (policy offsitePolicy) input() ManagementOffsiteInput {
	return ManagementOffsiteInput{Enabled: policy.Enabled, Repository: policy.Repository, Region: policy.Region, Interval: policy.Interval, KeepLast: policy.KeepLast, CredentialsRef: policy.CredentialsRef}
}
func validOffsiteRef(reference string) bool {
	identity := strings.TrimPrefix(reference, "borq/management/offsite/")
	return strings.HasPrefix(reference, "borq/management/offsite/") && len(identity) == 32 && strings.Trim(identity, "0123456789abcdef") == ""
}
func (manager Manager) prepareOffsite(ctx context.Context, input ManagementOffsiteInput, storage ManagementS3) (offsitePolicy, error) {
	if input.Interval == "" {
		input.Interval = "6h"
	}
	if input.KeepLast == 0 {
		input.KeepLast = 14
	}
	interval, err := time.ParseDuration(input.Interval)
	if err != nil || interval < time.Minute || interval > 365*24*time.Hour || input.KeepLast < 1 || input.KeepLast > 10000 {
		return offsitePolicy{}, fmt.Errorf("%w: offsite interval must be 1m to 8760h and keepLast 1 to 10000", ErrInvalid)
	}
	policy := offsitePolicy{Enabled: input.Enabled, Repository: input.Repository, Region: input.Region, Interval: input.Interval, KeepLast: input.KeepLast, CredentialsRef: input.CredentialsRef}
	inline := input.Password != "" || input.AccessKey != "" || input.SecretKey != "" || input.SessionToken != ""
	if inline && input.CredentialsRef != "" {
		return offsitePolicy{}, fmt.Errorf("%w: supply offsite credentials or credentialsRef, not both", ErrInvalid)
	}
	if !inline && !validOffsiteRef(input.CredentialsRef) {
		if !input.Enabled && input.Repository == "" && input.CredentialsRef == "" {
			return policy, nil
		}
		return offsitePolicy{}, fmt.Errorf("%w: offsite credentialsRef or independently retained credentials are required", ErrInvalid)
	}
	repository := offsite.Repository{URL: input.Repository, Region: input.Region, Password: input.Password, AccessKey: input.AccessKey, SecretKey: input.SecretKey, SessionToken: input.SessionToken}
	if inline {
		if err := repository.Validate(); err != nil {
			return offsitePolicy{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if err := offsite.ValidateSeparation(repository, storageClientConfig(storage, objectstore.Credentials{})); err != nil {
		return offsitePolicy{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if inline {
		id, err := newId()
		if err != nil {
			return offsitePolicy{}, err
		}
		policy.CredentialsRef = "borq/management/offsite/" + id
		vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return offsitePolicy{}, err
		}
		if _, err := vault.WriteKv(ctx, "secret", policy.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"password": input.Password, "accessKey": input.AccessKey, "secretKey": input.SecretKey, "sessionToken": input.SessionToken}}); err != nil {
			return offsitePolicy{}, errors.New("stage offsite recovery credentials in management vault")
		}
	}
	return policy, nil
}

func (manager Manager) testOffsite(ctx context.Context, input ManagementOffsiteInput) error {
	if input.Repository == "" && !input.Enabled {
		return nil
	}
	repository, err := manager.resolveOffsite(ctx, offsitePolicy{Repository: input.Repository, Region: input.Region, CredentialsRef: input.CredentialsRef})
	if err != nil {
		return fmt.Errorf("%w: Offsite S3 connection test could not read the stored credentials. Saved settings are unchanged.", ErrInvalid)
	}
	if err := offsite.TestConnection(ctx, repository); err != nil {
		return fmt.Errorf("%w: Offsite S3 connection test failed. Check the destination, credentials, and list, write, read, and delete permissions. Saved settings are unchanged.", ErrInvalid)
	}
	return nil
}
func (manager Manager) resolveOffsite(ctx context.Context, policy offsitePolicy) (offsite.Repository, error) {
	if !validOffsiteRef(policy.CredentialsRef) {
		return offsite.Repository{}, errors.New("invalid offsite credential reference")
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return offsite.Repository{}, errors.New("open offsite credential vault")
	}
	entry, err := vault.ReadKv(ctx, "secret", policy.CredentialsRef, 0)
	if err != nil {
		return offsite.Repository{}, errors.New("read offsite recovery credentials")
	}
	return offsite.Repository{URL: policy.Repository, Region: policy.Region, Password: entry.Values["password"], AccessKey: entry.Values["accessKey"], SecretKey: entry.Values["secretKey"], SessionToken: entry.Values["sessionToken"]}, nil
}
func (manager Manager) activateOffsite(ctx context.Context, input ManagementOffsiteInput) error {
	manager.runtime.offsiteMutex.Lock()
	defer manager.runtime.offsiteMutex.Unlock()
	previous, record, err := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	policy := offsitePolicy{Enabled: input.Enabled, Repository: input.Repository, Region: input.Region, Interval: input.Interval, KeepLast: input.KeepLast, CredentialsRef: input.CredentialsRef}
	if !input.Enabled && input.Repository == "" && input.CredentialsRef == "" && err == nil {
		policy = previous
		policy.Enabled = false
	}
	write, err := documentWrite("management-offsite", "shared", policy, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

// GetManagementOffsiteStatus returns safe durable worker progress without resolving credentials.
func (manager Manager) GetManagementOffsiteStatus(ctx context.Context) (ManagementOffsiteStatus, error) {
	status, _, err := loadDocument[ManagementOffsiteStatus](ctx, manager.database, "management-offsite-status", "shared")
	if errors.Is(err, ErrNotFound) {
		return ManagementOffsiteStatus{State: "never"}, nil
	}
	return status, err
}
func (manager Manager) saveOffsiteStatus(ctx context.Context, status ManagementOffsiteStatus) error {
	_, record, err := loadDocument[ManagementOffsiteStatus](ctx, manager.database, "management-offsite-status", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	write, err := documentWrite("management-offsite-status", "shared", status, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}
func (manager Manager) startOffsiteWorker() {
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		ticker := time.NewTicker(max(manager.runtime.pollInterval, time.Second))
		defer ticker.Stop()
		for {
			if manager.runtime.context.Err() != nil {
				return
			}
			manager.runOffsiteDue(manager.runtime.context)
			select {
			case <-manager.runtime.context.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (manager Manager) runOffsiteDue(ctx context.Context) {
	manager.runtime.offsiteMutex.Lock()
	accepted := false
	defer func() {
		if !accepted {
			manager.runtime.offsiteMutex.Unlock()
		}
	}()
	policy, record, err := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
	if err != nil || !policy.Enabled {
		return
	}
	status, err := manager.GetManagementOffsiteStatus(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if status.PolicyRevision == record.Revision && status.State != "running" && status.NextAttempt.After(now) {
		return
	}
	interval, err := time.ParseDuration(policy.Interval)
	if err != nil || interval < time.Minute {
		return
	}
	status.State, status.LastAttempt, status.NextAttempt, status.Error = "running", now, now.Add(interval), ""
	status.PolicyRevision = record.Revision
	if manager.saveOffsiteStatus(ctx, status) != nil {
		return
	}
	accepted = true
	manager.runtime.offsiteMutex.Unlock()
	attempt, cancelAttempt := context.WithTimeout(ctx, 24*time.Hour)
	defer cancelAttempt()
	ctx = attempt
	repository, err := manager.resolveOffsite(ctx, policy)
	snapshot := ""
	sourceID := ""
	retentionFailed := false
	if err == nil {
		result, captureErr := manager.captureOffsiteArchives(ctx, repository, policy)
		snapshot, sourceID, retentionFailed, err = result.snapshot, result.source, result.retentionFailed, captureErr
	}

	status.State = "succeeded"
	if snapshot != "" && err == nil {
		status.SnapshotID, status.LastSuccess = snapshot, time.Now().UTC()
		status.Source, status.Repository, status.CredentialsRef = sourceID, policy.Repository, policy.CredentialsRef
	}
	if err != nil {
		status.State, status.Error = "failed", "offsite snapshot failed; previous successful checkpoint is retained"
		if retentionFailed {
			status.State, status.Error = "retention-failed", "one or more sources failed, including repository retention; previous checkpoint is retained"
		}
	}
	if ctx.Err() != nil {
		status.State, status.NextAttempt, status.Error = "failed", time.Now().UTC(), "offsite attempt interrupted"
	}
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = manager.saveOffsiteStatus(final, status)
}

type offsiteCaptureResult struct {
	snapshot        string
	source          string
	retentionFailed bool
}

func (manager Manager) captureOffsiteArchives(ctx context.Context, repository offsite.Repository, policy offsitePolicy) (offsiteCaptureResult, error) {
	result := offsiteCaptureResult{}
	storage, err := manager.GetManagementS3(ctx)
	if err != nil {
		return result, err
	}
	sources, err := manager.sqliteBackupSources(ctx)
	if err != nil {
		return result, err
	}
	sources = append([]ManagementS3{storage}, sources...)
	type frozenSource struct {
		config    objectstore.Config
		inventory offsite.FrozenInventory
	}
	frozen := []frozenSource{}
	var failures error
	for index, storage := range sources {
		config, err := manager.ResolveManagementS3(ctx, storage)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if err := offsite.ValidateSeparation(repository, config); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		inventory, err := offsite.Freeze(ctx, config)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		// The shared root always retains the fresh key checkpoint, even before any database backups exist.
		if index == 0 || len(inventory.Objects) > 0 {
			frozen = append(frozen, frozenSource{config: config, inventory: inventory})
		}
	}
	if len(frozen) == 0 {
		return result, errors.Join(failures, errors.New("no managed archive sources were available"))
	}
	// Keys are persisted before workload backup writes. Taking this checkpoint after every inventory
	// excludes later workloads without holding a global lock throughout their potentially long export.
	if err := manager.checkOffsiteArchiveKeys(ctx); err != nil {
		return result, errors.Join(failures, err)
	}
	directory, err := os.MkdirTemp("", "borq-offsite-checkpoint-")
	if err != nil {
		return result, errors.Join(failures, errors.New("create private offsite checkpoint workspace"))
	}
	defer os.RemoveAll(directory)
	checkpoint, err := manager.captureOffsiteVaultCheckpoint(ctx, directory)
	if err != nil {
		return result, errors.Join(failures, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return result, errors.Join(failures, err)
	}
	for _, source := range frozen {
		identity := offsite.SourceID(source.config)
		captured, err := offsite.Run(ctx, offsite.RunInput{Repository: repository, Source: source.config, Inventory: source.inventory, Checkpoint: checkpoint, Executable: executable, CacheDir: filepath.Join(filepath.Dir(manager.runtime.credentialsDir), "offsite-cache", identity), KeepLast: policy.KeepLast})
		if captured != "" {
			result.snapshot, result.source = captured, identity
		}
		result.retentionFailed = result.retentionFailed || captured != "" && err != nil
		failures = errors.Join(failures, err)
	}
	if result.snapshot == "" && failures == nil {
		failures = errors.New("no complete managed backups were available")
	}
	return result, failures
}

func (manager Manager) checkOffsiteArchiveKeys(ctx context.Context) error {
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return errors.New("offsite archive key storage is unavailable")
	}
	if err := platform.CheckRetainedRustfsEncryptionRoot(ctx, vault); err != nil {
		return err
	}
	records, err := manager.database.List(ctx, "sqlite-archive-key")
	if err != nil {
		return err
	}
	// Markers survive workload deletion; current desired applications alone cannot establish recovery coverage.
	for _, record := range records {
		var reference string
		if json.Unmarshal(record.Value, &reference) != nil || reference != "borq/sqlite-archives/"+record.Key {
			return errors.New("offsite SQLite key ownership is invalid")
		}
		entry, err := vault.ReadKv(ctx, "secret", reference, 1)
		if err != nil {
			return errors.New("offsite SQLite key is missing; restore the original vault entry before replacing recovery checkpoints")
		}
		if err := (sqliteworkload.ArchiveKey{Identity: entry.Values["ageIdentity"], Recipient: entry.Values["ageRecipient"]}).Validate(); err != nil {
			return err
		}
	}
	records, err = manager.database.List(ctx, "space-pocketid")
	if err != nil {
		return err
	}
	for _, record := range records {
		var stored spacePocketIdRecord
		if json.Unmarshal(record.Value, &stored) != nil {
			return errors.New("offsite Pocket ID ownership is invalid")
		}
		if _, err := manager.spacePocketIdCredentials(ctx, stored); err != nil {
			return err
		}
	}
	records, err = manager.database.List(ctx, "organization-pocketid")
	if err != nil {
		return err
	}
	for _, record := range records {
		var stored organizationPocketIdRecord
		if json.Unmarshal(record.Value, &stored) != nil {
			return errors.New("offsite Pocket ID ownership is invalid")
		}
		if !stored.CredentialsStored {
			continue
		}
		entry, err := vault.ReadKv(ctx, "secret", stored.credentialsPath(), 1)
		if err != nil || len(entry.Values["encryptionKey"]) < 32 {
			return errors.New("offsite Pocket ID keys are missing; restore the original vault entry before replacing recovery checkpoints")
		}
		if err := (sqliteworkload.ArchiveKey{Identity: entry.Values["ageIdentity"], Recipient: entry.Values["ageRecipient"]}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (manager Manager) captureOffsiteVaultCheckpoint(ctx context.Context, directory string) (offsite.VaultCheckpoint, error) {
	recipient, err := platform.ManagementBackupRecipient(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return offsite.VaultCheckpoint{}, err
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return offsite.VaultCheckpoint{}, errors.New("open offsite checkpoint vault")
	}
	plaintext := filepath.Join(directory, "vault.raft")
	output, err := os.OpenFile(plaintext, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return offsite.VaultCheckpoint{}, errors.New("create private offsite vault checkpoint")
	}
	capturedAt := time.Now().UTC()
	captureErr := vault.SaveRaftSnapshot(ctx, &nativeSnapshotWriter{Writer: output, remaining: managementbackup.MaxPlaintextSize})
	closeErr := output.Close()
	if captureErr != nil || closeErr != nil {
		return offsite.VaultCheckpoint{}, errors.New("capture complete offsite vault checkpoint")
	}
	id, err := managementbackup.NewID()
	if err != nil {
		return offsite.VaultCheckpoint{}, err
	}
	file := filepath.Join(directory, "vault.age")
	// A local-only checkpoint cannot participate in native S3 pruning or replace its independent status.
	artifact, err := managementbackup.Encrypt(ctx, managementbackup.Artifact{Version: 1, Kind: "openbao", ID: id, Recipient: recipient, CapturedAt: capturedAt}, plaintext, file)
	if err != nil {
		return offsite.VaultCheckpoint{}, err
	}
	if err := os.Remove(plaintext); err != nil {
		return offsite.VaultCheckpoint{}, errors.New("remove private offsite capture workspace")
	}
	return offsite.VaultCheckpoint{Artifact: artifact, File: file}, nil
}
