// Package offsite captures managed PostgreSQL S3 archives in encrypted Restic snapshots.
package offsite

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
)

// Repository selects an independently recoverable Restic repository.
type Repository struct {
	cacheDir string
	// URL is an S3 Restic repository, without embedded credentials or query parameters.
	URL string `json:"repository"`
	// Password must also be retained outside the management cluster.
	Password string `json:"password"`
	// AccessKey and SecretKey authenticate to the offsite provider.
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	// SessionToken optionally authenticates a temporary principal.
	SessionToken string `json:"sessionToken,omitempty"`
	// Region selects the external S3 signing region.
	Region string `json:"region"`
}

// Object is one captured relative S3 key and its verified content identity.
type Object struct {
	// Key preserves the source's relative organization/archive/server layout.
	Key string `json:"key"`
	// Size is the verified byte length.
	Size int64 `json:"size"`
	// ETag is the source's captured identity, used for conditional reads.
	ETag string `json:"etag"`
	// SHA256 verifies the streamed bytes independently of provider ETags.
	SHA256 string `json:"sha256"`
}

// Archive is the terminal versioned completion manifest. Its presence is required for recovery.
type Archive struct {
	// Version selects the archive protocol.
	Version int `json:"version"`
	// Source scopes snapshots and retention to this bucket and root.
	Source string `json:"source"`
	// Objects contains every regular object preceding this manifest in sorted order.
	Objects []Object `json:"objects"`
	// Native records complete independently encrypted management component artifacts.
	Native []managementbackup.Artifact `json:"native"`
	// Backups records only recovery boundaries established by the shared catalogue evaluator.
	Backups map[string][]postgres.BackupMetadata `json:"backups"`
}

// RecoveryConfig is operator-created private material retained independently of Borq.
type RecoveryConfig struct {
	// Repository contains the password and provider credentials supplied during setup.
	Repository Repository `json:"offsite"`
	// Source identifies the original archive source for snapshot selection.
	Source string `json:"source"`
	// Local selects local or offsite-rehydrated S3 for native component recovery.
	Local StorageConfig `json:"local"`
	// Identity is an independently retained age private identity, used only for native restore.
	Identity string `json:"identity"`
	// OpenBao contains independent native vault recovery material.
	OpenBao managementbackup.OpenBaoRecovery `json:"openbao"`
	// Destination is a fresh S3 prefix with independently supplied credentials.
	Destination StorageConfig `json:"destination"`
}

// StorageConfig is a private file boundary for source or recovery destination credentials.
type StorageConfig struct {
	// Endpoint, Region, Bucket, Prefix and PathStyle select the S3 location.
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	PathStyle bool   `json:"pathStyle"`
	// AccessKey, SecretKey and SessionToken are private file inputs only.
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken,omitempty"`
}

// Config returns a transient client configuration without ambient credentials.
func (storage StorageConfig) Config() objectstore.Config {
	return objectstore.Config{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: storage.PathStyle, Credentials: objectstore.Credentials{AccessKey: storage.AccessKey, SecretKey: storage.SecretKey, SessionToken: storage.SessionToken}}
}

// SourceID is stable across process restarts and separates repository histories by source.
func SourceID(config objectstore.Config) string {
	digest := sha256.Sum256([]byte(strings.TrimRight(config.Endpoint, "/") + "\n" + config.Region + "\n" + config.Bucket + "\n" + strings.Trim(config.Prefix, "/")))
	return hex.EncodeToString(digest[:16])
}

// Validate checks shape without revealing user-provided secrets in errors.
func (repository Repository) Validate() error {
	location := strings.TrimPrefix(repository.URL, "s3:")
	parsed, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(repository.URL, "s3:") || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Path == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("offsite repository must be s3:https://endpoint/bucket/prefix without credentials or query parameters")
	}
	if repository.Password == "" || repository.AccessKey == "" || repository.SecretKey == "" || repository.Region == "" {
		return errors.New("offsite password, access key, secret key and region are required from an independently retained recovery configuration")
	}
	return nil
}

// Snapshot describes one completed source-scoped Restic snapshot.
type Snapshot struct {
	// ID identifies the encrypted snapshot.
	ID string `json:"id"`
	// Time is Restic's completed snapshot time.
	Time time.Time `json:"time"`
	// Tags preserves source identity for recovery validation.
	Tags []string `json:"tags"`
}

// ValidateSeparation rejects a repository in the same source bucket, including aliases of AWS regional defaults.
// A separate bucket prevents recursive capture and keeps offsite bytes independent of the source namespace.
func ValidateSeparation(repository Repository, source objectstore.Config) error {
	if err := repository.Validate(); err != nil {
		return err
	}
	location, _ := url.Parse(strings.TrimPrefix(repository.URL, "s3:"))
	parts := strings.Split(strings.Trim(location.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == source.Bucket {
		return errors.New("offsite repository must use a separate external S3 bucket from source archives")
	}
	return nil
}
