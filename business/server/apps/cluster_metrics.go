package apps

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/observability"
	"go.opentelemetry.io/otel"
)

// GetClusterMetrics observes the physical cluster through one organization's saved link.
func (manager Manager) GetClusterMetrics(ctx context.Context, organization, name string) (kubeapps.ClusterMetrics, error) {
	ctx, span := otel.Tracer("borq/apps").Start(ctx, "apps.get_cluster_metrics")
	defer span.End()
	if err := validateNames(organization, name); err != nil {
		return kubeapps.ClusterMetrics{}, err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+name)
	if err != nil {
		return kubeapps.ClusterMetrics{}, err
	}
	client, err := newClusterClient(cluster)
	if err != nil {
		return kubeapps.ClusterMetrics{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshot, err := client.ClusterMetrics(ctx)
	if err != nil {
		return snapshot, err
	}
	return manager.withClusterUsage(ctx, snapshot, cluster.ID), nil
}

func (manager Manager) withClusterUsage(ctx context.Context, snapshot kubeapps.ClusterMetrics, clusterID string) kubeapps.ClusterMetrics {
	if len(snapshot.Nodes) == 0 {
		return snapshot
	}
	snapshot.UsageNotice = "CPU and memory usage is unavailable. Check Prometheus and the cluster collector."
	if manager.runtime.observability.PrometheusUrl == "" {
		return snapshot
	}
	labels := fmt.Sprintf(`borq_cluster=%s,job="borq-cadvisor",id="/"`, strconv.Quote(clusterID))
	queries := []string{
		`max by (node) (rate(container_cpu_usage_seconds_total{` + labels + `,cpu="total"}[2m]))`,
		`max by (node) (container_memory_working_set_bytes{` + labels + `})`,
		// Aggregations and rate use evaluation timestamps; freshness must use source samples.
		`min by (node) (timestamp({__name__=~"container_cpu_usage_seconds_total|container_memory_working_set_bytes",` + labels + `}))`,
	}
	measurements := make([]map[string]float64, len(queries))
	for index, query := range queries {
		samples, err := manager.runtime.observability.Query(ctx, observability.QueryInput{Query: query, Time: snapshot.ObservedAt})
		if err != nil {
			return snapshot
		}
		measurements[index] = map[string]float64{}
		for _, sample := range samples {
			measurements[index][sample.Labels["node"]] = sample.Sample.Value
		}
	}
	measuredNodes := 0
	for index, node := range snapshot.Nodes {
		cpu, cpuAvailable := measurements[0][node.Name]
		memory, memoryAvailable := measurements[1][node.Name]
		seconds, timeAvailable := measurements[2][node.Name]
		if !cpuAvailable || !memoryAvailable || !timeAvailable || cpu < 0 || memory < 0 || memory >= math.MaxInt64 || seconds < float64(snapshot.ObservedAt.Add(-5*time.Minute).Unix()) || seconds > float64(snapshot.ObservedAt.Add(time.Minute).Unix()) {
			continue
		}
		node.UsageAvailable = true
		node.CpuUsageCores, node.MemoryUsageBytes = cpu, int64(memory)
		node.UsageObservedAt = time.UnixMilli(int64(seconds * 1000)).UTC()
		snapshot.Nodes[index] = node
		measuredNodes++
	}
	snapshot.UsageNotice = ""
	if measuredNodes < len(snapshot.Nodes) {
		snapshot.UsageNotice = fmt.Sprintf("Recent CPU and memory measurements are available for %d of %d nodes.", measuredNodes, len(snapshot.Nodes))
	}
	return snapshot
}
