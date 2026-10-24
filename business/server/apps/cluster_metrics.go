package apps

import (
	"context"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
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
	return client.ClusterMetrics(ctx)
}
