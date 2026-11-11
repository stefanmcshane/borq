package apps

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func (manager Manager) addonTelemetrySelector(ctx context.Context, input TelemetryInput) (string, error) {
	addon, err := manager.GetAddon(ctx, input.Organization, input.Space, input.Application)
	if err != nil {
		return "", err
	}
	space, err := manager.GetSpace(ctx, input.Organization, input.Space)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("{borq_cluster=%s,namespace=%s,borq_space=%s,borq_addon=%s}", strconv.Quote(stableId(input.Organization+"/"+space.Cluster)), strconv.Quote(addon.Namespace), strconv.Quote(space.ID), strconv.Quote(addon.Name)), nil
}

// GetAddonMetrics sums CPU and memory across the addon's selected pods and containers.
func (manager Manager) GetAddonMetrics(ctx context.Context, input TelemetryInput) (Metrics, error) {
	selector, err := manager.addonTelemetrySelector(ctx, input)
	if err != nil {
		return Metrics{}, err
	}
	selector = strings.TrimSuffix(selector, "}") + `,job="borq-cadvisor"}`
	result, err := manager.workloadMetrics(ctx, input, selector)
	if err != nil {
		return Metrics{}, err
	}
	input.End = result.End
	history, err := manager.GetAddonEvents(ctx, input)
	if err != nil {
		return Metrics{}, err
	}
	result.Events, result.EventsNotice = history.Events, history.EventsNotice
	return result, nil
}

// GetAddonLogs reads bounded persisted output for the addon's selected pods.
func (manager Manager) GetAddonLogs(ctx context.Context, input TelemetryInput) (Logs, error) {
	selector, err := manager.addonTelemetrySelector(ctx, input)
	if err != nil {
		return Logs{}, err
	}
	result, err := manager.workloadLogs(ctx, input, selector)
	if err != nil {
		return Logs{}, err
	}
	input.End = result.End
	history, err := manager.GetAddonEvents(ctx, input)
	if err != nil {
		return Logs{}, err
	}
	result.Events, result.EventsNotice = history.Events, history.EventsNotice
	return result, nil
}
