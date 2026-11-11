// Package sqliteworkload constructs the managed SQLite replication configuration
// shared by application workloads and management services.
package sqliteworkload

import (
	"errors"
	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
)

// Image pins the restore-tested backend with native age archive encryption.
const Image = "litestream/litestream:0.3.14@sha256:c5a1e1b01916b3a110f6600820ef176d048d9b9c2411ef0a680de0caa68934a5"

// Destination identifies the workload's scoped S3 archive.
type Destination struct {
	// Endpoint is reachable from the workload and recovery pods.
	Endpoint string
	// Region selects the S3 signing region.
	Region string
	// Bucket belongs to the owning application space or management service.
	Bucket string
	// Prefix restricts this workload's archive within the bucket.
	Prefix string
	// PathStyle enables RustFS-compatible bucket addressing.
	PathStyle bool
	// SyncInterval overrides the backend default when explicitly required.
	SyncInterval string
}

// Configuration is the pinned Litestream backend's on-disk configuration.
type Configuration struct {
	// Databases contains the managed database and its replica.
	Databases []Database `json:"dbs"`
}

// Database identifies one persistent SQLite file.
type Database struct {
	// Path is shared by the application and replication containers.
	Path string `json:"path"`
	// Replicas contains the managed S3 destination.
	Replicas []Replica `json:"replicas"`
}

// Replica holds the encrypted S3 backup policy shared by all managed SQLite workloads.
type Replica struct {
	// Age encrypts every snapshot and WAL object with the workload's vaulted key.
	Age AgeConfiguration `json:"age"`
	// Type selects the S3 backend.
	Type string `json:"type"`
	// Endpoint is the workload-reachable S3 origin.
	Endpoint string `json:"endpoint"`
	// Region selects request signing.
	Region string `json:"region"`
	// Bucket owns the archive.
	Bucket string `json:"bucket"`
	// Path isolates this workload's objects.
	Path string `json:"path"`
	// PathStyle uses compatible bucket addressing.
	PathStyle bool `json:"force-path-style"`
	// Retention bounds the recovery window.
	Retention string `json:"retention"`
	// SnapshotInterval bounds snapshot creation frequency.
	SnapshotInterval string `json:"snapshot-interval"`
	// SyncInterval is omitted when the backend default applies.
	SyncInterval string `json:"sync-interval,omitempty"`
}

// Config applies Borq's seven-day retention and daily snapshot policy.
func Config(databasePath string, destination Destination) Configuration {
	return Configuration{Databases: []Database{{Path: databasePath, Replicas: []Replica{{
		Age:  AgeConfiguration{Identities: []string{"${LITESTREAM_AGE_IDENTITY}"}, Recipients: []string{"${LITESTREAM_AGE_RECIPIENT}"}},
		Type: "s3", Endpoint: destination.Endpoint, Region: destination.Region,
		Bucket: destination.Bucket, Path: destination.Prefix, PathStyle: destination.PathStyle,
		Retention: "168h", SnapshotInterval: "24h", SyncInterval: destination.SyncInterval,
	}}}}}
}

// ReplicationInput connects replication to the workload's existing storage and credentials.
type ReplicationInput struct {
	// Name preserves the workload's established container identity.
	Name string
	// Directory contains the database file.
	Directory string
	// DataVolume names the existing persistent volume in the pod.
	DataVolume string
	// ConfigVolume provides litestream.yml.
	ConfigVolume string
	// CredentialSecret provides scoped S3 credentials, never application credentials.
	CredentialSecret string
	// PullPolicy preserves the owning workload's image policy.
	PullPolicy corev1.PullPolicy
}

// ReplicationContainer starts before the writer and shuts down after its final
// database writes. Callers retain their own pod and container security policy.
func ReplicationContainer(input ReplicationInput) corev1.Container {
	// Kubernetes represents native sidecar restart policy with a pointer.
	restart := corev1.ContainerRestartPolicyAlways
	return corev1.Container{Name: input.Name, Image: Image, ImagePullPolicy: input.PullPolicy, RestartPolicy: &restart,
		Command: []string{"litestream"}, Args: []string{"replicate", "-config", "/etc/litestream/litestream.yml"},
		EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: input.CredentialSecret}}}},
		VolumeMounts: []corev1.VolumeMount{{Name: input.DataVolume, MountPath: input.Directory},
			{Name: input.ConfigVolume, MountPath: "/etc/litestream", ReadOnly: true}},
	}
}

// AgeConfiguration is Litestream 0.3.14's native encryption configuration.
// Managed configurations contain environment placeholders, never key material.
type AgeConfiguration struct {
	// Identities decrypt archived snapshots and WAL segments during restoration.
	Identities []string `json:"identities"`
	// Recipients encrypt every new snapshot and WAL segment.
	Recipients []string `json:"recipients"`
}

// ArchiveKey protects one workload's SQLite archive and is stored only in OpenBao
// and the dedicated Kubernetes archive credential Secret.
type ArchiveKey struct {
	// Identity decrypts this workload's encrypted archive.
	Identity string `json:"-"`
	// Recipient is the public encryption counterpart of Identity.
	Recipient string `json:"-"`
}

// NewArchiveKey generates a workload-specific age X25519 key pair.
func NewArchiveKey() (ArchiveKey, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return ArchiveKey{}, errors.New("generate SQLite archive encryption key")
	}
	return ArchiveKey{Identity: identity.String(), Recipient: identity.Recipient().String()}, nil
}

// Validate rejects absent or mismatched encryption credentials before replication.
func (key ArchiveKey) Validate() error {
	identity, err := age.ParseX25519Identity(key.Identity)
	if err != nil || identity.Recipient().String() != key.Recipient {
		return errors.New("SQLite archive encryption keys are missing or invalid; restore their original vault entry")
	}
	return nil
}
