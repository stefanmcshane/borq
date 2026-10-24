package platform

import (
	"context"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"go.opentelemetry.io/otel"
)

// ReadClusterManagement inspects management services on the selected physical
// cluster, preserving its kubeconfig context and omitting releases absent there.
func ReadClusterManagement(ctx context.Context, input TargetInput) (Result, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.read_cluster_management")
	defer span.End()
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return Result{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return Result{}, err
	}
	result, err := readComponents(ctx, connection, input.KubeconfigPath, valuesInput{Settings: settings}, managementCatalog(settings, false))
	if err != nil {
		return Result{}, err
	}
	installed := result
	installed.Components = []ComponentState{}
	for _, component := range result.Components {
		if component.Status != "absent" {
			installed.Components = append(installed.Components, component)
		}
	}
	return installed, nil
}
