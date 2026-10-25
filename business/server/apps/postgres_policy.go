package apps

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func postgresBackupsEnabled(input PostgresInput) bool {
	return input.Backup.Enabled == nil || *input.Backup.Enabled
}

// UnmarshalJSON migrates only private persisted records, never public requests.
func (record *postgresRecord) UnmarshalJSON(data []byte) error {
	type persisted postgresRecord
	var decoded persisted
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.PolicyVersion == 0 {
		var legacy struct {
			Postgres struct {
				Specification struct {
					RestoreCheck *PostgresRestoreCheckInput `json:"restoreCheck"`
				} `json:"specification"`
			} `json:"postgres"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		if legacy.Postgres.Specification.RestoreCheck != nil {
			decoded.Postgres.Specification.Backup.RestoreCheck = *legacy.Postgres.Specification.RestoreCheck
		}
	}
	*record = postgresRecord(decoded)
	return nil
}

func (manager Manager) migratePostgresPolicy(ctx context.Context) error {
	records, err := manager.database.List(ctx, "postgres")
	if err != nil {
		return err
	}
	for _, record := range records {
		var stored postgresRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return err
		}
		if stored.PolicyVersion != 0 {
			continue
		}
		specification, err := normalizePostgres(stored.Postgres.Specification)
		if err != nil {
			return err
		}
		stored.Postgres.Specification = specification
		stored.LastTestArchive, stored.LastTestAt = "", time.Time{}
		stored.Postgres.Protection = postgresProtection(stored, postgres.Observation{}, time.Now().UTC())
		stored.PolicyVersion = 1
		stored.NeedsPolicyApply = stored.Postgres.State != "removed" && stored.Postgres.State != "deleting"
		writes := []sqlite.Write{}
		if stored.ActiveOperation == "" && stored.ActiveCheck == "" && stored.Postgres.State != "removed" && stored.Postgres.State != "deleting" {
			id, err := newId()
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			stored.ActiveOperation, stored.Postgres.Operation = id, id
			target, err := manager.GetSpace(ctx, stored.Organization, stored.Space)
			if err != nil {
				return err
			}
			stored.NeedsPolicyApply = false
			operation := Operation{ID: id, SpaceID: target.ID, Status: "pending", Phase: "postgres-apply", Message: "updating mandatory backup verification", StartedAt: now, UpdatedAt: now}
			task := postgresTask{Key: record.Key, Action: "apply", Deadline: now.Add(manager.runtime.rolloutTimeout)}
			for _, item := range []struct {
				collection string
				value      any
			}{{"postgres-operation", operation}, {"postgres-task", task}} {
				write, err := documentWrite(item.collection, id, item.value, 0)
				if err != nil {
					return err
				}
				writes = append(writes, write)
			}
		}
		write, err := documentWrite("postgres", record.Key, stored, record.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, write)
		if err := manager.database.Commit(ctx, writes); err != nil {
			return err
		}
	}
	return nil
}

func nextPostgresArchive(previous postgresArchive, specification PostgresInput) (postgresArchive, error) {
	id, err := newId()
	if err != nil {
		return postgresArchive{}, err
	}
	previous.Prefix = strings.TrimSuffix(previous.Prefix, "/"+previous.ID) + "/" + id
	previous.ID, previous.ServerName = id, "archive-"+id
	previous.Specification, previous.CreatedAt = specification, time.Now().UTC()
	return previous, nil
}

func postgresProtection(stored postgresRecord, observed postgres.Observation, now time.Time) PostgresProtection {
	policy := stored.Postgres.Specification.Backup
	result := PostgresProtection{Status: "disabled", Message: "Backups are disabled; existing backups remain available for restore.", RecoveryWindowDays: policy.RecoveryWindowDays}
	if !postgresBackupsEnabled(stored.Postgres.Specification) {
		return result
	}
	if observed.CheckUnavailable {
		result.Status, result.Message = "degraded", "Restore-test routing is unavailable."
		return result
	}
	result.LastBackupAt = observed.LastBackupAt
	if stored.LastTestArchive == stored.ArchiveID {
		result.LastTestAt = stored.LastTestAt
	}
	result.Status, result.Message = "pending", "Waiting for backup protection."
	if observed.Backups == "failed" || observed.Wal == "failed" {
		result.Status, result.Message = "degraded", "Backup protection needs attention."
		return result
	}
	if observed.Backups != "ready" || observed.Wal != "ready" {
		return result
	}
	result.Status, result.Message = "unverified", "Waiting for a successful restore test of the current backups."
	check := stored.Postgres.RestoreCheck
	if (stored.LastTestOutcomeArchive == stored.ArchiveID && stored.LastTestOutcome == "failed") || check.Status == "failed" || check.Cleanup == "failed" || (check.Cleanup == "pending" && !check.CompletedAt.IsZero()) {
		result.Status, result.Message = "degraded", "The latest restore test or its cleanup failed."
		return result
	}
	if stored.LastTestArchive != stored.ArchiveID || stored.LastTestAt.IsZero() {
		return result
	}
	result.LastTestAt = stored.LastTestAt
	schedule, err := cron.ParseStandard(policy.RestoreCheck.Schedule)
	if err != nil {
		return result
	}
	due := schedule.Next(stored.LastTestAt)
	if due.IsZero() || now.After(due.Add(time.Duration(policy.RestoreCheck.TimeoutSeconds)*time.Second)) {
		result.Status, result.Message = "degraded", "A restore test is overdue."
		return result
	}
	if check.Cleanup == "pending" && !check.StartedAt.IsZero() && now.After(check.StartedAt.Add(time.Duration(policy.RestoreCheck.TimeoutSeconds)*time.Second)) {
		result.Status, result.Message = "degraded", "The restore test has exceeded its deadline or is waiting for cleanup."
		return result
	}
	result.Status, result.Message = "ready", "Backups are protected and the latest restore test succeeded."
	return result
}
