package apps

import "time"

// PostgresInput configures an isolated, managed PostgreSQL cluster.
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
	// Credentials configures OpenBao password rotation and its private SQL route.
	Credentials PostgresCredentialPolicy `json:"credentials,omitempty"`
	// Resources sets Kubernetes resource requests and limits.
	Resources PostgresResources `json:"resources,omitempty"`
	// Backup configures backups and mandatory recovery verification.
	Backup PostgresBackupPolicy `json:"backup,omitempty"`
}

// PostgresCredentialPolicy controls automatically rotated database passwords.
type PostgresCredentialPolicy struct {
	// RotationPeriod defaults to 720h (30 days) and cannot disable rotation.
	RotationPeriod string `json:"rotationPeriod,omitempty"`
	// Host overrides the private SQL hostname reachable from management OpenBao.
	Host string `json:"host,omitempty"`
	// Port defaults to 5432 for the management SQL connection.
	Port int `json:"port,omitempty"`
}

// PostgresBackupPolicy configures backup retention and recovery verification.
type PostgresBackupPolicy struct {
	// Enabled uses a pointer to distinguish omitted partial updates from explicit disablement.
	// Omission on creation enables backups.
	Enabled *bool `json:"enabled,omitempty"`
	// RestoreCheck configures mandatory tests while backups are enabled.
	RestoreCheck PostgresRestoreCheckInput `json:"restoreCheck,omitempty"`
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
	// Protection reports observed backup and recovery-test health.
	Protection PostgresProtection `json:"protection"`
	// RestoreCheck reports the latest isolated verification outcome.
	RestoreCheck PostgresRestoreCheck `json:"restoreCheck"`
	// UpdatedAt is the latest accepted or completed state transition.
	UpdatedAt time.Time `json:"updatedAt"`
}

// PostgresProtection reports recoverability independently of database availability.
type PostgresProtection struct {
	// Status is disabled, pending, unverified, ready, or degraded.
	Status string `json:"status"`
	// Message explains protection separately from database readiness.
	Message string `json:"message"`
	// LastBackupAt is the latest observed backup in the current archive.
	LastBackupAt time.Time `json:"lastBackupAt,omitempty"`
	// LastTestAt is the last successful restore test of the current archive.
	LastTestAt time.Time `json:"lastTestAt,omitempty"`
	// RecoveryWindowDays is the configured recovery retention window.
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
	// RecoveryEnd is the latest time confirmed by completed backups; newer archived WAL can extend recovery.
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
	// RecoveryTarget selects a time after backup completion; PostgreSQL must verify that archived WAL reaches it.
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
