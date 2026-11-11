package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// DeleteApp starts durable cleanup of one application's runtime resources, retaining SQLite data and secrets.
// The application remains accepted until cleanup succeeds; repeating a pending deletion returns the same operation.
func (manager Manager) DeleteApp(ctx context.Context, organization, space, name string) (Operation, error) {
	plan, existing, err := manager.appDeletionPlan(ctx, organization, space, name)
	if err != nil || existing.ID != "" {
		return existing, err
	}
	return manager.enqueuePlan(ctx, plan.ID)
}

func (manager Manager) appDeletionPlan(ctx context.Context, organization, space, name string) (Plan, Operation, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, space))
	if err != nil {
		return Plan{}, Operation{}, err
	}
	if !slices.ContainsFunc(stored.Space.Configuration.Apps, func(app App) bool { return app.Name == name }) {
		return Plan{}, Operation{}, ErrNotFound
	}
	if stored.ActiveOperation != "" {
		operation, err := manager.GetOperation(ctx, stored.ActiveOperation)
		if err != nil {
			return Plan{}, Operation{}, err
		}
		saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return Plan{}, Operation{}, err
		}
		if saved.DeleteApp == name {
			return Plan{}, operation, nil
		}
		return Plan{}, Operation{}, fmt.Errorf("%w: wait for the active operation to finish before deleting an application", ErrConflict)
	}
	if len(stored.Queue) > 0 {
		return Plan{}, Operation{}, fmt.Errorf("%w: wait for queued deployments to finish before deleting an application", ErrConflict)
	}
	connection, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+stored.Space.Cluster)
	if err != nil {
		return Plan{}, Operation{}, err
	}
	inventory, err := manager.appDeletionInventory(ctx, stored, name)
	if err != nil {
		return Plan{}, Operation{}, err
	}
	id, err := newId()
	if err != nil {
		return Plan{}, Operation{}, err
	}
	after := withoutApp(stored, name)
	difference, err := diffConfiguration(stored.Space.Configuration, after.Space.Configuration)
	if err != nil {
		return Plan{}, Operation{}, err
	}
	differences := []kubeapps.ResourceDiff{}
	for _, manifest := range inventory {
		differences = append(differences, kubeapps.ResourceDiff{ApiVersion: manifestText(manifest, "apiVersion"), Kind: manifestText(manifest, "kind"), Namespace: stored.Space.Namespace, Name: manifestName(manifest), Action: "delete"})
	}
	plan := Plan{ID: id, Space: stored.Space, BaseRevision: stored.Space.Revision, Before: stored.Space.Configuration, Configuration: after.Space.Configuration,
		ConfigurationDiff: difference, Values: map[string]any{}, RendererVersion: "application-deletion-v1", Manifests: after.Manifests, ResourceDiffs: differences, CreatedAt: time.Now().UTC()}
	write, err := documentWrite("plan", id, planRecord{Plan: plan, Connection: connection, DeleteApp: name, DeleteManifests: inventory, DeleteBaseRevision: record.Revision}, 0)
	if err != nil {
		return Plan{}, Operation{}, err
	}
	return plan, Operation{}, manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) appDeletionInventory(ctx context.Context, stored spaceRecord, name string) ([]map[string]any, error) {
	manifests := slices.Clone(stored.Manifests)
	records, err := manager.database.List(ctx, "operation")
	if err != nil {
		return nil, err
	}
	// Accepted history also owns earlier predeploy Jobs and pinned environment resources.
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return nil, err
		}
		if operation.SpaceID != stored.Space.ID {
			continue
		}
		saved, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, saved.Plan.Manifests...)
		manifests = append(manifests, saved.RetiredIngresses...)
	}
	inventory := []map[string]any{}
	seen := map[string]bool{}
	for _, manifest := range manifests {
		if manifestText(manifest, "metadata", "labels", "app.kubernetes.io/name") != name {
			continue
		}
		// SQLite recovery state, volumes and backup credentials remain available for reuse.
		if manifestText(manifest, "metadata", "labels", "borq.run/phase") == "sqlite" && manifest["kind"] != "CronJob" {
			continue
		}
		identity := manifestIdentity(manifest)
		if !seen[identity] {
			inventory = append(inventory, manifest)
			seen[identity] = true
		}
	}
	return inventory, nil
}

func withoutApp(stored spaceRecord, name string) spaceRecord {
	stored.Space.Configuration.Apps = slices.DeleteFunc(slices.Clone(stored.Space.Configuration.Apps), func(app App) bool { return app.Name == name })
	stored.Manifests = slices.DeleteFunc(slices.Clone(stored.Manifests), func(manifest map[string]any) bool {
		return manifestText(manifest, "metadata", "labels", "app.kubernetes.io/name") == name
	})
	stored.EnvSnapshots = slices.DeleteFunc(slices.Clone(stored.EnvSnapshots), func(snapshot EnvSnapshot) bool { return snapshot.AppName == name })
	stored.EnvSelectors = maps.Clone(stored.EnvSelectors)
	delete(stored.EnvSelectors, name)
	return stored
}

func (manager Manager) runAppDeletion(ctx context.Context, operation Operation, saved planRecord) {
	client, err := newClusterClient(saved.Connection)
	if err != nil {
		manager.failOperation(operation, client, nil, err)
		return
	}
	for {
		status, err := client.DeleteApplicationResources(ctx, saved.DeleteManifests, operation.SpaceID, saved.DeleteApp)
		if err != nil {
			manager.failOperation(operation, client, nil, err)
			return
		}
		operation.Resources = status.Resources
		operation.Status, operation.Message = "waiting", "Deleting "+saved.DeleteApp+"; waiting for its runtime resources to be removed."
		if status.State == "succeeded" {
			manager.finishOperation(operation, "succeeded", "Deleted "+saved.DeleteApp+". SQLite volumes, backups and secrets were retained.")
			return
		}
		if err := manager.saveOperation(ctx, operation); err != nil {
			manager.failOperation(operation, client, nil, err)
			return
		}
		if err := manager.waitForPoll(ctx); err != nil {
			manager.failOperation(operation, client, nil, err)
			return
		}
	}
}
