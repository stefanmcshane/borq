package management

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func restoreManagementClusterName(ctx context.Context, connection kubernetes.Connection, request Request) (Request, error) {
	name := ""
	installedMetadata := false
	stored, err := connection.ConfigMaps().Get(ctx, request.Namespace, "borq-network-settings")
	if err != nil && !apierrors.IsNotFound(err) {
		return Request{}, fmt.Errorf("read installed management name: %w", err)
	}
	if err == nil {
		installedMetadata = true
		if stored.Labels["app.kubernetes.io/managed-by"] != "borq" {
			return Request{}, errors.New("network bootstrap settings ConfigMap is owned outside Borq")
		}
		name = stored.Data["clusterName"]
	}
	deployment, err := connection.Deployments().Get(ctx, request.Namespace, borqName)
	if err != nil && !apierrors.IsNotFound(err) {
		return Request{}, fmt.Errorf("read installed management server name: %w", err)
	}
	if err == nil {
		installedMetadata = true
		if deployment.Labels["app.kubernetes.io/managed-by"] != "borq" {
			return Request{}, errors.New("management server Deployment is owned outside Borq")
		}
		for _, container := range deployment.Pods.Containers {
			if container.Name != borqName {
				continue
			}
			for index, argument := range container.Args {
				var installed string
				if strings.HasPrefix(argument, "--management-cluster-name=") {
					installed = strings.TrimPrefix(argument, "--management-cluster-name=")
				} else if argument == "--management-cluster-name" && index+1 < len(container.Args) {
					installed = container.Args[index+1]
				} else {
					continue
				}
				if installed == "" || name != "" && name != installed {
					return Request{}, errors.New("installed management name is inconsistent; restore its configuration before re-bootstrap")
				}
				name = installed
			}
		}
	}
	if name == "" {
		if installedMetadata && request.inferredClusterName {
			return Request{}, errors.New("installed management name is unavailable; supply its existing --cluster-name to preserve addresses and storage naming")
		}
		return request, nil
	}
	if !request.inferredClusterName && request.ClusterName != name {
		return Request{}, fmt.Errorf("management installation is named %q; omit the name to reuse it; renaming requires a separate installation or migration", name)
	}
	request.ClusterName = name
	if err := request.validate(); err != nil {
		return Request{}, fmt.Errorf("installed management name is invalid: %w", err)
	}
	return request, nil
}
