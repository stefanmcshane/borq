package managementreconcile

// Kind identifies a Kubernetes resource type for ordering and dispatch.
type Kind string

const (
	KindServiceAccount        Kind = "ServiceAccount"
	KindClusterRole           Kind = "ClusterRole"
	KindClusterRoleBinding    Kind = "ClusterRoleBinding"
	KindConfigMap             Kind = "ConfigMap"
	KindService               Kind = "Service"
	KindPersistentVolumeClaim Kind = "PersistentVolumeClaim"
	KindDeployment            Kind = "Deployment"
	KindStatefulSet           Kind = "StatefulSet"
)

// DiffPatch is a single RFC 6902 JSON Patch operation.
type DiffPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// ResourceDiff describes how a single resource differs.
type ResourceDiff struct {
	Kind      string      `json:"kind"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Changed   bool        `json:"changed"`
	Patch     []DiffPatch `json:"patch,omitempty"`
}

// ResourceState captures one resource's identity and both states.
type ResourceState struct {
	Kind      Kind
	Namespace string
	Name      string
	Desired   any
	Actual    any
}

// Infrastructure is the complete K8s state of a managed component.
type Infrastructure struct {
	Resources  []ResourceState
	ApplyOrder []Kind
	Labels     map[string]string
}

// ReconcileResult is the API response from reconciliation.
type ReconcileResult struct {
	Diffs   []ResourceDiff `json:"diffs"`
	Applied bool           `json:"applied"`
}
