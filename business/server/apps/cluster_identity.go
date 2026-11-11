package apps

import (
	"context"
	"errors"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func samePhysicalCluster(ctx context.Context, first, second Cluster) (bool, error) {
	firstID, err := physicalClusterIdentity(ctx, first)
	if err != nil {
		return false, err
	}
	secondID, err := physicalClusterIdentity(ctx, second)
	return firstID == secondID, err
}

func physicalClusterIdentity(ctx context.Context, cluster Cluster) (string, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return "", errors.New("physical cluster connection unavailable")
	}
	namespace, err := connection.Objects().Get(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}})
	if err != nil {
		return "", errors.New("physical cluster identity unavailable")
	}
	identity := string(namespace.(*corev1.Namespace).UID)
	if identity == "" {
		return "", errors.New("physical cluster identity is empty")
	}
	return identity, nil
}
