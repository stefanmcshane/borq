package kubernetes

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WaitForRollout waits for the current Deployment generation and all desired
// replicas to finish updating. Available replicas from an older generation do
// not satisfy this gate. A nil replicas field defaults to one; zero is supported.
func (manager DeploymentManager) WaitForRollout(ctx context.Context, namespace, name string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("deployment %s/%s rollout did not complete: %w", namespace, name, err)
		}
		deployment, err := manager.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get deployment %s/%s for rollout: %w", namespace, name, err)
		}
		if err == nil && deploymentRolloutComplete(*deployment) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("deployment %s/%s rollout did not complete: %w", namespace, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func deploymentRolloutComplete(deployment appsv1.Deployment) bool {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.UpdatedReplicas == desired &&
		deployment.Status.Replicas == desired &&
		deployment.Status.ReadyReplicas >= desired &&
		deployment.Status.AvailableReplicas >= desired
}
