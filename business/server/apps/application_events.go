package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/observability"
)

// ApplicationEvent correlates durable deployments and collected Kubernetes events with telemetry.
type ApplicationEvent struct {
	// Timestamp is when a deployment was accepted, completed, or a Kubernetes event observed.
	Timestamp time.Time `json:"timestamp"`
	// Kind is deployment, succeeded, failed, restart, or lifecycle.
	Kind string `json:"kind"`
	// Message explains the observed transition; treat it as untrusted text.
	Message string `json:"message"`
	// Pod identifies an involved replica when the event concerns a Pod.
	Pod string `json:"pod"`
	// Revision matches the app runtime version in logs, when known.
	Revision string `json:"revision"`
	// Deployment is a durable operation ID for deployment markers; empty on Kubernetes events.
	Deployment string `json:"deployment"`
}

// Runtime revisions exclude scaling, routing and predeploy budgets, which do not
// change the application process. The same settings retain their version on retries.
func applicationRevision(application App) string {
	encoded, _ := json.Marshal(struct {
		Image   string
		Port    int
		Command string
		Env     EnvReference
		Checks  Checks
	}{application.Image, application.Port, application.Command, application.Env, application.Checks})
	return stableId(string(encoded))[:12]
}

func (manager Manager) applicationEvents(ctx context.Context, input TelemetryInput, stored spaceRecord, start, end time.Time) ([]ApplicationEvent, string) {
	events := []ApplicationEvent{}
	notice := ""
	releases, err := manager.appDeployments(ctx, input.Organization, input.Space, input.Application, 0)
	if err != nil {
		notice = "Deployment history is temporarily unavailable."
	}
	for _, release := range releases {
		operation := release.Operation
		if !operation.StartedAt.Before(start) && !operation.StartedAt.After(end) {
			events = append(events, ApplicationEvent{Timestamp: operation.StartedAt, Kind: "deployment", Message: "Deployment accepted · " + release.Configuration.Image, Revision: release.Revision, Deployment: operation.ID})
		}
		if (operation.Status == "succeeded" || operation.Status == "failed" || operation.Status == "conflict") && !operation.UpdatedAt.Before(start) && !operation.UpdatedAt.After(end) {
			kind := "succeeded"
			if operation.Status != "succeeded" {
				kind = "failed"
			}
			events = append(events, ApplicationEvent{Timestamp: operation.UpdatedAt, Kind: kind, Message: "Deployment " + operation.Status + " · " + operation.Message, Revision: release.Revision, Deployment: operation.ID})
		}
	}
	name := regexp.QuoteMeta(input.Application)
	objects := []kubernetesEventMatch{
		{kind: "Deployment", pattern: name},
		{kind: "ReplicaSet", pattern: name + `-[a-z0-9]{8,10}`},
		{kind: "Job", pattern: name + `-[a-f0-9]{12}-pre`},
		{kind: "Pod", pattern: name + `-([a-z0-9]{8,10}|[a-f0-9]{12}-pre)-[a-z0-9]{5}`},
	}
	observed, observedNotice := manager.kubernetesEvents(ctx, stableId(input.Organization+"/"+stored.Space.Cluster), stored.Space.Namespace, objects, start, end)
	events = append(events, observed...)
	slices.SortStableFunc(events, func(a, b ApplicationEvent) int { return a.Timestamp.Compare(b.Timestamp) })
	return events, strings.TrimSpace(notice + " " + observedNotice)
}

type kubernetesEventMatch struct {
	kind, pattern string
}

func (manager Manager) kubernetesEvents(ctx context.Context, cluster, namespace string, objects []kubernetesEventMatch, start, end time.Time) ([]ApplicationEvent, string) {
	events := []ApplicationEvent{}
	if manager.runtime.observability.LokiUrl == "" {
		return events, "Kubernetes lifecycle history is unavailable."
	}
	if len(objects) == 0 {
		return events, ""
	}
	patterns := []string{}
	matchers := map[string][]*regexp.Regexp{} // Compiled regular expressions are pointers in Go's regexp API.
	for _, object := range objects {
		patterns = append(patterns, object.pattern)
		matchers[object.kind] = append(matchers[object.kind], regexp.MustCompile("^(?:"+object.pattern+")$"))
	}
	// Filter names before the bounded Loki read; recheck name AND kind locally.
	selector := fmt.Sprintf(`{borq_cluster=%s,namespace=%s,job="borq-kubernetes-events"} | json | __error__="" | name=~%s`, strconv.Quote(cluster), strconv.Quote(namespace), strconv.Quote("(?:"+strings.Join(patterns, "|")+")"))
	lines, err := manager.runtime.observability.Logs(ctx, observability.LogsInput{Query: selector, Start: start, End: end, Limit: 500})
	if err != nil {
		return events, "Kubernetes lifecycle history is temporarily unavailable."
	}
	notice := ""
	if len(lines) == 500 {
		notice = "Showing the latest 500 Kubernetes events; narrow the time range for more detail."
	}
	for _, line := range lines {
		if line.Timestamp.Before(start) || line.Timestamp.After(end) {
			continue
		}
		var event struct {
			Name, Kind, Reason, Msg string
			Count                   int
		}
		if json.Unmarshal([]byte(line.Message), &event) != nil {
			continue
		}
		if !slices.ContainsFunc(matchers[event.Kind], func(pattern *regexp.Regexp) bool { return pattern.MatchString(event.Name) }) {
			continue
		}
		kind, message, pod := "lifecycle", event.Reason+" · "+event.Msg, ""
		if event.Kind == "Pod" {
			pod = event.Name
			if event.Reason == "BackOff" || (event.Reason == "Started" && event.Count > 1) {
				kind = "restart"
			}
		}
		if event.Count > 1 {
			message += fmt.Sprintf(" (observed %d times)", event.Count)
		}
		events = append(events, ApplicationEvent{Timestamp: line.Timestamp, Kind: kind, Message: message, Pod: pod})
	}
	return events, notice
}
