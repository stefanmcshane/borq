package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/observability"
)

// ApplicationDetails separates accepted configuration from current cluster health.
type ApplicationDetails struct {
	// Configuration is the accepted desired application, including queued updates.
	Configuration App `json:"configuration"`
	// State is healthy, deploying, degraded, stopped, or unavailable.
	State string `json:"state"`
	// Message explains health or why the cluster could not be observed.
	Message string `json:"message"`
	// Resources contains live resource readiness without secret values.
	Resources []kubeapps.ResourceStatus `json:"resources"`
}

// Deployment is an application-specific view of a durable space operation.
type Deployment struct {
	// Revision matches the runtime revision attached to logs for these settings.
	Revision string `json:"revision"`
	// Operation contains progress and a stable ID for subsequent polling.
	Operation Operation `json:"operation"`
	// Configuration is the application's configuration in this release.
	Configuration App `json:"configuration"`
}

// Metrics contains aggregate usage across application replicas in explicit units.
type Metrics struct {
	// Events annotates deployments and observed Kubernetes lifecycle changes.
	Events []ApplicationEvent `json:"events"`
	// EventsNotice reports partial or unavailable lifecycle history separately from usage.
	EventsNotice string `json:"eventsNotice"`
	// Start is the beginning of the requested history.
	Start time.Time `json:"start"`
	// End is the end of the requested history.
	End time.Time `json:"end"`
	// StepSeconds is the evaluation interval; missing points represent missing data.
	StepSeconds int `json:"stepSeconds"`
	// CpuCores is summed CPU usage in cores, averaged over the evaluation window.
	CpuCores []observability.Sample `json:"cpuCores"`
	// MemoryBytes is summed container working set in bytes, averaged over the evaluation window.
	MemoryBytes []observability.Sample `json:"memoryBytes"`
}

// Logs is a bounded snapshot of persisted application output.
type Logs struct {
	// Events contains deployment and lifecycle markers independent of the text filter.
	Events []ApplicationEvent `json:"events"`
	// EventsNotice reports partial or unavailable lifecycle history separately from output.
	EventsNotice string `json:"eventsNotice"`
	// Start and End delimit the requested history.
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Entries contains the newest matching lines in chronological order.
	Entries []observability.Entry `json:"entries"`
	// LimitReached means earlier matching lines may exist in the selected range.
	LimitReached bool `json:"limitReached"`
}

// TelemetryInput scopes bounded queries to an accepted application.
type TelemetryInput struct {
	// Organization, Space and Application select the application without accepting arbitrary queries.
	Organization, Space, Application string
	// Range is 30m, 1h, 12h, or 3d; omission selects 1h.
	Range string
	// End optionally freezes the query time for repeatable inspection.
	End time.Time
	// Search filters logs by a literal substring.
	Search string
	// Limit caps returned log lines; zero selects 200.
	Limit int
}

// GetApp observes live health; deployment history only bounds the readiness grace period.
func (manager Manager) GetApp(ctx context.Context, organization, space, name string) (ApplicationDetails, error) {
	stored, application, err := manager.applicationRecord(ctx, organization, space, name)
	if err != nil {
		return ApplicationDetails{}, err
	}
	result := ApplicationDetails{Configuration: application, State: "unavailable", Message: "Could not read live application health. Check the cluster connection.", Resources: []kubeapps.ResourceStatus{}}
	connection, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+stored.Space.Cluster)
	if err != nil {
		return result, err
	}
	client, err := newClusterClient(connection)
	if err != nil {
		return result, nil
	}
	manifests := []map[string]any{}
	for _, manifest := range stored.Manifests {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if labels["app.kubernetes.io/name"] == name && !isPredeployJob(manifest) {
			manifests = append(manifests, manifest)
		}
	}
	if len(manifests) == 0 {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	status, err := client.Status(ctx, manifests, stored.Space.ID)
	if err != nil {
		return result, nil
	}
	result.State, result.Message, result.Resources = "deploying", status.Message, status.Resources
	if status.State == "running" {
		failure, err := manager.appDeploymentFailure(ctx, organization, space, name)
		if err != nil {
			return ApplicationDetails{}, err
		}
		if failure != "" {
			result.State, result.Message = "degraded", failure+"; "+status.Message
		}
	}
	if status.State == "failed" {
		result.State = "degraded"
	}
	if status.State == "succeeded" {
		result.State = "healthy"
		if application.Replicas == 0 {
			result.State, result.Message = "stopped", "Scaled to zero. Increase replicas to start this application."
		}
	}
	return result, nil
}

func (manager Manager) appDeploymentFailure(ctx context.Context, organization, space, name string) (string, error) {
	deployments, err := manager.appDeployments(ctx, organization, space, name, 1)
	if err != nil || len(deployments) == 0 {
		return "", err
	}
	operation := deployments[0].Operation
	if operation.Status == "queued" {
		return "", nil
	}
	if operationFinished(operation.Status) && operation.Status != "succeeded" {
		return "deployment " + operation.Status + ": " + operation.Message, nil
	}
	// Compare the durable execution timestamp on each observation. Polling and
	// server restarts must not grant an unready application a new grace period.
	if !time.Now().Before(manager.operationDeadline(operation)) {
		return "rollout deadline exceeded", nil
	}
	return "", nil
}

// ListAppDeployments returns the newest twenty accepted operations that explicitly targeted this app.
func (manager Manager) ListAppDeployments(ctx context.Context, organization, space, name string) ([]Deployment, error) {
	return manager.appDeployments(ctx, organization, space, name, 20)
}

func (manager Manager) appDeployments(ctx context.Context, organization, space, name string, limit int) ([]Deployment, error) {
	stored, _, err := manager.applicationRecord(ctx, organization, space, name)
	if err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "operation")
	if err != nil {
		return nil, err
	}
	deployments := []Deployment{}
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
		if saved.DeleteApp != "" {
			continue
		}
		included := saved.Source.Apps == nil
		if saved.Source.Apps != nil {
			for _, update := range *saved.Source.Apps {
				if update.Name == name {
					included = true
				}
			}
		}
		if !included {
			continue
		}
		for _, application := range saved.Plan.Configuration.Apps {
			if application.Name == name {
				deployments = append(deployments, Deployment{Operation: operation, Configuration: application, Revision: applicationRevision(application)})
			}
		}
	}
	slices.SortFunc(deployments, func(left, right Deployment) int { return right.Operation.StartedAt.Compare(left.Operation.StartedAt) })
	if limit > 0 && len(deployments) > limit {
		deployments = deployments[:limit]
	}
	return deployments, nil
}

// RestartApp immediately requests a rollout restart of the existing deployment without changing desired configuration.
func (manager Manager) RestartApp(ctx context.Context, organization, space, name string) (kubeapps.ResourceStatus, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	stored, application, err := manager.applicationRecord(ctx, organization, space, name)
	if err != nil {
		return kubeapps.ResourceStatus{}, err
	}
	if stored.ActiveOperation != "" {
		operation, err := manager.GetOperation(ctx, stored.ActiveOperation)
		if err != nil {
			return kubeapps.ResourceStatus{}, err
		}
		if operation.Phase == "deleting" {
			return kubeapps.ResourceStatus{}, fmt.Errorf("%w: wait for application deletion to finish before restarting", ErrConflict)
		}
	}
	if application.Replicas == 0 {
		return kubeapps.ResourceStatus{}, fmt.Errorf("%w: increase replicas before restarting a stopped application", ErrInvalid)
	}
	connection, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+stored.Space.Cluster)
	if err != nil {
		return kubeapps.ResourceStatus{}, err
	}
	client, err := newClusterClient(connection)
	if err != nil {
		return kubeapps.ResourceStatus{}, err
	}
	status, err := client.RestartDeployment(ctx, kubeapps.RestartInput{Namespace: stored.Space.Namespace, Name: name, TargetID: stored.Space.ID})
	if errors.Is(err, kubeapps.ErrOwnershipConflict) || errors.Is(err, kubeapps.ErrRestartUnavailable) {
		return kubeapps.ResourceStatus{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return status, err
}

// PlanAppRollback restores one successful release as a new plan, retaining other applications.
// Existing deletion and environment-version guards remain in force.
func (manager Manager) PlanAppRollback(ctx context.Context, organization, space, name, deployment string) (Plan, error) {
	stored, _, err := manager.applicationRecord(ctx, organization, space, name)
	if err != nil {
		return Plan{}, err
	}
	operation, err := manager.GetOperation(ctx, deployment)
	if err != nil {
		return Plan{}, err
	}
	if operation.SpaceID != stored.Space.ID {
		return Plan{}, ErrNotFound
	}
	if operation.Status != "succeeded" {
		return Plan{}, fmt.Errorf("%w: only successful releases can be restored", ErrInvalid)
	}
	saved, err := manager.GetPlan(ctx, operation.PlanID)
	if err != nil {
		return Plan{}, err
	}
	for _, application := range saved.Configuration.Apps {
		if application.Name != name {
			continue
		}
		encoded, err := json.Marshal(application)
		if err != nil {
			return Plan{}, err
		}
		var update AppPatch
		if err := json.Unmarshal(encoded, &update); err != nil {
			return Plan{}, err
		}
		for _, current := range stored.Space.Configuration.Apps {
			if current.Name != name {
				continue
			}
			for _, route := range current.Ingresses {
				if !slices.ContainsFunc(application.Ingresses, func(previous Ingress) bool { return previous.Name == route.Name }) {
					return Plan{}, fmt.Errorf("%w: restoring this release would remove a route; resource deletion is not supported", ErrInvalid)
				}
			}
		}
		updates := []AppPatch{update}
		return manager.Plan(ctx, PlanInput{Organization: organization, Space: space, Patch: Patch{Apps: &updates}})
	}
	return Plan{}, ErrNotFound
}

func (manager Manager) applicationRecord(ctx context.Context, organization, space, name string) (spaceRecord, App, error) {
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, space))
	if err != nil {
		return spaceRecord{}, App{}, err
	}
	for _, application := range stored.Space.Configuration.Apps {
		if application.Name == name {
			return stored, application, nil
		}
	}
	return spaceRecord{}, App{}, ErrNotFound
}

// GetAppMetrics returns bounded CPU and memory history from management Prometheus.
func (manager Manager) GetAppMetrics(ctx context.Context, input TelemetryInput) (Metrics, error) {
	stored, _, err := manager.applicationRecord(ctx, input.Organization, input.Space, input.Application)
	if err != nil {
		return Metrics{}, err
	}
	// Deployment pod shape excludes predeploy Jobs using the same container name.
	selector := fmt.Sprintf(`{borq_cluster=%s,namespace=%s,container=%s,pod=~%s,job="borq-cadvisor"}`, strconv.Quote(stableId(input.Organization+"/"+stored.Space.Cluster)), strconv.Quote(stored.Space.Namespace), strconv.Quote(input.Application), strconv.Quote(input.Application+`-[^-]+-[^-]+`))
	result, err := manager.workloadMetrics(ctx, input, selector)
	if err != nil {
		return Metrics{}, err
	}
	result.Events, result.EventsNotice = manager.applicationEvents(ctx, input, stored, result.Start, result.End)
	return result, nil
}

// GetAppLogs reads persisted output using labels rather than transient pod names.
func (manager Manager) GetAppLogs(ctx context.Context, input TelemetryInput) (Logs, error) {
	stored, _, err := manager.applicationRecord(ctx, input.Organization, input.Space, input.Application)
	if err != nil {
		return Logs{}, err
	}
	selector := fmt.Sprintf("{borq_cluster=%s,namespace=%s,borq_space=%s,borq_app=%s}", strconv.Quote(stableId(input.Organization+"/"+stored.Space.Cluster)), strconv.Quote(stored.Space.Namespace), strconv.Quote(stored.Space.ID), strconv.Quote(input.Application))
	result, err := manager.workloadLogs(ctx, input, selector)
	if err != nil {
		return Logs{}, err
	}
	result.Events, result.EventsNotice = manager.applicationEvents(ctx, input, stored, result.Start, result.End)
	return result, nil
}

func (manager Manager) workloadMetrics(ctx context.Context, input TelemetryInput, selector string) (Metrics, error) {
	start, end, step, err := telemetryWindow(input)
	if err != nil {
		return Metrics{}, err
	}
	if manager.runtime.observability.PrometheusUrl == "" {
		return Metrics{}, ErrTelemetryUnavailable
	}
	window := strconv.Itoa(max(120, int(step.Seconds()))) + "s"
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cpu, err := manager.runtime.observability.QueryRange(ctx, observability.RangeInput{Query: "sum(rate(container_cpu_usage_seconds_total" + selector + "[" + window + "]))", Start: start, End: end, Step: step})
	if err != nil {
		return Metrics{}, fmt.Errorf("%w: %v", ErrTelemetryUnavailable, err)
	}
	memory, err := manager.runtime.observability.QueryRange(ctx, observability.RangeInput{Query: "sum(avg_over_time(container_memory_working_set_bytes" + selector + "[" + strconv.Itoa(int(step.Seconds())) + "s]))", Start: start, End: end, Step: step})
	if err != nil {
		return Metrics{}, fmt.Errorf("%w: %v", ErrTelemetryUnavailable, err)
	}
	return Metrics{Start: start, End: end, StepSeconds: int(step.Seconds()), CpuCores: cpu, MemoryBytes: memory, Events: []ApplicationEvent{}}, nil
}

func (manager Manager) workloadLogs(ctx context.Context, input TelemetryInput, selector string) (Logs, error) {
	start, end, _, err := telemetryWindow(input)
	if err != nil {
		return Logs{}, err
	}
	if input.Limit == 0 {
		input.Limit = 200
	}
	if input.Limit < 1 || input.Limit > 1000 || len(input.Search) > 256 {
		return Logs{}, ErrInvalid
	}
	if manager.runtime.observability.LokiUrl == "" {
		return Logs{}, ErrTelemetryUnavailable
	}
	if input.Search != "" {
		selector += " |= " + strconv.Quote(input.Search)
	}
	entries, err := manager.runtime.observability.Logs(ctx, observability.LogsInput{Query: selector, Start: start, End: end, Limit: input.Limit})
	if err != nil {
		return Logs{}, fmt.Errorf("%w: %v", ErrTelemetryUnavailable, err)
	}
	return Logs{Start: start, End: end, Entries: entries, LimitReached: len(entries) == input.Limit, Events: []ApplicationEvent{}}, nil
}

func telemetryWindow(input TelemetryInput) (time.Time, time.Time, time.Duration, error) {
	duration, step := time.Hour, 30*time.Second
	switch input.Range {
	case "30m":
		duration = 30 * time.Minute
	case "", "1h":
	case "12h":
		duration, step = 12*time.Hour, 5*time.Minute
	case "3d":
		duration, step = 72*time.Hour, 30*time.Minute
	default:
		return time.Time{}, time.Time{}, 0, ErrInvalid
	}
	end := input.End
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if end.After(time.Now().Add(time.Minute)) || end.Before(time.Unix(0, 0).Add(duration)) {
		return time.Time{}, time.Time{}, 0, ErrInvalid
	}
	return end.Add(-duration).UTC(), end.UTC(), step, nil
}
