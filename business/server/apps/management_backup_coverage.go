package apps

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
)

// ManagementBackupCoverage reports archive evidence for every currently configured database.
// Available means archive metadata exists; it does not replace a successful restore test.
type ManagementBackupCoverage struct {
	// CheckedAt is when this live, read-only inspection started.
	CheckedAt time.Time `json:"checkedAt"`
	// State is available only when every expected backup has current evidence.
	State string `json:"state"`
	// Components includes disabled and unconfigured backups, so omissions remain visible.
	Components []BackupCoverageComponent `json:"components"`
}

// BackupCoverageComponent separates backup creation from offsite export progress.
type BackupCoverageComponent struct {
	// Kind identifies management, sqlite, pocketid, postgres, or offsite.
	Kind string `json:"kind"`
	// Name is a safe human-readable workload path, never a storage URL or credential.
	Name string `json:"name"`
	// State is available, disabled, missing, stale, failed, or unknown.
	State string `json:"state"`
	// LastSuccess is the latest observed complete backup or committed export.
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	// StaleAfter explains the freshness deadline used for this component.
	StaleAfter time.Time `json:"staleAfter,omitzero"`
	// Message explains what was observed and how to address missing coverage.
	Message string `json:"message"`
}

// GetManagementBackupCoverage inspects expected archives without generating keys,
// provisioning buckets, starting captures, or changing accepted configuration.
func (manager Manager) GetManagementBackupCoverage(ctx context.Context) (ManagementBackupCoverage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	report := ManagementBackupCoverage{CheckedAt: time.Now().UTC(), State: "available", Components: []BackupCoverageComponent{}}
	native, err := manager.nativeBackupCoverage(ctx, report.CheckedAt)
	if err != nil {
		return report, err
	}
	report.Components = append(report.Components, native...)
	targets, err := manager.workloadBackupTargets(ctx)
	if err != nil {
		return report, err
	}
	for _, target := range targets {
		report.Components = append(report.Components, manager.inspectBackupTarget(ctx, target, report.CheckedAt))
	}
	export, err := manager.offsiteBackupCoverage(ctx, report.CheckedAt)
	if err != nil {
		return report, err
	}
	report.Components = append(report.Components, export)
	sort.Slice(report.Components, func(i, j int) bool {
		left, right := report.Components[i], report.Components[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Name < right.Name
	})
	for _, component := range report.Components {
		if component.State != "available" {
			report.State = "attention"
		}
	}
	return report, nil
}

func (manager Manager) nativeBackupCoverage(ctx context.Context, now time.Time) ([]BackupCoverageComponent, error) {
	policy, _, err := loadDocument[ManagementBackupInput](ctx, manager.database, "management-backups", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	status, err := manager.managementBackupStatus(ctx)
	if err != nil {
		return nil, err
	}
	interval, _ := time.ParseDuration(policy.Interval)
	if interval == 0 {
		interval = time.Hour
	}
	result := []BackupCoverageComponent{}
	for _, kind := range []string{"sqlite", "openbao"} {
		component := BackupCoverageComponent{Kind: "management", Name: kind, State: "disabled", Message: "Management backups are disabled. Set backups.enabled: true in management configuration."}
		if !policy.Enabled {
			result = append(result, component)
			continue
		}
		saved := status.Components[kind]
		component.State, component.Message = "missing", "No completed management backup has been recorded. Check backups.status for capture progress."
		if saved.Artifact.ID != "" {
			config, resolveErr := manager.ResolveManagementS3(ctx, saved.Storage)
			entries := []managementbackup.Entry{}
			if resolveErr == nil {
				entries, resolveErr = managementbackup.List(ctx, config)
			}
			if resolveErr != nil {
				component.State, component.Message = "unknown", "The management backup archive could not be inspected. Check archive storage and OpenBao access."
			} else {
				for _, entry := range entries {
					if entry.Artifact.Kind == kind && entry.Artifact == saved.Artifact {
						component.LastSuccess = entry.Artifact.CapturedAt
						component.StaleAfter = component.LastSuccess.Add(2 * interval)
						component.State, component.Message = "available", "Completion metadata and its payload are present in archive storage."
					}
				}
			}
		}
		if saved.State == "failed" || saved.State == "retention-failed" {
			component.State, component.Message = "failed", saved.Error
		}
		result = append(result, backupFreshness(component, now))
	}
	return result, nil
}

func (manager Manager) inspectBackupTarget(ctx context.Context, target backupCoverageTarget, now time.Time) BackupCoverageComponent {
	component := target.component
	if component.State != "" {
		return component
	}
	config, err := manager.ResolveManagementS3(ctx, target.storage)
	if err != nil {
		component.State, component.Message = "unknown", "Backup storage credentials could not be read. Check OpenBao and the workload's storage configuration."
		return component
	}
	if component.Kind == "postgres" {
		backups, err := postgres.Catalogue(ctx, config, target.server)
		if err != nil {
			component.State, component.Message = "unknown", "The PostgreSQL archive could not be inspected. Check storage access."
			return component
		}
		for _, backup := range backups {
			if backup.Eligible && backup.MajorVersion == target.major && backup.CompletedAt.After(component.LastSuccess) {
				component.LastSuccess = backup.CompletedAt
			}
		}
		if !component.LastSuccess.IsZero() {
			component.StaleAfter = target.schedule.Next(component.LastSuccess).Add(time.Hour)
		}
	} else {
		client, err := objectstore.New(config)
		if err == nil {
			var objects []objectstore.Object
			objects, err = client.List(ctx, target.prefix+"/generations/")
			component.LastSuccess = sqliteArchiveActivity(objects, target.prefix)
		}
		if err != nil {
			component.State, component.Message = "unknown", "The SQLite archive could not be inspected. Check storage access and the replication container."
			return component
		}
		if !component.LastSuccess.IsZero() {
			// Idle databases need not emit WAL. Litestream takes a fresh snapshot every 24h.
			component.StaleAfter = component.LastSuccess.Add(48 * time.Hour)
		}
	}
	component.State, component.Message = "missing", "No complete backup was found for this workload. Check its backup configuration and replication logs."
	if !component.LastSuccess.IsZero() {
		component.State, component.Message = "available", "Backup metadata is present in archive storage. A restore test is required to verify recovery."
	}
	return backupFreshness(component, now)
}

func (manager Manager) offsiteBackupCoverage(ctx context.Context, now time.Time) (BackupCoverageComponent, error) {
	component := BackupCoverageComponent{Kind: "offsite", Name: "export", State: "disabled", Message: "Offsite exports are disabled. Primary archive backups are reported separately."}
	policy, policyRecord, err := loadDocument[offsitePolicy](ctx, manager.database, "management-offsite", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return component, err
	}
	if !policy.Enabled {
		return component, nil
	}
	status, err := manager.GetManagementOffsiteStatus(ctx)
	if err != nil {
		return component, err
	}
	component.State, component.Message = "missing", "No successful offsite export is recorded. Check offsite.status for progress."
	if status.Repository != policy.Repository || status.CredentialsRef != policy.CredentialsRef {
		component.Message = "No successful offsite export is recorded for the current destination and credentials. Previous export history belongs to an earlier configuration."
	} else {
		component.LastSuccess = status.LastSuccess
	}
	if !component.LastSuccess.IsZero() {
		interval, _ := time.ParseDuration(policy.Interval)
		if interval == 0 {
			interval = 6 * time.Hour
		}
		component.StaleAfter = status.LastSuccess.Add(2 * interval)
		component.State, component.Message = "available", "The last export succeeded. This does not establish backup coverage for individual databases; inspect their rows."
	}
	if (status.State == "failed" || status.State == "retention-failed") && status.PolicyRevision == policyRecord.Revision {
		component.State, component.Message = "failed", status.Error
	}
	return backupFreshness(component, now), nil
}

func backupFreshness(component BackupCoverageComponent, now time.Time) BackupCoverageComponent {
	if component.State == "available" && !component.StaleAfter.IsZero() && now.After(component.StaleAfter) {
		component.State, component.Message = "stale", "The latest observed backup is overdue for replacement. Check backup scheduling and replication."
	}
	return component
}
