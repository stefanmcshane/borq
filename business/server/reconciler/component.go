package reconciler

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Management resources no longer share the namespace created by application controllers.
func ensureComponentNamespace(ctx context.Context, conn kubernetes.Connection, namespace string) error {
	_, err := conn.Namespaces().Get(ctx, namespace)
	if apierrors.IsNotFound(err) {
		return conn.Namespaces().Apply(ctx, kubernetes.Namespace{Name: namespace})
	}
	return err
}

// ComponentHandler describes a managed component that can be reconciled.
// Every component (openbao, whoami, etc.) implements this interface.
type ComponentHandler interface {
	Name() string
	// DefaultConfig returns the default configuration as a pointer to a
	// handler-specific struct (e.g. *OpenBaoConfig). The pointer is required so
	// that json.Decode can populate fields directly — a value type wrapped
	// in any is unaddressable and causes decode to produce map[string]any
	// instead, which panics in Plan's type assertion.
	DefaultConfig() any
	Plan(config any) Plan
	ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error
	DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error
}

// CoreApplier handles ApplyResource and DeleteResource for all standard
// Kubernetes resource types (Service, Deployment, ConfigMap, etc.).
// Components that only use built-in K8s resources embed CoreApplier and
// write zero apply/delete code. Components with custom resource types
// (like OpenBaoInit) override the methods they need.
type CoreApplier struct{}

func (CoreApplier) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	switch res.Type {
	case TypeServiceAccount:
		return conn.ServiceAccounts().Apply(ctx, res.Desired.(kubernetes.ServiceAccount))
	case TypeClusterRole:
		return conn.ClusterRoles().Apply(ctx, res.Desired.(kubernetes.ClusterRole))
	case TypeClusterRoleBinding:
		return conn.ClusterRoleBindings().Apply(ctx, res.Desired.(kubernetes.ClusterRoleBinding))
	case TypeConfigMap:
		return conn.ConfigMaps().Apply(ctx, res.Desired.(kubernetes.ConfigMap))
	case TypeService:
		return conn.Services().Apply(ctx, res.Desired.(kubernetes.Service))
	case TypePersistentVolumeClaim:
		return conn.PersistentVolumeClaims().Apply(ctx, res.Desired.(kubernetes.PersistentVolumeClaim))
	case TypeDeployment:
		return conn.Deployments().Apply(ctx, res.Desired.(kubernetes.Deployment))
	case TypeStatefulSet:
		return conn.StatefulSets().Apply(ctx, res.Desired.(kubernetes.StatefulSet))
	case TypeSecret:
		return conn.Secrets().Apply(ctx, res.Desired.(kubernetes.Secret))
	default:
		return fmt.Errorf("core applier: unknown type %q", res.Type)
	}
}

func (CoreApplier) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	switch res.Type {
	case TypeServiceAccount:
		return conn.ServiceAccounts().Delete(ctx, res.Namespace, res.Name)
	case TypeClusterRole:
		return conn.ClusterRoles().Delete(ctx, res.Name)
	case TypeClusterRoleBinding:
		return conn.ClusterRoleBindings().Delete(ctx, res.Name)
	case TypeConfigMap:
		return conn.ConfigMaps().Delete(ctx, res.Namespace, res.Name)
	case TypeService:
		return conn.Services().Delete(ctx, res.Namespace, res.Name)
	case TypePersistentVolumeClaim:
		return conn.PersistentVolumeClaims().Delete(ctx, res.Namespace, res.Name)
	case TypeDeployment:
		return conn.Deployments().Delete(ctx, res.Namespace, res.Name)
	case TypeStatefulSet:
		return conn.StatefulSets().Delete(ctx, res.Namespace, res.Name)
	case TypeSecret:
		return conn.Secrets().Delete(ctx, res.Namespace, res.Name)
	default:
		return fmt.Errorf("core applier: unknown type %q", res.Type)
	}
}
