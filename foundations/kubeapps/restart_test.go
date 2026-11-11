package kubeapps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestRestartRejectsUnavailableDeployments owns restart input and live ownership guards without fabricating controller results.
func TestRestartRejectsUnavailableDeployments(t *testing.T) {
	for _, test := range []struct {
		name, message string
		change        func(map[string]any, *RestartInput)
	}{
		{"missing namespace", "restart requires", func(_ map[string]any, input *RestartInput) { input.Namespace = "" }},
		{"missing name", "restart requires", func(_ map[string]any, input *RestartInput) { input.Name = "" }},
		{"missing target", "restart requires", func(_ map[string]any, input *RestartInput) { input.TargetID = "" }},
		{"missing deployment", "does not exist", func(_ map[string]any, input *RestartInput) { input.Name = "missing" }},
		{"other target", "not owned", func(_ map[string]any, input *RestartInput) { input.TargetID = "another-target" }},
		{"paused", "paused deployment", func(live map[string]any, _ *RestartInput) { live["spec"].(map[string]any)["paused"] = true }},
		{"stopped", "stopped deployment", func(live map[string]any, _ *RestartInput) { live["spec"].(map[string]any)["replicas"] = int64(0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			live := defaultResource(deploymentManifest())
			input := RestartInput{Namespace: "shop", Name: "frontend", TargetID: "target-one"}
			test.change(live, &input)
			client, _ := testClient(live)
			_, err := client.RestartDeployment(context.Background(), input)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.message))
			deployment, err := client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(context.Background(), "frontend", metav1.GetOptions{})
			assert.NoErr(err)
			assert.Equal(deployment.Object, live)
		})
	}
}

// TestRestartRechecksOwnershipAfterConflict prevents a concurrent ownership change from restarting another target's deployment.
func TestRestartRechecksOwnershipAfterConflict(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	client, fakeClient := testClient(defaultResource(deploymentManifest()))
	fakeClient.PrependReactor("patch", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object, err := fakeClient.Tracker().Get(deploymentResource(), "shop", "frontend")
		assert.NoErr(err)
		deployment := object.(*unstructured.Unstructured).DeepCopy()
		deployment.SetLabels(map[string]string{targetLabel: "another-target"})
		assert.NoErr(fakeClient.Tracker().Update(deploymentResource(), deployment, "shop"))
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "frontend", errors.New("ownership changed"))
	})
	_, err := client.RestartDeployment(ctx, RestartInput{Namespace: "shop", Name: "frontend", TargetID: "target-one"})
	assert.True(errors.Is(err, ErrOwnershipConflict))
	deployment, err := client.dynamic.Resource(deploymentResource()).Namespace("shop").Get(ctx, "frontend", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(textAt(deployment.Object, "spec", "template", "metadata", "annotations", "kubectl.kubernetes.io/restartedAt"), "")
}
