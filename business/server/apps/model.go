// Package apps expands partial application configuration and renders deployment artifacts.
package apps

// Configuration is the complete desired application state for one deployment target.
type Configuration struct {
	// Apps contains every desired app, including apps omitted from the latest update.
	Apps []App `json:"apps"`
}

// Patch describes a partial update. Pointers preserve omitted fields separately
// from explicit zero values and empty lists; lists merge by name unless empty.
type Patch struct {
	// Apps updates named apps, retaining entries omitted from a nonempty list.
	Apps *[]AppPatch `json:"apps,omitempty"`
}

// App is a fully expanded application definition with explicit defaults.
type App struct {
	// Name identifies the app and its resources and is limited to 50 characters.
	Name string `json:"name"`
	// Image is the container image reference to deploy.
	Image string `json:"image"`
	// Port is the container and Service port; zero disables networking.
	Port int `json:"port"`
	// Replicas is the requested number of running instances, including zero.
	Replicas int `json:"replicas"`
	// Env selects a named secret group and its published version.
	Env EnvReference `json:"env"`
	// Ingresses contains public HTTPS routes with automatically managed certificates.
	Ingresses []Ingress `json:"ingresses"`
	// Predeploy defines the app's optional process that gates the environment rollout.
	Predeploy Predeploy `json:"predeploy"`
	// Command defines the executable and quoted arguments; empty uses image defaults.
	Command string `json:"command"`
	// Checks controls startup health and readiness to receive traffic.
	Checks Checks `json:"checks"`
	// Sqlite controls persistent embedded database storage and continuous backups.
	Sqlite SqliteSettings `json:"sqlite"`
}

// AppPatch updates one named app. Pointers are necessary to distinguish omitted
// fields from explicit zero values, disabled checks, and list clearing.
type AppPatch struct {
	// Name selects the app to update or create and is limited to 50 characters.
	Name string `json:"name"`
	// Image replaces the image when supplied; a new app requires an image.
	Image *string `json:"image,omitempty"`
	// Port changes the listener port; zero disables networking.
	Port *int `json:"port,omitempty"`
	// Replicas changes the instance count; zero intentionally scales down.
	Replicas *int `json:"replicas,omitempty"`
	// Env changes the config reference; omission retains the existing selection.
	Env *EnvReference `json:"env,omitempty"`
	// Ingresses upserts named routes; an empty list clears all routes.
	Ingresses *[]Ingress `json:"ingresses,omitempty"`
	// Predeploy changes supplied task fields; an empty command disables the task.
	Predeploy *PredeployPatch `json:"predeploy,omitempty"`
	// Command replaces the complete command; empty restores image defaults.
	Command *string `json:"command,omitempty"`
	// Checks changes only supplied healthcheck fields.
	Checks *ChecksPatch `json:"checks,omitempty"`
	// Sqlite changes storage and backups without exposing filesystem configuration.
	Sqlite *SqlitePatch `json:"sqlite,omitempty"`
}

// EnvReference selects a named, independently versioned secret group.
type EnvReference struct {
	// Name identifies the configuration; empty selects the app's target default.
	Name string `json:"name"`
	// Version defaults to latest when a reference is supplied and is pinned during planning.
	// Deployments may use the latest or up to ten previous versions.
	Version string `json:"version"`
}

// Variable is a secret group value identified by name.
type Variable struct {
	// Name is the environment variable name.
	Name string `json:"name"`
	// Value is the literal environment value, including an empty string.
	Value string `json:"value"`
}

// EnvSnapshot binds an application to a pinned configuration version or a live database producer.
type EnvSnapshot struct {
	// PostgresID identifies a live managed database binding; values contain key names only.
	PostgresID string `json:"postgresId,omitempty"`
	// AppName identifies the application consuming these values.
	AppName string `json:"appName"`
	// ConfigName identifies the named secret group.
	ConfigName string `json:"configName"`
	// Store names the target cluster's configured ClusterSecretStore.
	Store string `json:"store"`
	// Path selects the KV v2 document within the store's configured mount.
	Path string `json:"path"`
	// Version pins the document version rather than following future edits.
	Version int `json:"version"`
	// SnapshotID identifies the saved snapshot independently of its vault version.
	SnapshotID string `json:"snapshotId"`
	// Values carries the published keyset for Secret readiness checks; app readback
	// uses the config identity and does not need the raw values.
	Values []Variable `json:"values"`
}

// Predeploy defines a finite process run with the parent app's image and env.
// A nonempty command must finish before any application resources change.
type Predeploy struct {
	// Command defines the executable and quoted arguments; empty disables predeploy.
	Command string `json:"command"`
	// TimeoutSeconds bounds the entire Job, including scheduling and retries.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// BackoffLimit bounds failed Pod retries; zero disables retries.
	BackoffLimit int `json:"backoffLimit"`
}

// PredeployPatch changes the app's task. Pointers distinguish omission from an
// explicit zero retry budget or an empty command that disables execution.
type PredeployPatch struct {
	// Command replaces the complete process command when supplied.
	Command *string `json:"command,omitempty"`
	// TimeoutSeconds overrides the Job's total execution deadline.
	TimeoutSeconds *int `json:"timeoutSeconds,omitempty"`
	// BackoffLimit overrides the number of failed Pod retries.
	BackoffLimit *int `json:"backoffLimit,omitempty"`
}

// Checks groups the complete startup and readiness healthcheck definitions.
type Checks struct {
	// Readiness determines when the app can receive Service traffic.
	Readiness Check `json:"readiness"`
	// Startup allows the app time to become healthy after starting.
	Startup Check `json:"startup"`
}

// ChecksPatch updates healthchecks independently. Pointers preserve omitted
// checks so changing readiness cannot reset startup settings.
type ChecksPatch struct {
	// Readiness changes only supplied readiness fields.
	Readiness *CheckPatch `json:"readiness,omitempty"`
	// Startup changes only supplied startup fields.
	Startup *CheckPatch `json:"startup,omitempty"`
}

// Ingress is a named HTTPS route with DNS and certificates managed by the platform.
type Ingress struct {
	// Name identifies this route within the app.
	Name string `json:"name"`
	// Host is the DNS host accepted by the route.
	Host string `json:"host"`
	// Path is the HTTP prefix and defaults to a slash.
	Path string `json:"path"`
}

// Check contains a resolved HTTP healthcheck, including its execution budget.
type Check struct {
	// Enabled controls whether Kubernetes receives this probe.
	Enabled bool `json:"enabled"`
	// Path is the HTTP endpoint and defaults to /healthz.
	Path string `json:"path"`
	// Port is the numeric HTTP port and defaults to the app's listener port.
	Port int `json:"port"`
	// PeriodSeconds is the interval between probe attempts.
	PeriodSeconds int `json:"periodSeconds"`
	// TimeoutSeconds bounds one HTTP probe attempt.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// FailureThreshold is the number of failed attempts before failure.
	FailureThreshold int `json:"failureThreshold"`
}

// CheckPatch updates individual healthcheck fields. Pointers are necessary to
// preserve omitted settings while allowing a check to be explicitly disabled.
type CheckPatch struct {
	// Enabled overrides whether the probe is emitted.
	Enabled *bool `json:"enabled,omitempty"`
	// Path overrides the HTTP endpoint.
	Path *string `json:"path,omitempty"`
	// Port overrides the HTTP probe port independently of the Service port.
	Port *int `json:"port,omitempty"`
	// PeriodSeconds overrides the interval between attempts.
	PeriodSeconds *int `json:"periodSeconds,omitempty"`
	// TimeoutSeconds overrides the per-attempt timeout.
	TimeoutSeconds *int `json:"timeoutSeconds,omitempty"`
	// FailureThreshold overrides the number of tolerated failed attempts.
	FailureThreshold *int `json:"failureThreshold,omitempty"`
}

// ApplicationState is the accepted configuration and its saved Kubernetes artifacts for one space.
type ApplicationState struct {
	// Configuration contains all apps with defaults and pinned env references expanded.
	Configuration Configuration `json:"configuration"`
	// Manifests contains the exact artifacts from the latest accepted apply, including failed or queued rollouts.
	Manifests []map[string]any `json:"manifests"`
}

// Rendered contains the immutable artifacts needed to execute a saved plan.
type Rendered struct {
	// Values is the complete Helm values document used during rendering.
	Values map[string]any `json:"values"`
	// Manifests contains Kubernetes resources in deterministic order.
	Manifests []map[string]any `json:"manifests"`
	// RendererVersion identifies the embedded chart that produced the artifacts.
	RendererVersion string `json:"rendererVersion"`
	// EnvSnapshots carries trusted resolution context without embedding values in
	// Helm values or Kubernetes manifests. It is not part of the rendering JSON.
	EnvSnapshots []EnvSnapshot `json:"-"`
}
