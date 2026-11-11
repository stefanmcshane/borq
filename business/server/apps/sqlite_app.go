package apps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"

	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func sqliteCheckName(application string) string {
	if len(application) <= 39 {
		return application + "-sqlite-check"
	}
	digest := sha256.Sum256([]byte(application))
	return fmt.Sprintf("%s-%x-sqlite-check", application[:30], digest[:4])
}

// SqliteSettings retains persistent storage independently from backup enablement.
type SqliteSettings struct {
	// Enabled allocates a database volume and exports SQLITE_DATABASE_PATH.
	Enabled bool `json:"enabled"`
	// Backups continuously replicates the database to management object storage.
	Backups bool `json:"backups"`
}

// SqlitePatch distinguishes retained settings from explicitly disabled backups.
type SqlitePatch struct {
	// Enabled creates persistent storage; accepted storage cannot be removed by an update.
	Enabled *bool `json:"enabled,omitempty"`
	// Backups overrides continuous replication, defaulting to enabled for new storage.
	Backups *bool `json:"backups,omitempty"`
}

const sqliteDirectory = "/var/lib/borq/sqlite"
const sqliteDatabasePath = sqliteDirectory + "/app.sqlite"
const sqlitePathVariable = "SQLITE_DATABASE_PATH"

const litestreamImage = sqliteworkload.Image

// The official Python image supplies SQLite integrity checks and the initialization-state client.
const sqliteRecoveryImage = "python:3.13-slim-bookworm@sha256:5024f48ba9441d4b13a95d3945abc6365538e3a31109833367a1923523c6efed"

type sqliteBackupConfig = sqliteworkload.Configuration

func mergeSqlite(current SqliteSettings, patch SqlitePatch) (SqliteSettings, error) {
	if patch.Enabled != nil {
		if current.Enabled && !*patch.Enabled {
			return current, fmt.Errorf("SQLite storage cannot be removed by an application update; disable backups instead")
		}
		if *patch.Enabled && !current.Enabled {
			current.Backups = true
		}
		current.Enabled = *patch.Enabled
	}
	if patch.Backups != nil {
		current.Backups = *patch.Backups
	}
	return current, nil
}

func sqliteValues(application App, target string, storage ManagementS3) (map[string]any, error) {
	values := map[string]any{
		"enabled": application.Sqlite.Enabled, "backups": application.Sqlite.Backups,
		"directory": sqliteDirectory, "path": sqliteDatabasePath, "variable": sqlitePathVariable,
		"volume": application.Name + "-sqlite", "config": application.Name + "-litestream",
		"secret": application.Name + "-litestream", "image": litestreamImage,
		"recoveryImage": sqliteRecoveryImage, "recoveryConfig": application.Name + "-sqlite-recovery",
		"state": application.Name + "-sqlite-state", "account": application.Name + "-sqlite",
		"check": sqliteCheckName(application.Name),
	}
	if !application.Sqlite.Backups {
		return values, nil
	}
	if storage.Bucket == "" {
		return nil, fmt.Errorf("SQLite backups require management S3 storage")
	}
	configuration := sqliteworkload.Config(sqliteDatabasePath, sqliteworkload.Destination{
		Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket,
		Prefix: path.Join(storage.Prefix, "sqlite", target, application.Name), PathStyle: storage.PathStyle,
	})
	container := sqliteworkload.ReplicationContainer(sqliteworkload.ReplicationInput{
		Name: "litestream", Directory: sqliteDirectory, DataVolume: "borq-sqlite", ConfigVolume: "borq-litestream-config",
		CredentialSecret: application.Name + "-litestream", PullPolicy: corev1.PullAlways,
	})
	// Helm values must contain JSON-shaped maps so Kubernetes field names survive YAML rendering.
	encodedContainer, err := json.Marshal(container)
	if err != nil {
		return nil, fmt.Errorf("encode SQLite replication container: %w", err)
	}
	var replication map[string]any
	if err := json.Unmarshal(encodedContainer, &replication); err != nil {
		return nil, fmt.Errorf("prepare SQLite replication values: %w", err)
	}
	values["replication"] = replication
	encoded, err := yaml.Marshal(configuration)
	if err != nil {
		return nil, fmt.Errorf("encode SQLite backup configuration: %w", err)
	}
	values["configuration"] = string(encoded)
	return values, nil
}
