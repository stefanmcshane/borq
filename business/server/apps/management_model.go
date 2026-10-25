package apps

import "github.com/stefanmcshane/borq/business/server/platform"

// ManagementS3Input accepts either transient inline credentials or a vault reference.
type ManagementS3Input struct {
	// Endpoint is the S3-compatible HTTP(S) origin.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 signing region.
	Region string `json:"region"`
	// Bucket is the existing shared bucket.
	Bucket string `json:"bucket"`
	// Prefix isolates managed-addon archives within the shared bucket.
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

// ManagementS3 is the safe persisted and exported shared storage connection.
type ManagementS3 struct {
	// Endpoint is the configured S3 origin.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 signing region.
	Region string `json:"region"`
	// Bucket contains management and granted organization data.
	Bucket string `json:"bucket"`
	// Prefix is the shared managed-addon archive root.
	Prefix string `json:"prefix,omitempty"`
	// PathStyle selects path-style bucket addressing.
	PathStyle bool `json:"pathStyle"`
	// CredentialsRef points to an immutable OpenBao document and contains no key material.
	CredentialsRef string `json:"credentialsRef"`
}

// ManagementConfiguration declares named resources without implicit deletion.
type ManagementConfiguration struct {
	// Components overrides known management and target Helm components by name.
	Components map[string]platform.ComponentOverride `json:"components,omitempty"`
	// S3 is optional so omitted storage remains unchanged. A pointer preserves omission.
	S3 *ManagementS3Input `json:"s3,omitempty"`
	// Organizations updates the named organization hierarchy.
	Organizations []ManagementOrganization `json:"organizations,omitempty"`
}

// ManagementOrganization declares an organization and its named children.
type ManagementOrganization struct {
	// Name identifies an existing or new organization.
	Name string `json:"name"`
	// ManagedAddons explicitly grants or revokes shared S3 usage. A pointer preserves omission.
	ManagedAddons *bool `json:"managedAddons,omitempty"`
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
	// Apps merges named applications and preserves all omitted applications.
	Apps []AppPatch `json:"apps,omitempty"`
	// CustomAddons merges named pinned Helm charts.
	CustomAddons []NamedCustomAddon `json:"customAddons,omitempty"`
	// ManagedAddons contains Borq-managed database services.
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
	// Postgres merges named PostgreSQL addons.
	Postgres []NamedPostgres `json:"postgres,omitempty"`
}

// NamedPostgres attaches a resource name to managed PostgreSQL settings.
type NamedPostgres struct {
	// Name identifies this PostgreSQL addon within its space.
	Name string `json:"name"`
	PostgresInput
}
