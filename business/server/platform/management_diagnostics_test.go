package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestManagementDiagnosticsCancellation keeps optional inspection bounded when
// the Kubernetes API stalls, including the first request on a new connection.
func TestManagementDiagnosticsCancellation(t *testing.T) {
	assert := is.New(t)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(time.Second):
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: "+api.URL+"\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\ncurrent-context: fixture\n"), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := ReadManagementDiagnostics(ctx, ManagementInput{KubeconfigPath: path})
	assert.True(errors.Is(err, context.DeadlineExceeded))
	assert.True(time.Since(started) < 500*time.Millisecond)
}

// TestManagementDiagnostics observes supplied Kubernetes status without claiming
// controller success. Recovered/replaced pods and old events must not look like
// current failures, and provider-controlled message bodies must remain private.
func TestManagementDiagnostics(t *testing.T) {
	assert := is.New(t)
	settings := Settings{Namespace: ManagementNamespace}
	crashing := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "operator-current", Namespace: SystemNamespace, UID: "current", Labels: map[string]string{"app.kubernetes.io/part-of": "borq"}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "operator", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "private-container-secret"}}}}}}
	healthy := crashing.DeepCopy()
	healthy.Name, healthy.UID = "operator-recovered", "recovered"
	healthy.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	historical := crashing.DeepCopy()
	historical.Name, historical.UID, historical.Status.Phase = "operator-retained-failure", "historical", corev1.PodFailed
	terminating := crashing.DeepCopy()
	terminating.Name, terminating.UID, terminating.Finalizers = "operator-deleted", "deleted", []string{"fixture"}
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	unrelated := crashing.DeepCopy()
	unrelated.Name, unrelated.UID, unrelated.Labels = "unrelated", "unrelated", nil
	probe := &batchv1.Job{ObjectMeta: networkMetadata(SystemNamespace, "borq-network-probe-current")}
	probe.UID = "probe"
	completed := probe.DeepCopy()
	completed.Name, completed.Status.Succeeded = "borq-network-probe-completed", 1
	ingress := &networkingv1.Ingress{ObjectMeta: networkMetadata(ManagementNamespace, "borq-management")}
	ingress.UID = "ingress"
	service := &corev1.Service{ObjectMeta: networkMetadata(SystemNamespace, managementAdminEgressName), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "unused", Ports: []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(443)}}}}
	objects := []ctrlclient.Object{crashing, healthy, historical, terminating, unrelated, probe, completed, ingress, service}
	blocked := &appsv1.StatefulSet{ObjectMeta: networkMetadata(SystemNamespace, "egress-without-pod")}
	blocked.UID = "egress"
	objects = append(objects, blocked)
	for _, warning := range []struct {
		name, reason, uid string
		age               time.Duration
	}{
		{"current", "BackOff", "current", 0},
		{"replaced", "FailedMount", "old-uid", 0},
		{"recovered", "FailedScheduling", "recovered", 0},
		{"retained failure", "FailedAttachVolume", "historical", 0},
		{"expired", "Unhealthy", "current", time.Hour},
		{"https", "HTTPSNotEnabled", "ingress", 0},
		{"unknown", "private-reason-secret", "current", 0},
		{"admission", "FailedCreate", "egress", 0},
	} {
		namespace, name := SystemNamespace, crashing.Name
		if warning.uid == "ingress" {
			namespace, name = ManagementNamespace, ingress.Name
		}
		message := "private-event-secret"
		if warning.uid == "egress" {
			name, message = blocked.Name, `pod creation violates PodSecurity "baseline:latest": privileged containers forbidden; private-event-secret`
		}
		objects = append(objects, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: warning.name, Namespace: namespace}, Type: corev1.EventTypeWarning, Reason: warning.reason, Message: message, LastTimestamp: metav1.NewTime(time.Now().Add(-warning.age)), InvolvedObject: corev1.ObjectReference{Name: name, UID: types.UID(warning.uid)}})
	}
	state, err := json.Marshal(Result{Components: []ComponentState{{Name: "external-dns", Namespace: ManagementNamespace, Status: "installing"}, {Name: "openbao", Status: "deployed"}}})
	assert.NoErr(err)
	objects = append(objects, &corev1.ConfigMap{ObjectMeta: networkMetadata(ManagementNamespace, "borq-platform-management"), Data: map[string]string{"state.json": string(state)}})
	client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(objects...).Build()
	resources, err := readManagementDiagnostics(t.Context(), client, settings)
	assert.NoErr(err)
	encoded, err := json.Marshal(resources)
	assert.NoErr(err)
	output := string(encoded)
	for _, want := range []string{"CrashLoopBackOff", "restarts=3", "HTTPSNotEnabled", "component installing", "borq-network-probe-current", "egress proxy", "FailedCreate", "PodSecurity policy"} {
		assert.True(strings.Contains(output, want))
	}
	for _, absent := range []string{"private-", "operator-recovered", "operator-retained-failure", "operator-deleted", "unrelated", "Unhealthy", "openbao", "probe-completed", "FailedMount", "FailedScheduling", "FailedAttachVolume"} {
		assert.True(!strings.Contains(output, absent))
	}
}

// TestManagementPodDiagnostics distinguishes initialization and exit status from
// healthy/completed pods without copying arbitrary termination messages.
func TestManagementPodDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, want string
		status     corev1.PodStatus
	}{
		{"completed", "", corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{"initializing", "PodInitializing", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}}},
		{"terminated", "exited with code 42", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 42, Message: "private-secret"}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			message := managementPodDiagnostic(corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: ManagementNamespace}, Status: test.status})
			if test.want == "" {
				assert.Equal(message, "")
			} else {
				assert.True(strings.Contains(message, test.want))
			}
			assert.True(!strings.Contains(message, "private-secret"))
		})
	}
}
