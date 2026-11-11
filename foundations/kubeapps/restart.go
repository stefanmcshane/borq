package kubeapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// ErrRestartUnavailable lets callers distinguish missing, stopped, or paused deployments from connection failures.
var ErrRestartUnavailable = errors.New("deployment cannot be restarted")

// RestartInput identifies one deployment owned by the selected target.
type RestartInput struct {
	// Namespace contains the deployment.
	Namespace string
	// Name identifies the deployment within its namespace.
	Name string
	// TargetID must match the deployment's Borq ownership label.
	TargetID string
}

// RestartDeployment requests a Kubernetes rollout restart without applying application configuration.
func (client Client) RestartDeployment(ctx context.Context, input RestartInput) (ResourceStatus, error) {
	if input.Namespace == "" || input.Name == "" || input.TargetID == "" {
		return ResourceStatus{}, fmt.Errorf("restart requires a namespace, deployment name, and target ID")
	}
	resource := client.dynamic.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(input.Namespace)
	restartedAt := time.Now().UTC().Format(time.RFC3339Nano)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := resource.Get(ctx, input.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: deployment does not exist", ErrRestartUnavailable)
		}
		if err != nil {
			return fmt.Errorf("read deployment before restart: %w", err)
		}
		if err := checkOwnership(deployment.Object, input.TargetID); err != nil {
			return err
		}
		if paused, _ := valueAt(deployment.Object, "spec", "paused").(bool); paused {
			return fmt.Errorf("%w: resume the paused deployment before restarting", ErrRestartUnavailable)
		}
		if valueAt(deployment.Object, "spec", "replicas") != nil && integerAt(deployment.Object, "spec", "replicas") == 0 {
			return fmt.Errorf("%w: increase replicas before restarting a stopped deployment", ErrRestartUnavailable)
		}
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": deployment.GetResourceVersion()},
			"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
				"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": restartedAt},
			}}},
		})
		if err != nil {
			return fmt.Errorf("encode deployment restart: %w", err)
		}
		_, err = resource.Patch(ctx, input.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "kubectl-rollout"})
		return err
	})
	if err != nil {
		return ResourceStatus{}, fmt.Errorf("restart deployment %s/%s: %w", input.Namespace, input.Name, err)
	}
	return ResourceStatus{
		ApiVersion: "apps/v1", Kind: "Deployment", Namespace: input.Namespace, Name: input.Name,
		State: "running", Message: "Rolling restart started. Waiting for replacement replicas to become ready.",
	}, nil
}
