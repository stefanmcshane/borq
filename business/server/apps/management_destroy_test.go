package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestManagementDestroyDrainDeadline verifies that an admitted, stalled request
// prevents cleanup, expires the operation, and leaves a durable maintenance fence.
func TestManagementDestroyDrainDeadline(t *testing.T) {
	assert := is.New(t)
	input, mutations, _ := managementDestroyFixture(t)
	input.RolloutTimeout = 500 * time.Millisecond
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	release, err := manager.BeginApplicationRequest(t.Context())
	assert.NoErr(err)
	operation, err := manager.DestroyManagement(t.Context())
	assert.NoErr(err)
	_, err = manager.BeginApplicationRequest(t.Context())
	assert.True(errors.Is(err, ErrConflict))
	assert.True(manager.runtime.context.Err() != nil)
	status := waitManagementDestroy(t, manager, operation.ID, "failed")
	assert.Equal(status.Plan.Server[0].UID, "server-original")
	assert.Equal(mutations(), 0)
	release()
	assert.NoErr(manager.Close())

	// This is the real disk close/reopen path, not another Manager value copy.
	input.RolloutTimeout = 3 * time.Second
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.BeginApplicationRequest(t.Context())
	assert.True(errors.Is(err, ErrConflict))
	status, err = manager.GetManagementDestroy(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.Equal(status.Operation.Status, "failed")
	assert.Equal(mutations(), 0)
	retried, err := manager.DestroyManagement(t.Context())
	assert.NoErr(err)
	assert.Equal(retried.ID, operation.ID)
	waitManagementDestroy(t, manager, operation.ID, "failed")
	// The metadata-only fixture denies the completion receipt write. This proves
	// the explicit retry reaches cleanup; it does not simulate controller success.
	assert.Equal(mutations(), 1)
}

// TestManagementDestroyRestart resumes the same accepted operation after an
// interrupted drain, without allowing ordinary work to run on the reopened DB.
func TestManagementDestroyRestart(t *testing.T) {
	assert := is.New(t)
	input, mutations, _ := managementDestroyFixture(t)
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	release, err := manager.BeginApplicationRequest(t.Context())
	assert.NoErr(err)
	operation, err := manager.DestroyManagement(t.Context())
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	release()
	assert.Equal(mutations(), 0)
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.BeginApplicationRequest(t.Context())
	assert.True(errors.Is(err, ErrConflict))
	status := waitManagementDestroy(t, manager, operation.ID, "failed")
	assert.Equal(status.Operation.ID, operation.ID)
	assert.Equal(mutations(), 1)
}

// TestManagementDestroyAdmissionIdentity guards the interval between read-only
// planning and the final drain: changing the core or retained network settings
// invalidates the accepted plan before any mutation.
func TestManagementDestroyAdmissionIdentity(t *testing.T) {
	for _, changed := range []string{"server", "network"} {
		t.Run(changed, func(t *testing.T) {
			assert := is.New(t)
			input, mutations, replace := managementDestroyFixture(t)
			manager, err := NewManager(t.Context(), input)
			assert.NoErr(err)
			defer manager.Close()
			release, err := manager.BeginApplicationRequest(t.Context())
			assert.NoErr(err)
			operation, err := manager.DestroyManagement(t.Context())
			assert.NoErr(err)
			replace(changed, "replacement")
			// The changed state is itself valid. Failure must reach the admission
			// identity comparison, rather than an unrelated configuration error.
			_, err = platform.PlanManagementDestroy(t.Context(), manager.managementDestroyInput(operation.ID))
			assert.NoErr(err)
			release()
			status := waitManagementDestroy(t, manager, operation.ID, "failed")
			assert.Equal(status.Plan.Server[0].UID, "server-original")
			assert.Equal(mutations(), 0)
			assert.True(manager.ManagementDestroyActive())
		})
	}
}

func waitManagementDestroy(t *testing.T, manager Manager, id, wanted string) ManagementDestroyStatus {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := manager.GetManagementDestroy(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if status.Operation.Status == wanted {
			return status
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("operation %s status %s, wanted %s", id, status.Operation.Status, wanted)
		}
	}
}

// This HTTP fixture exposes metadata and refuses every mutation. Real resource
// deletion, finalizers, receipt publication and readiness belong to integration tests.
func managementDestroyFixture(t *testing.T) (NewInput, func() int, func(string, string)) {
	t.Helper()
	var mutex sync.Mutex
	uid, hostname, mutations := "server-original", "original", 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		write := func(value any) { _ = json.NewEncoder(writer).Encode(value) }
		if request.Method != http.MethodGet {
			mutations++
			writer.WriteHeader(http.StatusForbidden)
			write(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Forbidden", "message": "fixture refuses mutation", "code": 403})
			return
		}
		path := request.URL.Path
		if path == "/api/v1/namespaces/borq-mgmt/configmaps/borq-network-settings" {
			configuration, _ := json.Marshal(platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{ManagementHostname: hostname}})
			write(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "borq-network-settings", "namespace": "borq-mgmt", "uid": "network-original", "resourceVersion": "1", "labels": map[string]string{"app.kubernetes.io/managed-by": "borq"}}, "data": map[string]string{"network.json": string(configuration)}})
			return
		}
		if path == "/api/v1/namespaces/borq-mgmt" || path == "/api/v1/namespaces/kube-system" {
			name := filepath.Base(path)
			write(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "uid": name + "-uid"}})
			return
		}
		deployment := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "borq", "namespace": "borq-mgmt", "uid": uid, "resourceVersion": "1", "labels": map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": "borq", "app.kubernetes.io/component": "server"}}, "spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "borq", "image": "fixture"}}}}}}
		if path == "/apis/apps/v1/namespaces/borq-mgmt/deployments/borq" {
			write(deployment)
			return
		}
		if path == "/apis/apps/v1/namespaces/borq-mgmt/deployments" {
			write(map[string]any{"apiVersion": "apps/v1", "kind": "DeploymentList", "items": []any{deployment}})
			return
		}
		// Optional resource APIs may be absent. Kubernetes' NotFound contract is
		// sufficient here; no cluster-wide resource discovery is being claimed.
		writer.WriteHeader(http.StatusNotFound)
		write(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "message": "fixture metadata absent", "code": 404})
	}))
	t.Cleanup(server.Close)
	directory := t.TempDir()
	kubeconfig := filepath.Join(directory, "kubeconfig")
	configuration := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: fixture\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	if err := os.WriteFile(kubeconfig, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	input := NewInput{DatabasePath: filepath.Join(directory, "state.sqlite"), InfrastructureEnabled: true, ManagementEnabled: true,
		ManagementKubeconfigPath: kubeconfig, RolloutTimeout: 3 * time.Second,
		PlatformSettings: platform.Settings{Namespace: "borq-mgmt", Profile: "dev", OpenBaoURL: "http://vault.test", TelemetryURL: "http://borq.test"}}
	return input, func() int { mutex.Lock(); defer mutex.Unlock(); return mutations }, func(kind, value string) {
		mutex.Lock()
		defer mutex.Unlock()
		if kind == "server" {
			uid = value
		} else {
			hostname = value
		}
	}
}
