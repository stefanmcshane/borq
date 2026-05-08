package reconciler

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	TypeServiceAccount        = "ServiceAccount"
	TypeClusterRole           = "ClusterRole"
	TypeClusterRoleBinding    = "ClusterRoleBinding"
	TypeConfigMap             = "ConfigMap"
	TypeService               = "Service"
	TypePersistentVolumeClaim = "PersistentVolumeClaim"
	TypeDeployment            = "Deployment"
	TypeStatefulSet           = "StatefulSet"
	TypeSecret                = "Secret"
)

// DiffPatch is a single RFC 6902 JSON Patch operation.
type DiffPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// ResourceDiff describes how a single resource differs.
type ResourceDiff struct {
	Type      string      `json:"type"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Changed   bool        `json:"changed"`
	Patch     []DiffPatch `json:"patch,omitempty"`
}

// Resource is a single unit of reconciliation.
type Resource struct {
	Type      string
	Namespace string
	Name      string
	Desired   any
	Actual    any
}

// Plan describes everything a component manages.
type Plan struct {
	// ApplyOrder defines the order in which resource types are applied.
	// Resources in Desired are applied in this order; Destroy reverses it.
	ApplyOrder []string
	// Desired is the set of resources the component wants to exist.
	Desired []Resource
	// Current discovers the live state of the component's resources.
	// It returns the same resource types that Desired declares so the
	// reconciler can diff them. The handler owns all discovery logic —
	// the reconciler only calls this function and diffs the results.
	Current func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error)
}

// ReconcileResult is the API response from reconciliation.
type ReconcileResult struct {
	Diffs   []ResourceDiff `json:"diffs"`
	Applied bool           `json:"applied"`
}

// ReconcileError describes a resource reconciliation failure with
// structured fields for logging — no field interpolation into strings.
type ReconcileError struct {
	Type      string
	Namespace string
	Name      string
	Op        string
	Err       error
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("%s %s/%s: %v", e.Op, e.Type, e.Name, e.Err)
}

func (e *ReconcileError) Unwrap() error {
	return e.Err
}

// Fields returns structured logger fields for this error.
func (e *ReconcileError) Fields() []logger.Field {
	return []logger.Field{
		logger.String("resource_type", e.Type),
		logger.String("resource_namespace", e.Namespace),
		logger.String("resource_name", e.Name),
		logger.String("op", e.Op),
	}
}
