package apps

import "time"

// PostgresInput configures an isolated, automatically protected PostgreSQL cluster.
type PostgresInput struct {
	// Instances is the number of PostgreSQL instances; omission selects one.
	Instances int `json:"instances,omitempty"`
	// Storage is the capacity of each instance; omission selects 1Gi.
	Storage string `json:"storage,omitempty"`
	// StorageClass selects the persistent storage driver.
	StorageClass string `json:"storageClass,omitempty"`
	// Image pins the PostgreSQL operand; omission selects the tested default.
	Image string `json:"image,omitempty"`
	// Database is the initial application database; omission selects app.
	Database string `json:"database,omitempty"`
	// Username is the application role; omission selects app.
	Username string `json:"username,omitempty"`
	// Resources sets Kubernetes resource requests and limits.
	Resources PostgresResources `json:"resources,omitempty"`
	// Backup configures mandatory full backups and WAL retention.
	Backup PostgresBackupPolicy `json:"backup,omitempty"`
	// SnapshotSchedule uses five cron fields and defaults to hourly.
	SnapshotSchedule string `json:"snapshotSchedule,omitempty"`
	// SnapshotRetentionHours bounds CSI backup retention; omission selects 24.
	SnapshotRetentionHours int `json:"snapshotRetentionHours,omitempty"`
	// Snapshots selects preferred, required, or disabled CSI protection.
	Snapshots string `json:"snapshots,omitempty"`
	// SnapshotClass selects an optional CSI snapshot class.
	SnapshotClass string `json:"snapshotClass,omitempty"`
	// RestoreCheck configures automatic isolated recovery verification.
	RestoreCheck PostgresRestoreCheckInput `json:"restoreCheck,omitempty"`
}

// PostgresBackupPolicy configures mandatory object storage protection.
type PostgresBackupPolicy struct {
	// Schedule is a five-field UTC cron expression; omission selects daily at midnight.
	Schedule string `json:"schedule,omitempty"`
	// RecoveryWindowDays retains a recovery window; omission selects 30 days.
	RecoveryWindowDays int `json:"recoveryWindowDays,omitempty"`
}

// PostgresResources constrains instance or verification resources.
type PostgresResources struct {
	// Requests reserves CPU and memory.
	Requests map[string]string `json:"requests,omitempty"`
	// Limits caps CPU and memory.
	Limits map[string]string `json:"limits,omitempty"`
}

// PostgresRestoreCheckInput schedules recovery verification without exposing credentials.
type PostgresRestoreCheckInput struct {
	// Enabled distinguishes omitted weekly verification from explicit disablement.
	Enabled *bool `json:"enabled,omitempty"`
	// Schedule is a five-field UTC cron expression; omission selects Sunday at 03:00.
	Schedule string `json:"schedule,omitempty"`
	// TimeoutSeconds bounds the complete verification and cleanup workflow.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Resources constrains the temporary recovered database.
	Resources PostgresResources `json:"resources,omitempty"`
}

// Postgres is accepted configuration and observed protection state, without credentials.
type Postgres struct {
	// ID changes for every new incarnation, including reuse of a deleted name.
	ID string `json:"id"`
	// Name identifies this managed addon within its space.
	Name string `json:"name"`
	// Specification is normalized accepted configuration.
	Specification PostgresInput `json:"specification"`
	// Namespace is the fixed namespace of its owning space.
	Namespace string `json:"namespace"`
	// ClusterName is the isolated Kubernetes PostgreSQL cluster identity.
	ClusterName string `json:"clusterName"`
	// EnvironmentGroup selects this database incarnation’s published connection values.
	EnvironmentGroup string `json:"environmentGroup"`
	// State is pending, ready, degraded, unavailable, failed, deleting, or removed.
	State string `json:"state"`
	// Message explains the latest durable operation outcome.
	Message string `json:"message"`
	// Operation identifies the latest accepted operation.
	Operation string `json:"operation"`
	// Protection reports independently observed backup and snapshot health.
	Protection PostgresProtection `json:"protection"`
	// RestoreCheck reports the latest isolated verification outcome.
	RestoreCheck PostgresRestoreCheck `json:"restoreCheck"`
	// UpdatedAt is the latest accepted or completed state transition.
	UpdatedAt time.Time `json:"updatedAt"`
}

// PostgresProtection distinguishes full backups, WAL archiving, and CSI capability.
type PostgresProtection struct {
	// Backups is ready, pending, failed, or unavailable.
	Backups string `json:"backups"`
	// Wal is ready, pending, failed, or unavailable.
	Wal string `json:"wal"`
	// Snapshots is supported, unsupported, disabled, pending, or failed.
	Snapshots string `json:"snapshots"`
	// Message explains protection degradation separately from database readiness.
	Message string `json:"message"`
	// LastBackupAt is the last observed completed full backup.
	LastBackupAt time.Time `json:"lastBackupAt,omitempty"`
	// RecoveryWindowDays is the minimum configured recovery retention window.
	RecoveryWindowDays int `json:"recoveryWindowDays"`
}

// PostgresRestoreCheck keeps recovery and cleanup results independently visible.
type PostgresRestoreCheck struct {
	// Status is never, pending, running, succeeded, or failed.
	Status string `json:"status"`
	// Operation identifies the durable verification task.
	Operation string `json:"operation,omitempty"`
	// Message describes the SQL verification result without credentials.
	Message string `json:"message,omitempty"`
	// SnapshotStatus reports succeeded, failed, unsupported, or disabled CSI recovery verification.
	SnapshotStatus string `json:"snapshotStatus,omitempty"`
	// Cleanup reports pending, succeeded, or failed resource cleanup.
	Cleanup string `json:"cleanup,omitempty"`
	// StartedAt is the original attempt start.
	StartedAt time.Time `json:"startedAt,omitempty"`
	// CompletedAt is the verification completion time.
	CompletedAt time.Time `json:"completedAt,omitempty"`
	// DurationSeconds measures the complete recovery verification.
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
}

// PostgresBackup is an approved or rejected physical backup read from real object storage.
type PostgresBackup struct {
	// ID is an opaque Borq catalogue identity, never a freely supplied object key.
	ID string `json:"id"`
	// ArchiveID identifies an immutable source archive.
	ArchiveID string `json:"archiveId"`
	// SourceID is the immutable original PostgreSQL incarnation.
	SourceID string `json:"sourceId"`
	// SourceName is the friendly source name retained after deletion.
	SourceName string `json:"sourceName"`
	// BackupID is Barman's physical backup identity.
	BackupID string `json:"backupId"`
	// CompletedAt records the physical backup completion.
	CompletedAt time.Time `json:"completedAt"`
	// MajorVersion is the PostgreSQL physical format major.
	MajorVersion int `json:"majorVersion"`
	// SizeBytes is the physical backup size.
	SizeBytes int64 `json:"sizeBytes"`
	// RecoveryStart is the earliest safe target for this base backup.
	RecoveryStart time.Time `json:"recoveryStart"`
	// RecoveryEnd is the latest conservatively established WAL recovery target.
	RecoveryEnd time.Time `json:"recoveryEnd"`
	// Eligible means the metadata is complete and compatible.
	Eligible bool `json:"eligible"`
	// Reason explains rejection or recovery bounds.
	Reason string `json:"reason,omitempty"`
}

// PostgresRestoreInput restores one approved backup into a fresh destination.
type PostgresRestoreInput struct {
	// BackupID selects an opaque entry returned by ListPostgresBackups.
	BackupID string `json:"backupId"`
	// RecoveryTarget optionally selects a point in time within the available WAL range.
	RecoveryTarget time.Time `json:"recoveryTarget,omitempty"`
	// Specification sets destination sizing and verification policy.
	Specification PostgresInput `json:"specification,omitempty"`
}

// PostgresMetrics combines scoped resource usage and live PostgreSQL activity.
type PostgresMetrics struct {
	// ActivityAvailable indicates whether SQL activity and database size were observed.
	ActivityAvailable bool `json:"activityAvailable"`
	// Usage contains CPU and memory history.
	Usage Metrics `json:"usage"`
	// DiskBytes is current database size measured by PostgreSQL.
	DiskBytes int64 `json:"diskBytes"`
	// DiskAvailable indicates whether volume capacity and usage were observed.
	DiskAvailable bool `json:"diskAvailable"`
	// DiskUsedBytes is observed storage use across the PostgreSQL volumes.
	DiskUsedBytes int64 `json:"diskUsedBytes"`
	// DiskCapacityBytes is observed total capacity across those volumes.
	DiskCapacityBytes int64 `json:"diskCapacityBytes"`
	// DiskAvailableBytes is observed remaining capacity across those volumes.
	DiskAvailableBytes int64 `json:"diskAvailableBytes"`
	// Connections is the current number of connected sessions.
	Connections int64 `json:"connections"`
	// ActiveQueries is the current number of non-idle sessions.
	ActiveQueries int64 `json:"activeQueries"`
	// Transactions is committed plus rolled back transactions since statistics reset.
	Transactions int64 `json:"transactions"`
	// Protection separates backup health from database resource use.
	Protection PostgresProtection `json:"protection"`
	// Notice explains unavailable portions without substituting zero observations.
	Notice string `json:"notice,omitempty"`
}

// PostgresCheckInput authenticates one scheduled restore check from its bound Kubernetes Job.
type PostgresCheckInput struct {
	// Token is the projected audience-bound service-account token; it is never persisted.
	Token string `json:"-"`
	// JobUID binds idempotent acceptance to one live Job owned by the resource CronJob.
	JobUID string `json:"jobUid"`
}
