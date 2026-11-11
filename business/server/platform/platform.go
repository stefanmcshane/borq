// Package platform manages Borq's pinned management and deployment components.
// Kubernetes Helm release records retain the installed chart, values and history.
package platform

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ManagementNamespace contains Borq's server, vault, and management services.
	ManagementNamespace = "borq-mgmt"
	// SystemNamespace contains shared cluster controllers and application credentials.
	SystemNamespace = "borq-system"
)

// Settings describes the topology shared by bootstrap and cluster registration.
type Settings struct {
	// BorqPublicURL is the explicitly configured public server origin, independent of the cluster name.
	BorqPublicURL string `json:"borqPublicUrl,omitempty"`
	// Network configures management-owned DNS and shared tailnet access.
	Network NetworkSettings `json:"network,omitempty"`
	// ClusterName identifies this management installation for shared storage naming.
	ClusterName string `json:"clusterName,omitempty"`
	// S3Endpoint advertises management RustFS to deployment targets.
	S3Endpoint string `json:"s3Endpoint,omitempty"`
	// Components holds validated chart coordinate and values overrides.
	Components map[string]ComponentOverride `json:"components,omitempty"`
	// S3 selects shared object storage through vault references only.
	S3 S3Settings `json:"s3,omitempty"`
	// ApiUrl is the management API base used for default vault and telemetry routes.
	ApiUrl string `json:"apiUrl,omitempty"`
	// Profile selects lightweight "dev" or persistent "production" defaults.
	Profile string `json:"profile"`
	// Namespace contains management components; defaults to borq-mgmt. Targets use borq-system.
	Namespace string `json:"namespace"`
	// OpenBaoURL is the management vault address reachable from target clusters.
	OpenBaoURL string `json:"openBaoUrl"`
	// TelemetryURL is Borq's authenticated ingestion gateway address.
	TelemetryURL string `json:"telemetryUrl"`
	// LokiURL optionally overrides the target's full authenticated log push URL.
	LokiURL string `json:"lokiUrl,omitempty"`
	// PrometheusURL optionally overrides its full authenticated remote-write URL.
	PrometheusURL string `json:"prometheusUrl,omitempty"`
	// StorageClass selects the local disk class; defaults to local-path.
	StorageClass string `json:"storageClass"`
	// Timeout bounds each component's installation or upgrade.
	Timeout time.Duration `json:"timeout"`
}

// ManagementInput describes an installation into Borq's own cluster.
type ManagementInput struct {
	// ReportProgress publishes credential-free networking stages to the owning operation.
	ReportProgress func(string)
	// KubeconfigPath connects to an existing cluster; empty uses in-cluster auth.
	KubeconfigPath string
	// Settings determines the installed topology and resource defaults.
	Settings Settings
	// UpgradeComponents limits existing release upgrades; nil upgrades the full catalog.
	UpgradeComponents []string
	// Upgrade permits chart version and ordinary component value changes.
	// Networking values reconcile automatically at the installed catalog version.
	Upgrade bool
	// RecoverPending permits startup to resume shared controllers with durable interrupted state.
	// Explicit upgrades leave this disabled so they cannot take over a live operation.
	RecoverPending bool
}

// TargetInput describes the components installed when registering a cluster.
type TargetInput struct {
	// KubeconfigPath connects to the registered deployment cluster.
	KubeconfigPath string
	// Context selects a specific kubeconfig context without changing the file.
	Context string
	// InCluster uses the Borq pod's Kubernetes credentials.
	InCluster bool
	// Organization identifies the registered cluster's owning organization.
	Organization string
	// Name is the cluster's human-readable registration name.
	Name string
	// ID is the stable registration identity used in secret and telemetry scopes.
	ID string
	// Settings supplies the reachable management endpoints.
	Settings Settings
	// UpgradeComponents limits existing release upgrades; nil upgrades the full catalog.
	UpgradeComponents []string
	// Upgrade permits chart version and ordinary component value changes.
	// Networking values reconcile automatically at the installed catalog version.
	Upgrade bool
	// RecoverPending resumes Borq-owned Helm operations interrupted by a server
	// restart. Only durable startup recovery may enable this flag.
	RecoverPending bool `json:"-"`
	// SecretMount is the management vault KV v2 mount scoped to this cluster.
	SecretMount string `json:"-"`
	// TailscaleClientID identifies the selected physical cluster's operator OAuth client.
	TailscaleClientID string `json:"-"`
	// TailscaleClientSecret is delivered privately, never as Helm values.
	TailscaleClientSecret string `json:"-"`
	// ClusterToken is the scoped vault credential; it is never returned in state.
	ClusterToken string `json:"-"`
}

// Result reports safe component state after a completed or interrupted run.
type Result struct {
	// Profile identifies the resource defaults used by this reconciliation.
	Profile string `json:"profile"`
	// Namespace contains installation state and is the default release namespace.
	Namespace string `json:"namespace"`
	// Components remain in catalog order while independent installations overlap.
	Components []ComponentState `json:"components"`
}

// ComponentState reports installed release identity without exposing secrets.
type ComponentState struct {
	// Name is the stable Borq component identity and Helm release name.
	Name string `json:"name"`
	// Namespace identifies the actual Helm release namespace, including shared controllers.
	Namespace string `json:"namespace,omitempty"`
	// Chart is the upstream chart name or OCI reference.
	Chart string `json:"chart"`
	// Version is the actual chart version installed in the cluster.
	Version string `json:"version"`
	// Revision is Helm's persisted release revision.
	Revision int `json:"revision"`
	// Status is pending, installing, blocked by a failed dependency, or the Helm release status.
	Status string `json:"status"`
	// Changed indicates this run created a release revision.
	Changed bool `json:"changed"`
	// UpgradeAvailable indicates Borq's desired chart or values differ.
	UpgradeAvailable bool `json:"upgradeAvailable"`
}

// ReconcileManagement installs missing management components in dependency order.
// Networking values reconcile at the installed catalog version; other changes
// require Upgrade to be explicitly enabled.
func ReconcileManagement(ctx context.Context, input ManagementInput) (Result, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.reconcile_management", trace.WithAttributes(attribute.String("profile", input.Settings.Profile), attribute.Bool("upgrade", input.Upgrade)))
	defer span.End()
	return reconcileManagement(ctx, input)
}

// ReadManagement reads live Helm release records without changing the cluster.
func ReadManagement(ctx context.Context, input ManagementInput) (Result, error) {
	return readManagement(ctx, input)
}

// ReconcileTarget installs registered cluster components and vault bindings.
// Networking values reconcile at the installed catalog version; other changes
// require Upgrade to be explicitly enabled.
func ReconcileTarget(ctx context.Context, input TargetInput) (Result, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.reconcile_target", trace.WithAttributes(attribute.String("cluster_id", input.ID), attribute.Bool("upgrade", input.Upgrade)))
	defer span.End()
	return reconcileTarget(ctx, input)
}
