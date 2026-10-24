package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestDeploymentManagerWaitForRolloutConditions checks that the current
// generation has replaced old pods and every requested replica is ready and available.
func TestDeploymentManagerWaitForRolloutConditions(t *testing.T) {
	for _, condition := range []string{"ready", "becomes ready", "stale generation", "old pods", "surge replicas", "not ready", "not available"} {
		t.Run(condition, func(t *testing.T) {
			assert := is.New(t)
			deployment := rolloutTestDeployment(1)
			switch condition {
			case "stale generation":
				deployment.Status.ObservedGeneration = deployment.Generation - 1
			case "old pods":
				deployment.Status.UpdatedReplicas = 0
			case "surge replicas":
				deployment.Status.Replicas = 2
			case "not ready":
				deployment.Status.ReadyReplicas = 0
			case "not available":
				deployment.Status.AvailableReplicas = 0
			}
			client := fake.NewClientset(&deployment)
			timeout := 50 * time.Millisecond
			pending := condition == "becomes ready"
			if pending {
				timeout = 2 * time.Second
				client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					current := deployment
					if pending {
						current.Status.UpdatedReplicas = 0
						pending = false
					}
					return true, &current, nil // Kubernetes requires a resource pointer.
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			err := (DeploymentManager{clientset: client}).WaitForRollout(ctx, "borq", "borq")
			if condition == "ready" || condition == "becomes ready" {
				assert.NoErr(err)
				return
			}
			assert.True(errors.Is(err, context.DeadlineExceeded))
			assert.True(len(client.Actions()) > 0)
		})
	}
}

// TestDeploymentManagerWaitForRolloutDefaultsAndScaleToZero verifies Kubernetes'
// implicit one replica and an explicit fully completed scale-to-zero operation.
func TestDeploymentManagerWaitForRolloutDefaultsAndScaleToZero(t *testing.T) {
	is := is.New(t)
	defaulted := rolloutTestDeployment(1)
	defaulted.Spec.Replicas = nil
	is.NoErr((DeploymentManager{clientset: fake.NewClientset(&defaulted)}).WaitForRollout(context.Background(), "borq", "borq"))
	zero := rolloutTestDeployment(0)
	is.NoErr((DeploymentManager{clientset: fake.NewClientset(&zero)}).WaitForRollout(context.Background(), "borq", "borq"))
	zero.Status.Replicas = 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := (DeploymentManager{clientset: fake.NewClientset(&zero)}).WaitForRollout(ctx, "borq", "borq")
	is.True(errors.Is(err, context.DeadlineExceeded))
}

// TestDeploymentManagerWaitForRolloutCancellation verifies cancellation releases
// the waiter both before creation and when a Deployment update remains pending.
func TestDeploymentManagerWaitForRolloutCancellation(t *testing.T) {
	for _, absent := range []bool{true, false} {
		is := is.New(t)
		client := fake.NewClientset()
		readStarted := make(chan struct{}, 1)
		client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			select {
			case readStarted <- struct{}{}:
			default:
			}
			return false, nil, nil
		})
		if !absent {
			deployment := rolloutTestDeployment(1)
			deployment.Status.UpdatedReplicas = 0
			_, err := client.AppsV1().Deployments("borq").Create(context.Background(), &deployment, metav1.CreateOptions{})
			is.NoErr(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() { finished <- (DeploymentManager{clientset: client}).WaitForRollout(ctx, "borq", "borq") }()
		select {
		case <-readStarted:
		case <-time.After(time.Second):
			t.Fatal("rollout waiter did not check the Deployment")
		}
		cancel()
		select {
		case err := <-finished:
			is.True(errors.Is(err, context.Canceled))
		case <-time.After(200 * time.Millisecond):
			t.Fatal("rollout waiter ignored cancellation")
		}
	}
}

func rolloutTestDeployment(replicas int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: "borq", Generation: 4},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas}, // Kubernetes uses a pointer for defaultable replica counts.
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
}
