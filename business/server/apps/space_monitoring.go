package apps

import (
	"context"
	"fmt"
	"strconv"
)

// GetSpaceMetrics aggregates container usage within the space's saved cluster and namespace.
func (manager Manager) GetSpaceMetrics(ctx context.Context, input TelemetryInput) (Metrics, error) {
	space, err := manager.GetSpace(ctx, input.Organization, input.Space)
	if err != nil {
		return Metrics{}, err
	}
	selector := fmt.Sprintf(`{borq_cluster=%s,namespace=%s,container!="",container!="POD",job="borq-cadvisor"}`, strconv.Quote(stableId(input.Organization+"/"+space.Cluster)), strconv.Quote(space.Namespace))
	return manager.workloadMetrics(ctx, input, selector)
}

// GetSpaceLogs reads collected pod output for the space, including applications and addons.
func (manager Manager) GetSpaceLogs(ctx context.Context, input TelemetryInput) (Logs, error) {
	space, err := manager.GetSpace(ctx, input.Organization, input.Space)
	if err != nil {
		return Logs{}, err
	}
	// Lifecycle event streams share the namespace but are not pod output.
	selector := fmt.Sprintf(`{borq_cluster=%s,namespace=%s,container!=""}`, strconv.Quote(stableId(input.Organization+"/"+space.Cluster)), strconv.Quote(space.Namespace))
	return manager.workloadLogs(ctx, input, selector)
}
