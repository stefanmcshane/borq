package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestManagementProgress describes startup failures without returning pod specs,
// literal configuration, secret data, or arbitrary server response text.
func TestManagementProgress(t *testing.T) {
	assert := is.New(t)
	pod := progressTestPod(DefaultRequest())
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PRIVATE", Value: "private-value"}}
	client := fake.NewClientset(&pod,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "borq-data", Namespace: "borq"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-management", Namespace: "borq"}, Data: map[string]string{"state.json": `{"components":[{"name":"openbao","status":"deployed"}]}`}})
	health := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"checks":[{"name":"management","phase":"failed","attempt":2,"description":"private-value"}]}`))
	}))
	defer health.Close()
	request := DefaultRequest()
	request.ProbeURL = health.URL
	progress := readManagementProgress(context.Background(), client.CoreV1(), health.Client(), request)
	assert.Equal(progress.pods, []string{"borq/borq-test: CrashLoopBackOff (0/1 ready, 4 restarts)"})
	assert.Equal(progress.volumes, []string{"borq/borq-data: Bound"})
	assert.Equal(progress.components, []string{"openbao: deployed"})
	assert.Equal(progress.managementPhase, "failed")
	assert.Equal(progress.serverAttempt, 2)
	assert.True(!strings.Contains(fmt.Sprint(progress), "private-value"))
}

// TestManagementProgressPodFailures distinguishes image, scheduling, and init
// failures so a pending pod is no longer reported as an unexplained wait.
func TestManagementProgressPodFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"image", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}, "ImagePullBackOff"},
		{"scheduling", corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}}, "Unschedulable"},
		{"init", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}}}}, "Init:ExitCode2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			pod := progressTestPod(DefaultRequest())
			pod.Status = test.status
			assert.True(strings.Contains(managementPodProgress(pod), test.want))
		})
	}
}

// TestManagementWaitPreservesBothReadinessGates verifies neither an old ready
// Deployment nor a healthy unrelated HTTP listener can satisfy bootstrap alone.
func TestManagementWaitPreservesBothReadinessGates(t *testing.T) {
	for _, test := range []struct {
		name       string
		ready      bool
		healthCode int
		wantReady  bool
	}{
		{"both ready", true, http.StatusOK, true},
		{"deployment unready", false, http.StatusOK, false},
		{"endpoint unready", true, http.StatusServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			request := DefaultRequest()
			health := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.healthCode)
			}))
			defer health.Close()
			request.ProbeURL = health.URL
			connection := progressTestConnection(t, request, test.ready, "")
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := waitForManagementServer(ctx, logger.Logger{}, request, connection)
			if test.wantReady {
				assert.NoErr(err)
				return
			}
			assert.True(errors.Is(err, context.DeadlineExceeded))
			assert.True(strings.Contains(err.Error(), "CrashLoopBackOff"))
			assert.True(strings.Contains(err.Error(), "borq/borq-data: Bound"))
		})
	}
}

// TestManagementWaitRejectsOldServer reproduces the published legacy image's
// unsupported server command and fails immediately without echoing its logs.
func TestManagementWaitRejectsOldServer(t *testing.T) {
	assert := is.New(t)
	request := DefaultRequest()
	request.ProbeURL = "http://127.0.0.1:1"
	connection := progressTestConnection(t, request, false, "private-log-value\nunknown command \"server\" for \"borq\"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := waitForManagementServer(ctx, logger.Logger{}, request, connection)
	assert.True(err != nil)
	assert.True(!errors.Is(err, context.DeadlineExceeded))
	assert.True(strings.Contains(err.Error(), "does not support the Borq server command"))
	assert.True(!strings.Contains(err.Error(), "private-log-value"))
}

func progressTestPod(request Request) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "borq-test", Namespace: "borq", Labels: selectorLabels(borqName)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: borqName, Image: request.BorqImage, Args: borqServerArguments(request)}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: borqName, RestartCount: 4,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}}}
}

func progressTestConnection(t *testing.T, input Request, ready bool, logs string) kubernetes.Connection {
	t.Helper()
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/apis/apps/v1/namespaces/borq/deployments/borq":
			deployment := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: borqName, Generation: 1}}
			if ready {
				deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
			}
			_ = json.NewEncoder(writer).Encode(deployment)
		case "/api/v1/namespaces/borq/pods":
			_ = json.NewEncoder(writer).Encode(corev1.PodList{Items: []corev1.Pod{progressTestPod(input)}})
		case "/api/v1/namespaces/borq/persistentvolumeclaims":
			_ = json.NewEncoder(writer).Encode(corev1.PersistentVolumeClaimList{Items: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "borq-data", Namespace: "borq"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}}})
		case "/api/v1/namespaces/borq/pods/borq-test/log":
			if request.URL.Query().Get("previous") != "true" {
				t.Error("crash-loop diagnosis must inspect the previous container")
			}
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = writer.Write([]byte(logs))
		default:
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: "Failure", Code: 404, Reason: metav1.StatusReasonNotFound})
		}
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: management\n  cluster:\n    server: %s\ncontexts:\n- name: management\n  context:\n    cluster: management\n    user: management\ncurrent-context: management\nusers:\n- name: management\n  user: {}\n", server.URL)
	assert.NoErr(os.WriteFile(path, []byte(document), 0o600))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	return connection
}
