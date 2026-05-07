package managementreconcile

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// Reconciler reconciles a ComponentPlan by discovering the current cluster state,
// computing diffs, and applying changes through the component's handler.
type Reconciler struct {
	conn kubernetes.Connection
	log  logger.Logger
}

// NewReconciler creates a reconciler that operates on a single
// Kubernetes connection. It is returned by value — the struct is lightweight
// and methods use value receivers.
func NewReconciler(conn kubernetes.Connection, log logger.Logger) Reconciler {
	return Reconciler{conn: conn, log: log}
}

// GetActual discovers the current cluster state for the component using
// label-based List queries. Only the resource kinds declared in
// plan.NamespaceScopedKinds and plan.ClusterScopedKinds are queried.
func (r Reconciler) GetActual(ctx context.Context, plan ComponentPlan) (Infrastructure, error) {
	infra := Infrastructure{Resources: make([]ResourceState, 0)}

	for _, ns := range plan.Namespaces {
		for _, kind := range plan.NamespaceScopedKinds {
			resources, err := r.listNamespaceScoped(ctx, ns, plan.Selector, kind)
			if err != nil {
				return Infrastructure{}, err
			}
			infra.Resources = append(infra.Resources, resources...)
		}
	}

	for _, kind := range plan.ClusterScopedKinds {
		resources, err := r.listClusterScoped(ctx, plan.Selector, kind)
		if err != nil {
			return Infrastructure{}, err
		}
		infra.Resources = append(infra.Resources, resources...)
	}

	return infra, nil
}

func (r Reconciler) listNamespaceScoped(ctx context.Context, namespace, selector string, kind Kind) ([]ResourceState, error) {
	switch kind {
	case KindService:
		items, err := r.conn.Services().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list services: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindService, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindConfigMap:
		items, err := r.conn.ConfigMaps().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list config maps: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindConfigMap, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindServiceAccount:
		items, err := r.conn.ServiceAccounts().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list service accounts: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindServiceAccount, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindPersistentVolumeClaim:
		items, err := r.conn.PersistentVolumeClaims().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list persistent volume claims: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindPersistentVolumeClaim, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindDeployment:
		items, err := r.conn.Deployments().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindDeployment, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindStatefulSet:
		items, err := r.conn.StatefulSets().List(ctx, namespace, selector)
		if err != nil {
			return nil, fmt.Errorf("list statefulsets: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindStatefulSet, Namespace: namespace, Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	default:
		return nil, fmt.Errorf("unknown namespace-scoped kind %q", kind)
	}
}

func (r Reconciler) listClusterScoped(ctx context.Context, selector string, kind Kind) ([]ResourceState, error) {
	switch kind {
	case KindClusterRole:
		items, err := r.conn.ClusterRoles().List(ctx, selector)
		if err != nil {
			return nil, fmt.Errorf("list cluster roles: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindClusterRole, Namespace: "", Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	case KindClusterRoleBinding:
		items, err := r.conn.ClusterRoleBindings().List(ctx, selector)
		if err != nil {
			return nil, fmt.Errorf("list cluster role bindings: %w", err)
		}
		resources := make([]ResourceState, len(items))
		for i := range items {
			resources[i] = ResourceState{Kind: KindClusterRoleBinding, Namespace: "", Name: items[i].Name, Actual: items[i]}
		}
		return resources, nil

	default:
		return nil, fmt.Errorf("unknown cluster-scoped kind %q", kind)
	}
}

// Reconcile aligns actual state with the plan's expected resources. Creates missing
// resources, updates changed ones, and deletes extra ones. When dryRun is true no
// mutations are performed — only the diffs are returned.
func (r Reconciler) Reconcile(ctx context.Context, actual Infrastructure, plan ComponentPlan, handler ComponentHandler, dryRun bool) (Infrastructure, ReconcileResult, error) {
	result := ReconcileResult{Diffs: make([]ResourceDiff, 0)}
	expected := Infrastructure{Resources: plan.Resources, ApplyOrder: plan.ApplyOrder}
	actualByKey := buildResourceLookup(&actual)

	for _, kind := range expected.ApplyOrder {
		for _, res := range expected.Resources {
			if res.Kind != kind {
				continue
			}
			diff := r.reconcileResource(ctx, res, actualByKey, handler, dryRun)
			if diff.Changed {
				result.Diffs = append(result.Diffs, diff)
			}
		}
	}

	for _, res := range actual.Resources {
		key := resourceKey(res.Kind, res.Name)
		if _, found := resourceInExpected(key, &expected); found || res.Actual == nil {
			continue
		}
		diff := ResourceDiff{
			Kind:      string(res.Kind),
			Namespace: res.Namespace,
			Name:      res.Name,
			Changed:   true,
			Patch:     []DiffPatch{{Op: "remove", Path: ""}},
		}
		result.Diffs = append(result.Diffs, diff)
		if !dryRun {
			if err := handler.DeleteResource(ctx, r.conn, res); err != nil {
				return Infrastructure{}, result, fmt.Errorf("destroy %s/%s: %w", res.Kind, res.Name, err)
			}
		}
	}

	result.Applied = !dryRun && len(result.Diffs) > 0
	return expected, result, nil
}

func (r Reconciler) reconcileResource(ctx context.Context, res ResourceState, actualByKey map[string]ResourceState, handler ComponentHandler, dryRun bool) ResourceDiff {
	key := resourceKey(res.Kind, res.Name)
	actualRes, exists := actualByKey[key]

	if !exists || actualRes.Actual == nil {
		diff := ResourceDiff{
			Kind:      string(res.Kind),
			Namespace: res.Namespace,
			Name:      res.Name,
			Changed:   true,
			Patch:     addPatchFor(res.Desired),
		}
		if !dryRun {
			r.log.Info("creating_resource", logger.WithFields(
				logger.String("kind", string(res.Kind)),
				logger.String("name", res.Name),
			))
			if err := handler.ApplyResource(ctx, r.conn, res); err != nil {
				diff.Changed = false
			}
		}
		return diff
	}

	patch := computePatch(actualRes.Actual, res.Desired)
	if len(patch) == 0 {
		return ResourceDiff{Kind: string(res.Kind), Namespace: res.Namespace, Name: res.Name}
	}

	diff := ResourceDiff{
		Kind:      string(res.Kind),
		Namespace: res.Namespace,
		Name:      res.Name,
		Changed:   true,
		Patch:     patch,
	}
	if !dryRun {
		r.log.Info("updating_resource", logger.WithFields(
			logger.String("kind", string(res.Kind)),
			logger.String("name", res.Name),
		))
		if err := handler.ApplyResource(ctx, r.conn, res); err != nil {
			diff.Changed = false
		}
	}
	return diff
}

// Destroy removes all resources described by the plan in reverse apply order.
func (r Reconciler) Destroy(ctx context.Context, plan ComponentPlan, handler ComponentHandler) error {
	order := plan.ApplyOrder
	for i := len(order) - 1; i >= 0; i-- {
		kind := order[i]
		for _, res := range plan.Resources {
			if res.Kind != kind {
				continue
			}
			r.log.Info("destroying_resource", logger.WithFields(
				logger.String("kind", string(res.Kind)),
				logger.String("name", res.Name),
			))
			if err := handler.DeleteResource(ctx, r.conn, res); err != nil {
				return err
			}
		}
	}
	return nil
}
