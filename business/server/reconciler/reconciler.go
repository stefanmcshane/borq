package reconciler

import (
	"context"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// Reconciler reconciles a Plan by discovering the current state via
// plan.Current, computing diffs against plan.Desired, and applying
// changes through the component's handler.
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

// Reconcile discovers the live state via plan.Current, diffs against
// plan.Desired, creates/updates missing or changed resources, and
// deletes resources not in the plan. When dryRun is true no mutations
// are performed — only the diffs are returned.
func (r Reconciler) Reconcile(ctx context.Context, plan Plan, handler ComponentHandler, dryRun bool) (ReconcileResult, error) {
	result := ReconcileResult{Diffs: make([]ResourceDiff, 0)}

	r.log.Info("reconcile_start", logger.WithFields(
		logger.String("handler", handler.Name()),
		logger.Bool("dry_run", dryRun),
	))

	actual, err := plan.Current(ctx, r.conn)
	if err != nil {
		r.log.Error("reconcile_current_failed", logger.WithFields(
			logger.String("handler", handler.Name()),
			logger.String("error", err.Error()),
		))
		return result, fmt.Errorf("discover current state: %w", err)
	}

	r.log.Info("reconcile_current_discovered", logger.WithFields(
		logger.String("handler", handler.Name()),
		logger.Int("actual_count", len(actual)),
		logger.Int("desired_count", len(plan.Desired)),
	))

	actualByKey := buildResourceLookup(actual)

	var created, updated, skipped int
	for _, resType := range plan.ApplyOrder {
		for _, res := range plan.Desired {
			if res.Type != resType {
				continue
			}
			diff, err := r.reconcileResource(ctx, res, actualByKey, handler, dryRun)
			if err != nil {
				r.log.Error("reconcile_resource_failed", logger.WithFields(
					logger.String("handler", handler.Name()),
					logger.String("type", res.Type),
					logger.String("namespace", res.Namespace),
					logger.String("name", res.Name),
					logger.String("error", err.Error()),
				))
				return result, &ReconcileError{
					Type: res.Type, Namespace: res.Namespace, Name: res.Name,
					Op: "reconcile", Err: err,
				}
			}
			if diff.Changed {
				op := "update"
				key := resourceKey(res.Type, res.Name)
				if _, exists := actualByKey[key]; !exists || actualByKey[key].Actual == nil {
					op = "create"
				}
				r.log.Info("reconcile_resource_applied", logger.WithFields(
					logger.String("handler", handler.Name()),
					logger.String("op", op),
					logger.String("type", diff.Type),
					logger.String("namespace", diff.Namespace),
					logger.String("name", diff.Name),
					logger.Int("patch_count", len(diff.Patch)),
				))
				if op == "create" {
					created++
				} else {
					updated++
				}
				result.Diffs = append(result.Diffs, diff)
			} else {
				skipped++
				r.log.Debug("reconcile_resource_skipped", logger.WithFields(
					logger.String("handler", handler.Name()),
					logger.String("type", res.Type),
					logger.String("namespace", res.Namespace),
					logger.String("name", res.Name),
				))
			}
		}
	}

	var deleted int
	for _, res := range actual {
		key := resourceKey(res.Type, res.Name)
		if _, found := resourceInExpected(key, plan.Desired); found || res.Actual == nil {
			continue
		}
		diff := ResourceDiff{
			Type:      res.Type,
			Namespace: res.Namespace,
			Name:      res.Name,
			Changed:   true,
			Patch:     []DiffPatch{{Op: "remove", Path: ""}},
		}
		result.Diffs = append(result.Diffs, diff)
		if !dryRun {
			r.log.Info("reconcile_resource_deleted", logger.WithFields(
				logger.String("handler", handler.Name()),
				logger.String("type", res.Type),
				logger.String("namespace", res.Namespace),
				logger.String("name", res.Name),
			))
			if err := handler.DeleteResource(ctx, r.conn, res); err != nil {
				r.log.Error("reconcile_delete_failed", logger.WithFields(
					logger.String("handler", handler.Name()),
					logger.String("type", res.Type),
					logger.String("namespace", res.Namespace),
					logger.String("name", res.Name),
					logger.String("error", err.Error()),
				))
				return result, &ReconcileError{
					Type: res.Type, Namespace: res.Namespace, Name: res.Name,
					Op: "destroy", Err: err,
				}
			}
			deleted++
		}
	}

	result.Applied = !dryRun && len(result.Diffs) > 0
	r.log.Info("reconcile_complete", logger.WithFields(
		logger.String("handler", handler.Name()),
		logger.Bool("applied", result.Applied),
		logger.Int("created", created),
		logger.Int("updated", updated),
		logger.Int("deleted", deleted),
		logger.Int("skipped", skipped),
		logger.Int("diffs", len(result.Diffs)),
	))
	return result, nil
}

func (r Reconciler) reconcileResource(ctx context.Context, res Resource, actualByKey map[string]Resource, handler ComponentHandler, dryRun bool) (ResourceDiff, error) {
	key := resourceKey(res.Type, res.Name)
	actualRes, exists := actualByKey[key]

	if !exists || actualRes.Actual == nil {
		r.log.Debug("reconcile_resource_missing", logger.WithFields(
			logger.String("handler", handler.Name()),
			logger.String("type", res.Type),
			logger.String("name", res.Name),
			logger.String("namespace", res.Namespace),
		))
		diff := ResourceDiff{
			Type:      res.Type,
			Namespace: res.Namespace,
			Name:      res.Name,
			Changed:   true,
			Patch:     addPatchFor(res.Desired),
		}
		if !dryRun {
			if err := handler.ApplyResource(ctx, r.conn, res); err != nil {
				return diff, err
			}
		}
		return diff, nil
	}

	r.log.Debug("reconcile_resource_checking_diff", logger.WithFields(
		logger.String("handler", handler.Name()),
		logger.String("type", res.Type),
		logger.String("name", res.Name),
	))
	patch := computePatch(actualRes.Actual, res.Desired)
	if len(patch) == 0 {
		r.log.Debug("reconcile_resource_unchanged", logger.WithFields(
			logger.String("handler", handler.Name()),
			logger.String("type", res.Type),
			logger.String("name", res.Name),
		))
		return ResourceDiff{Type: res.Type, Namespace: res.Namespace, Name: res.Name}, nil
	}

	r.log.Debug("reconcile_resource_changed", logger.WithFields(
		logger.String("handler", handler.Name()),
		logger.String("type", res.Type),
		logger.String("name", res.Name),
		logger.Int("patch_count", len(patch)),
	))
	diff := ResourceDiff{
		Type:      res.Type,
		Namespace: res.Namespace,
		Name:      res.Name,
		Changed:   true,
		Patch:     patch,
	}
	if !dryRun {
		if err := handler.ApplyResource(ctx, r.conn, res); err != nil {
			return diff, err
		}
	}
	return diff, nil
}

// Destroy removes all resources described by the plan in reverse apply order.
func (r Reconciler) Destroy(ctx context.Context, plan Plan, handler ComponentHandler) error {
	order := plan.ApplyOrder
	for i := len(order) - 1; i >= 0; i-- {
		resType := order[i]
		for _, res := range plan.Desired {
			if res.Type != resType {
				continue
			}
			r.log.Info("destroying_resource", logger.WithFields(
				logger.String("type", res.Type),
				logger.String("name", res.Name),
			))
			if err := handler.DeleteResource(ctx, r.conn, res); err != nil {
				return err
			}
		}
	}
	return nil
}
