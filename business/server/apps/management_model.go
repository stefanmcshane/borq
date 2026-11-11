package apps

import (
	"github.com/stefanmcshane/borq/business/server/platform"
	"time"
)

// ManagementS3Input configures primary backup archive storage with inline credentials or a vault reference.
type ManagementS3Input struct {
	// Endpoint is the S3-compatible HTTP(S) origin.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 signing region.
	Region string `json:"region"`
	// Bucket contains management backups and PostgreSQL archives.
	Bucket string `json:"bucket"`
	// Prefix contains management backups and managed-addon archives within the bucket.
	Prefix string `json:"prefix,omitempty"`
	// PathStyle enables compatible S3 bucket paths.
	PathStyle bool `json:"pathStyle"`
	// AccessKey is accepted only at the input boundary and moved into OpenBao.
	AccessKey string `json:"accessKey,omitempty"`
	// SecretKey is accepted only at the input boundary and moved into OpenBao.
	SecretKey string `json:"secretKey,omitempty"`
	// SessionToken optionally supplies temporary credentials.
	SessionToken string `json:"sessionToken,omitempty"`
	// CredentialsRef selects an immutable management-vault credential document.
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// ManagementS3 is the safe primary backup archive connection, separate from the Restic repository.
type ManagementS3 struct {
	// Endpoint is the configured S3 origin.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 signing region.
	Region string `json:"region"`
	// Bucket contains management backups and granted organizations' PostgreSQL archives.
	Bucket string `json:"bucket"`
	// Prefix is the management backup and managed-addon archive root.
	Prefix string `json:"prefix,omitempty"`
	// PathStyle selects path-style bucket addressing.
	PathStyle bool `json:"pathStyle"`
	// CredentialsRef points to an immutable OpenBao document and contains no key material.
	CredentialsRef string `json:"credentialsRef"`
}

// ManagementConfiguration declares named resources without implicit deletion.
type ManagementConfiguration struct {
	// BorqPublicURL explicitly selects the browser-facing server address used for public routing and identity callbacks.
	BorqPublicURL string `json:"borqPublicUrl,omitempty"`
	// Network records bootstrap integrations; omitted provider blocks preserve current settings.
	// A pointer preserves omission in ordinary management applies.
	Network *platform.NetworkSettings `json:"network,omitempty"`
	// Registries configures management image pulls; credentials are staged into OpenBao.
	Registries map[string]RegistryAuth `json:"registries,omitempty"`
	// Components overrides known management and target Helm components by name.
	Components map[string]platform.ComponentOverride `json:"components,omitempty"`
	// S3 selects primary backup archive storage. A pointer preserves omission.
	S3 *ManagementS3Input `json:"s3,omitempty"`
	// Backups controls independent local native captures. A pointer preserves omission.
	Backups *ManagementBackupInput `json:"backups,omitempty"`
	// Offsite configures Restic exports to external S3. A pointer preserves omission.
	Offsite *ManagementOffsiteInput `json:"offsite,omitempty"`
	// SecretGroups publishes management-scoped environment configuration in OpenBao.
	SecretGroups []ManagementSecretGroup `json:"secretGroups,omitempty"`
	// Organizations updates the named organization hierarchy.
	Organizations []ManagementOrganization `json:"organizations,omitempty"`
}

// ManagementOrganization declares an organization and its named children.
type ManagementOrganization struct {
	// Name identifies an existing or new organization.
	Name string `json:"name"`
	// ManagedAddons explicitly grants or revokes shared S3 usage. A pointer preserves omission.
	ManagedAddons *bool `json:"managedAddons,omitempty"`
	// Oidc connects an external provider. A pointer preserves omission.
	Oidc *ManagementOidc `json:"oidc,omitempty"`
	// ManagedPocketId reconciles this organization's dedicated identity provider.
	// A pointer distinguishes omission from a declared provider.
	ManagedPocketId *ManagementPocketId `json:"managedPocketId,omitempty"`
	// Clusters registers named deployment connections.
	Clusters []ClusterInput `json:"clusters,omitempty"`
	// Spaces updates named spaces without deleting omitted ones.
	Spaces []ManagementSpace `json:"spaces,omitempty"`
}

// ManagementSpace uses the same resource fragment as a standalone space file.
type ManagementSpace struct {
	// Name identifies the space within its organization.
	Name string `json:"name"`
	// Cluster binds a new space to an existing registered cluster.
	Cluster string `json:"cluster,omitempty"`
	SpaceConfiguration
}

// SpaceConfiguration is the shared apply model for apps and both addon kinds.
type SpaceConfiguration struct {
	// Registries configures image pulls only for this space's namespace.
	Registries map[string]RegistryAuth `json:"registries,omitempty"`
	// Apps merges named applications and preserves all omitted applications.
	Apps []AppPatch `json:"apps,omitempty"`
	// CustomAddons merges named pinned Helm charts.
	CustomAddons []NamedCustomAddon `json:"customAddons,omitempty"`
	// ManagedAddons contains Borq-managed database and identity services.
	ManagedAddons ManagedAddonConfiguration `json:"managedAddons,omitempty"`
}

// NamedCustomAddon attaches a resource name to the existing chart input.
type NamedCustomAddon struct {
	// Name identifies this custom addon within its space.
	Name string `json:"name"`
	AddonInput
}

// ManagedAddonConfiguration groups supported managed addon kinds.
type ManagedAddonConfiguration struct {
	// PocketId merges named SQLite identity providers.
	PocketId []NamedPocketId `json:"pocketid,omitempty"`
	// Postgres merges named PostgreSQL addons.
	Postgres []NamedPostgres `json:"postgres,omitempty"`
}

// NamedPocketId attaches a Space-local name to a managed identity provider.
type NamedPocketId struct {
	// Name identifies this provider within its Space.
	Name string `json:"name"`
	PocketIdInput
}

// NamedPostgres attaches a resource name to managed PostgreSQL settings.
type NamedPostgres struct {
	// Name identifies this PostgreSQL addon within its space.
	Name string `json:"name"`
	PostgresInput
}

// ManagementOffsiteInput controls encrypted Restic exports of management backups and PostgreSQL archives.
// Credentials must originate in a private recovery configuration retained outside Borq.
type ManagementOffsiteInput struct {
	// Enabled enables the manager-owned worker; omission of the block preserves its policy.
	Enabled bool `json:"enabled"`
	// Repository is an external s3:https://endpoint/bucket/prefix Restic location.
	Repository string `json:"repository,omitempty"`
	// Region is the external provider's signing region.
	Region string `json:"region,omitempty"`
	// Interval is a Go duration of at least one minute, for example 6h.
	Interval string `json:"interval,omitempty"`
	// KeepLast retains this many successful snapshots for this source.
	KeepLast int `json:"keepLast,omitempty"`
	// CredentialsRef identifies an immutable vault entry without key material.
	CredentialsRef string `json:"credentialsRef,omitempty"`
	// Password is accepted only for staging into the vault and must have independent custody.
	Password string `json:"password,omitempty"`
	// AccessKey and SecretKey are transient external provider credentials.
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	// SessionToken optionally supplies temporary provider credentials.
	SessionToken string `json:"sessionToken,omitempty"`
	// Status is safe read-only execution state; applying it has no effect.
	Status *ManagementOffsiteStatus `json:"status,omitempty"`
}

// ManagementOffsiteStatus preserves the last successful checkpoint across failed attempts and restarts.
type ManagementOffsiteStatus struct {
	// State is never, running, succeeded, failed, or retention-failed.
	State string `json:"state"`
	// LastAttempt records when the worker durably accepted the latest attempt.
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	// LastSuccess records the last successfully committed snapshot.
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	// NextAttempt allows due work to resume after restart without postponing its schedule.
	NextAttempt time.Time `json:"nextAttempt,omitempty"`
	// SnapshotID is the last successfully committed Restic snapshot.
	SnapshotID string `json:"snapshotId,omitempty"`
	// Source identifies the original source independently of SQLite and OpenBao.
	Source string `json:"source,omitempty"`
	// Repository and CredentialsRef bind the last success to its original repository access.
	Repository     string `json:"repository,omitempty"`
	CredentialsRef string `json:"credentialsRef,omitempty"`
	// PolicyRevision identifies the policy whose attempt owns NextAttempt.
	PolicyRevision int64 `json:"policyRevision,omitempty"`
	// Error contains a fixed safe failure message, never provider or exporter output.
	Error string `json:"error,omitempty"`
}

// ManagementOidc configures an organization's login provider and initial administrator.
type ManagementOidc struct {
	// AdminEmail selects the existing provider account administering Borq; it never changes Pocket ID roles.
	AdminEmail string `json:"adminEmail,omitempty"`
	// AdminSubject explicitly pins the first administrator by Account ID without trusting email ownership.
	AdminSubject string `json:"adminSubject,omitempty"`
	// IssuerURL is discovered after provisioning when a managed Pocket ID supplies it.
	IssuerURL string `json:"issuerUrl,omitempty"`
	// ClientID identifies the web client; managed Pocket ID can generate it.
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is moved to OpenBao before the management operation is persisted.
	ClientSecret string `json:"clientSecret,omitempty"`
	// ClientSecretRef reuses an immutable credential owned by this organization.
	ClientSecretRef string `json:"clientSecretRef,omitempty"`
	// BaseURL is Borq's public origin for login callbacks, logout returns, and application launch links.
	BaseURL string `json:"borqPublicUrl"`
}

// ManagementPocketId declares the dedicated provider owned by an organization.
type ManagementPocketId struct {
	// IssuerURL is the dedicated public Pocket ID origin.
	IssuerURL string `json:"issuerUrl"`
	// BaseURL overrides the management public origin used for callbacks and logout.
	BaseURL string `json:"borqPublicUrl,omitempty"`
	// CertificateIssuer selects an existing cert-manager ClusterIssuer for HTTPS.
	CertificateIssuer string `json:"certificateIssuer,omitempty"`
	// AdminUsername defaults to a compatible email username or stable internal alias; explicit names are retained.
	AdminUsername string `json:"adminUsername,omitempty"`
	// AdminEmail identifies the management-designated Pocket ID administrator.
	AdminEmail string `json:"adminEmail"`
	// OrganizationAdminEmail selects a different initial Borq account without granting provider administration.
	OrganizationAdminEmail string `json:"organizationAdminEmail,omitempty"`
	// Env selects a management secret group. A pointer preserves an omitted binding.
	Env *EnvReference `json:"env,omitempty"`
	// Values overrides supported legacy workload values. Omitted preserves overrides; an empty object resets them.
	Values map[string]any `json:"values,omitzero"`
	// ValuesRef reuses immutable values stored in this organization's management vault.
	ValuesRef string `json:"valuesRef,omitempty"`
}

// ManagementSecretGroup declares a management-scoped, versioned environment group.
type ManagementSecretGroup struct {
	// Organization owns the group independently of any application space.
	Organization string `json:"organization"`
	// Name identifies this group within its organization.
	Name string `json:"name"`
	// Data publishes complete values; an empty object clears them and omission selects Version.
	Data map[string]string `json:"data,omitzero"`
	// Version references an existing version when Data is omitted; latest is the default.
	Version string `json:"version,omitempty"`
}
