//go:build integration

package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestManagementDiagnosticsAdmissionIntegration reads an actual StatefulSet
// controller's FailedCreate event after baseline admission rejects its pod.
// No Pod status or admission outcome is fabricated by the fixture.
func TestManagementDiagnosticsAdmissionIntegration(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, client, settings, kubeconfig := networkReconciliationFixture(t, ctx)
	var namespace corev1.Namespace
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Name: settings.Namespace}, &namespace))
	namespace.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
	assert.NoErr(client.Update(ctx, &namespace))
	privileged := true // Kubernetes represents this optional security setting with a pointer.
	labels := networkLabels()
	workload := &appsv1.StatefulSet{ObjectMeta: networkMetadata(settings.Namespace, "rejected-egress"), Spec: appsv1.StatefulSetSpec{
		ServiceName: "fixture", Selector: &metav1.LabelSelector{MatchLabels: labels}, // Kubernetes requires a selector pointer.
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "tailscale", Image: networkProbeImage, SecurityContext: &corev1.SecurityContext{Privileged: &privileged}}}}},
	}}
	assert.NoErr(client.Create(ctx, workload))
	assert.NoErr(wait.PollUntilContextCancel(ctx, 250*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		resources, err := ReadManagementDiagnostics(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
		if err != nil {
			return false, err
		}
		encoded, err := json.Marshal(resources)
		return strings.Contains(string(encoded), "FailedCreate") && strings.Contains(string(encoded), "PodSecurity policy blocks privileged containers"), err
	}))
	var pods corev1.PodList
	assert.NoErr(client.List(ctx, &pods, ctrlclient.InNamespace(settings.Namespace)))
	assert.Equal(len(pods.Items), 0)
}
