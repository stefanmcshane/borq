package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	appsv1 "k8s.io/api/apps/v1"
)

func checkNetworkControllerConflicts(ctx context.Context, connection kubernetes.Connection, settings Settings, management bool) error {
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	deployments := &appsv1.DeploymentList{}
	if err := client.List(ctx, deployments); err != nil {
		return fmt.Errorf("inventory network controllers: %w", err)
	}
	for _, deployment := range deployments.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			component, namespace := "", ""
			image := strings.Split(container.Image, "@")[0]
			if strings.Contains(image, "tailscale/k8s-operator") {
				component, namespace = "tailscale-operator", SystemNamespace
			}
			if management && settings.Network.Cloudflare != nil && strings.Contains(image, "external-dns/external-dns") {
				component, namespace = "external-dns", settings.Namespace
			}
			if component == "" {
				continue
			}
			if deployment.Namespace != namespace || deployment.Annotations["meta.helm.sh/release-name"] != component || deployment.Annotations["meta.helm.sh/release-namespace"] != namespace {
				return fmt.Errorf("network controller %s already runs outside the managed release at %s/%s", component, deployment.Namespace, deployment.Name)
			}
		}
	}
	return nil
}
