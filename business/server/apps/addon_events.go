package apps

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// EventHistory contains bounded workload activity, independent of log collection.
type EventHistory struct {
	// Start and End delimit the requested history.
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Events combines durable operations and collected Kubernetes lifecycle changes.
	Events []ApplicationEvent `json:"events"`
	// EventsNotice explains incomplete history without hiding available events.
	EventsNotice string `json:"eventsNotice"`
}

// GetAddonEvents correlates only this addon's operations and rendered workloads.
// It retains installation history when pod logs are disabled or Loki is unavailable.
func (manager Manager) GetAddonEvents(ctx context.Context, input TelemetryInput) (EventHistory, error) {
	addon, err := manager.GetAddon(ctx, input.Organization, input.Space, input.Application)
	if err != nil {
		return EventHistory{}, err
	}
	space, err := manager.GetSpace(ctx, input.Organization, input.Space)
	if err != nil {
		return EventHistory{}, err
	}
	start, end, _, err := telemetryWindow(input)
	if err != nil {
		return EventHistory{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result := EventHistory{Start: start, End: end, Events: []ApplicationEvent{}}
	tasks, err := manager.database.List(ctx, "addon-task")
	if err != nil {
		return EventHistory{}, err
	}
	for _, record := range tasks {
		var task addonTask
		if err := json.Unmarshal(record.Value, &task); err != nil {
			return EventHistory{}, err
		}
		if task.Key != addonKey(input.Organization, input.Space, input.Application) {
			continue
		}
		operation, err := manager.GetAddonOperation(ctx, record.Key)
		if err != nil {
			result.EventsNotice = "Installation history is partially unavailable."
			continue
		}
		action := "Installation or upgrade"
		if task.Remove {
			action = "Uninstall"
		}
		if !operation.StartedAt.Before(start) && !operation.StartedAt.After(end) {
			result.Events = append(result.Events, ApplicationEvent{Timestamp: operation.StartedAt, Kind: "deployment", Message: action + " accepted", Deployment: operation.ID})
		}
		if (operation.Status == "succeeded" || operation.Status == "failed") && !operation.UpdatedAt.Before(start) && !operation.UpdatedAt.After(end) {
			result.Events = append(result.Events, ApplicationEvent{Timestamp: operation.UpdatedAt, Kind: operation.Status, Message: action + " " + operation.Status + " · " + operation.Message, Deployment: operation.ID})
		}
	}
	observed, notice := manager.addonKubernetesEvents(ctx, input, addon, space, start, end)
	result.Events = append(result.Events, observed...)
	result.EventsNotice = strings.TrimSpace(result.EventsNotice + " " + notice)
	slices.SortStableFunc(result.Events, func(a, b ApplicationEvent) int { return a.Timestamp.Compare(b.Timestamp) })
	return result, nil
}

func (manager Manager) addonKubernetesEvents(ctx context.Context, input TelemetryInput, addon Addon, space Space, start, end time.Time) ([]ApplicationEvent, string) {
	if manager.runtime.observability.LokiUrl == "" {
		return nil, "Kubernetes lifecycle history is unavailable."
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", input.Organization+"/"+space.Cluster)
	if err != nil {
		return nil, "Kubernetes lifecycle history is temporarily unavailable."
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return nil, "Kubernetes lifecycle history is temporarily unavailable."
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: cluster.KubeconfigPath})
	if err != nil {
		return nil, "Kubernetes lifecycle history is temporarily unavailable."
	}
	defer releases.Close()
	resources, err := releases.ResourceHistory(ctx, addon.Namespace, addon.ReleaseName, stableId("addon/"+addonKey(input.Organization, input.Space, input.Application)))
	if err != nil {
		return nil, "Kubernetes lifecycle history is temporarily unavailable. Check the cluster connection."
	}
	return manager.kubernetesEvents(ctx, stableId(input.Organization+"/"+space.Cluster), addon.Namespace, addonEventObjects(resources, addon.Namespace), start, end)
}

func addonEventObjects(resources []helm.ResourceIdentity, namespace string) []kubernetesEventMatch {
	objects := []kubernetesEventMatch{}
	appendObject := func(kind, pattern string) {
		object := kubernetesEventMatch{kind: kind, pattern: pattern}
		if !slices.Contains(objects, object) {
			objects = append(objects, object)
		}
	}
	for _, resource := range resources {
		if resource.Namespace != namespace {
			continue
		}
		name := regexp.QuoteMeta(resource.Name)
		appendObject(resource.Kind, name)
		switch resource.Kind {
		case "Deployment":
			appendObject("ReplicaSet", name+`-[a-z0-9]{8,10}`)
			appendObject("Pod", name+`-[a-z0-9]{8,10}-[a-z0-9]{5}`)
		case "StatefulSet":
			appendObject("Pod", name+`-[0-9]+`)
		case "DaemonSet", "ReplicaSet", "ReplicationController", "Job":
			appendObject("Pod", name+`-[a-z0-9]{5}`)
		case "CronJob":
			appendObject("Job", name+`-[0-9]{8,11}`)
			appendObject("Pod", name+`-[0-9]{8,11}-[a-z0-9]{5}`)
		}
	}
	return objects
}
