package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
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
			previous, _, err := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
			if err == nil {
				previous.Enabled = false
				return previous, nil
			}
			if errors.Is(err, ErrNotFound) {
				return policy, nil
			}
			return offsitePolicy{}, err
		}
		return offsitePolicy{}, fmt.Errorf("%w: offsite credentialsRef or independently retained credentials are required", ErrInvalid)
	}
	repository := offsite.Repository{URL: input.Repository, Region: input.Region, Password: input.Password, AccessKey: input.AccessKey, SecretKey: input.SecretKey, SessionToken: input.SessionToken}
	if !inline {
		repository, err = manager.resolveOffsite(ctx, policy)
		if err != nil {
			return offsitePolicy{}, err
		}
	}
	if err := repository.Validate(); err != nil {
		return offsitePolicy{}, fmt.Errorf("%w: %v", ErrInvalid, err)
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
	_, record, err := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	policy := offsitePolicy{Enabled: input.Enabled, Repository: input.Repository, Region: input.Region, Interval: input.Interval, KeepLast: input.KeepLast, CredentialsRef: input.CredentialsRef}
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
	if err == nil {
		storage, storageErr := manager.GetManagementS3(ctx)
		err = storageErr
		if err == nil {
			source, sourceErr := manager.ResolveManagementS3(ctx, storage)
			err = sourceErr
			if err == nil {
				sourceID = offsite.SourceID(source)
				executable, executableErr := os.Executable()
				err = executableErr
				if err == nil {
					snapshot, err = offsite.Run(ctx, offsite.RunInput{Repository: repository, Source: source, Executable: executable, CacheDir: filepath.Join(filepath.Dir(manager.runtime.credentialsDir), "offsite-cache", sourceID), KeepLast: policy.KeepLast})
				}
			}
		}
	}
	status.State = "succeeded"
	if snapshot != "" {
		status.SnapshotID, status.LastSuccess = snapshot, time.Now().UTC()
		status.Source, status.Repository, status.CredentialsRef = sourceID, policy.Repository, policy.CredentialsRef
	}
	if err != nil {
		status.State, status.Error = "failed", "offsite snapshot failed; previous successful checkpoint is retained"
		if snapshot != "" {
			status.State, status.Error = "retention-failed", "snapshot completed; repository retention failed"
		}
	}
	if ctx.Err() != nil {
		status.State, status.NextAttempt, status.Error = "failed", time.Now().UTC(), "offsite attempt interrupted"
	}
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = manager.saveOffsiteStatus(final, status)
}
