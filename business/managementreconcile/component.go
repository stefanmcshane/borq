package managementreconcile

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

// ComponentHandler describes a managed component that can be reconciled.
// Every component (vault, whoami, etc.) implements this interface.
type ComponentHandler interface {
	Name() string
	// DefaultConfig returns the default configuration as a pointer to a
	// handler-specific struct (e.g. *VaultConfig). The pointer is required so
	// that json.Decode can populate fields directly — a value type wrapped
	// in any is unaddressable and causes decode to produce map[string]any
	// instead, which panics in Plan's type assertion.
	DefaultConfig() any
	Plan(config any) ComponentPlan
	ApplyResource(ctx context.Context, conn kubernetes.Connection, res ResourceState) error
	DeleteResource(ctx context.Context, conn kubernetes.Connection, res ResourceState) error
}

// ComponentPlan describes the full reconciliation target for a single component — what
// resources to discover, what to build, and in what order.
type ComponentPlan struct {
	Selector             string
	Namespaces           []string
	NamespaceScopedKinds []Kind
	ClusterScopedKinds   []Kind
	Resources            []ResourceState
	ApplyOrder           []Kind
}

// CoreApplier handles ApplyResource and DeleteResource for all core Kubernetes
// resource kinds (Service, Deployment, ConfigMap, etc.). Components that only
// use built-in k8s resources embed CoreApplier and write zero apply/delete code.
// Custom-operator components override the methods they need.
type CoreApplier struct{}

func (CoreApplier) ApplyResource(ctx context.Context, conn kubernetes.Connection, res ResourceState) error {
	switch res.Kind {
	case KindServiceAccount:
		return conn.ServiceAccounts().Apply(ctx, res.Desired.(kubernetes.ServiceAccount))
	case KindClusterRole:
		return conn.ClusterRoles().Apply(ctx, res.Desired.(kubernetes.ClusterRole))
	case KindClusterRoleBinding:
		return conn.ClusterRoleBindings().Apply(ctx, res.Desired.(kubernetes.ClusterRoleBinding))
	case KindConfigMap:
		return conn.ConfigMaps().Apply(ctx, res.Desired.(kubernetes.ConfigMap))
	case KindService:
		return conn.Services().Apply(ctx, res.Desired.(kubernetes.Service))
	case KindPersistentVolumeClaim:
		return conn.PersistentVolumeClaims().Apply(ctx, res.Desired.(kubernetes.PersistentVolumeClaim))
	case KindDeployment:
		return conn.Deployments().Apply(ctx, res.Desired.(kubernetes.Deployment))
	case KindStatefulSet:
		return conn.StatefulSets().Apply(ctx, res.Desired.(kubernetes.StatefulSet))
	default:
		return fmt.Errorf("core applier: unknown kind %q", res.Kind)
	}
}

func (CoreApplier) DeleteResource(ctx context.Context, conn kubernetes.Connection, res ResourceState) error {
	switch res.Kind {
	case KindServiceAccount:
		return conn.ServiceAccounts().Delete(ctx, res.Namespace, res.Name)
	case KindClusterRole:
		return conn.ClusterRoles().Delete(ctx, res.Name)
	case KindClusterRoleBinding:
		return conn.ClusterRoleBindings().Delete(ctx, res.Name)
	case KindConfigMap:
		return conn.ConfigMaps().Delete(ctx, res.Namespace, res.Name)
	case KindService:
		return conn.Services().Delete(ctx, res.Namespace, res.Name)
	case KindPersistentVolumeClaim:
		return conn.PersistentVolumeClaims().Delete(ctx, res.Namespace, res.Name)
	case KindDeployment:
		return conn.Deployments().Delete(ctx, res.Namespace, res.Name)
	case KindStatefulSet:
		return conn.StatefulSets().Delete(ctx, res.Namespace, res.Name)
	default:
		return fmt.Errorf("core applier: unknown kind %q", res.Kind)
	}
}
