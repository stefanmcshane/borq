package apps

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestClusterRegistrationQueuesDurableWork verifies registration commits a run
// without contacting the management vault, and a restart preserves its identity.
func TestClusterRegistrationQueuesDurableWork(t *testing.T) {
	is := is.New(t)
	target := newComponentTarget(t)
	input := componentManagerInput(t, target.kubeconfig)
	manager, err := NewManager(context.Background(), input)
	is.NoErr(err)
	_, err = manager.CreateOrganization(context.Background(), "acme")
	is.NoErr(err)
	request, cancel := context.WithCancel(context.Background())
	cluster, err := manager.RegisterCluster(request, ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig})
	is.NoErr(err)
	cancel()
	is.True(cluster.Reconcile == nil)
	queued, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
	is.NoErr(err)
	is.Equal(queued.Status, "queued")
	is.True(queued.ID != "")
	is.True(queued.Diff.Components != nil)
	is.True(queued.Result.Components != nil)
	is.Equal(target.writes.Load(), int64(0))
	_, err = manager.ListClusters(context.Background(), "acme")
	is.NoErr(err)
	is.NoErr(manager.Close())
	reopened, err := NewManager(context.Background(), input)
	is.NoErr(err)
	defer reopened.Close()
	after, err := reopened.GetClusterComponents(context.Background(), "acme", "remote")
	is.NoErr(err)
	is.Equal(after.ID, queued.ID)
	is.Equal(after.Status, "queued")
}

// TestComponentRegistrationOnlyAndRecovery preserves an explicit opt-out while
// interrupted installations resume under their original run and upgrade intent.
func TestComponentRegistrationOnlyAndRecovery(t *testing.T) {
	is := is.New(t)
	target := newComponentTarget(t)
	input := componentManagerInput(t, target.kubeconfig)
	manager, err := NewManager(context.Background(), input)
	is.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(context.Background(), "acme")
	is.NoErr(err)
	disabled := false
	cluster, err := manager.RegisterCluster(context.Background(), ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig, Reconcile: &disabled})
	is.NoErr(err)
	is.True(cluster.Reconcile == nil)
	is.NoErr(manager.recoverComponentRuns(context.Background()))
	installation, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
	is.NoErr(err)
	is.Equal(installation.Status, "disabled")
	is.Equal(target.writes.Load(), int64(0))
	queued, err := manager.ReconcileClusterComponents(context.Background(), "acme", "remote", true)
	is.NoErr(err)
	repeated, err := manager.ReconcileClusterComponents(context.Background(), "acme", "remote", true)
	is.NoErr(err)
	is.Equal(repeated.ID, queued.ID)
	stored, _, err := loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
	is.NoErr(err)
	stored.Installation.Status = "installing"
	is.NoErr(manager.saveComponentRecord(context.Background(), cluster.ID, stored))
	is.NoErr(manager.recoverComponentRuns(context.Background()))
	after, _, err := loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
	is.NoErr(err)
	is.Equal(after.Installation.ID, queued.ID)
	is.Equal(after.Installation.Status, "queued")
	is.True(after.Upgrade)
	is.True(after.RecoverPending)
}

// TestComponentWorkerDiffPrecedesCredentials proves a failed diff makes no
// installation writes, while a successful diff is saved before vault access.
func TestComponentWorkerDiffPrecedesCredentials(t *testing.T) {
	is := is.New(t)
	target := newComponentTarget(t)
	var vaultCalls atomic.Int64
	var durableDiff atomic.Bool
	var manager Manager
	management := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		vaultCalls.Add(1)
		state, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
		if err == nil && state.Status == "installing" && len(state.Diff.Components) == 5 {
			durableDiff.Store(true)
		}
		managerStatus(writer, http.StatusForbidden, "Forbidden", "management vault unavailable")
	}))
	defer management.Close()
	input := componentManagerInput(t, writeComponentKubeconfig(t, management.URL))
	var err error
	manager, err = NewManager(context.Background(), input)
	is.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(context.Background(), "acme")
	is.NoErr(err)
	request, cancel := context.WithCancel(context.Background())
	_, err = manager.RegisterCluster(request, ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig})
	is.NoErr(err)
	cancel()
	target.failDiff.Store(true)
	manager.startCredentialRenewal()
	failed := waitComponentStatus(t, manager, "failed")
	is.True(!failed.NextRetryAt.IsZero())
	is.Equal(vaultCalls.Load(), int64(0))
	is.Equal(target.writes.Load(), int64(0))
	target.failDiff.Store(false)
	queued, err := manager.ReconcileClusterComponents(context.Background(), "acme", "remote", false)
	is.NoErr(err)
	is.True(queued.ID != failed.ID)
	second := waitComponentStatus(t, manager, "failed")
	is.Equal(second.ID, queued.ID)
	is.True(durableDiff.Load())
	is.True(vaultCalls.Load() > 0)
	is.Equal(target.writes.Load(), int64(0))
	for _, change := range second.Diff.Components {
		is.Equal(change.Action, "install")
	}
}

// TestComponentScheduling keeps opt-outs inert, backs off failures and renews
// only completed installations after the credential renewal interval.
func TestComponentScheduling(t *testing.T) {
	is := is.New(t)
	now := time.Now()
	for _, status := range []string{"disabled", "installing", "diffing"} {
		is.True(!componentRunDue(ComponentInstallation{Status: status}, now))
	}
	is.True(componentRunDue(ComponentInstallation{Status: "queued"}, now))
	is.True(!componentRunDue(ComponentInstallation{Status: "failed", NextRetryAt: now.Add(time.Second)}, now))
	is.True(componentRunDue(ComponentInstallation{Status: "failed", NextRetryAt: now.Add(-time.Second)}, now))
	is.True(!componentRunDue(ComponentInstallation{Status: "ready", UpdatedAt: now}, now))
	is.True(componentRunDue(ComponentInstallation{Status: "ready", UpdatedAt: now.Add(-6 * time.Hour)}, now))
	is.Equal(componentRetryDelay(1), 15*time.Second)
	is.Equal(componentRetryDelay(2), 30*time.Second)
	is.Equal(componentRetryDelay(20), 5*time.Minute)
	stored, err := queueComponentRecord("cluster", componentRecord{Installation: ComponentInstallation{ID: "active", Status: "installing"}}, false)
	is.NoErr(err)
	is.Equal(stored.Installation.ID, "active")
}

// TestComponentReadinessBlocksNewPlans avoids rendering app environment pins
// against a target whose secret and ingress controllers are still being installed.
func TestComponentReadinessBlocksNewPlans(t *testing.T) {
	is := is.New(t)
	target := newComponentTarget(t)
	manager, err := NewManager(context.Background(), componentManagerInput(t, target.kubeconfig))
	is.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(context.Background(), "acme")
	is.NoErr(err)
	cluster, err := manager.RegisterCluster(context.Background(), ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig})
	is.NoErr(err)
	_, err = manager.CreateSpace(context.Background(), SpaceInput{Organization: "acme", Name: "default", Cluster: "remote"})
	is.NoErr(err)
	for _, status := range []string{"queued", "diffing", "installing", "failed", "disabled"} {
		stored := componentRecord{Installation: ComponentInstallation{ClusterID: cluster.ID, Status: status}}
		is.NoErr(manager.saveComponentRecord(context.Background(), cluster.ID, stored))
		_, err := manager.Plan(context.Background(), PlanInput{Organization: "acme", Space: "default", Patch: patchFromJson(t, `{"apps":[{"name":"web","image":"web:v1","port":8080}]}`)})
		is.True(errors.Is(err, ErrComponentsNotReady))
		is.True(errors.Is(err, ErrConflict))
	}
	is.Equal(target.writes.Load(), int64(0))
}

// Shared atomics require pointer ownership across the fixture's HTTP handlers.
type componentTarget struct {
	kubeconfig string
	writes     atomic.Int64
	reads      atomic.Int64
	failDiff   atomic.Bool
}

func newComponentTarget(t *testing.T, records ...releasev1.Release) *componentTarget {
	t.Helper()
	target := &componentTarget{}
	client := fake.NewSimpleClientset()
	for _, record := range records {
		if err := driver.NewSecrets(client.CoreV1().Secrets(record.Namespace)).Create(fmt.Sprintf("sh.helm.release.v1.%s.v%d", record.Name, record.Version), &record); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			target.writes.Add(1)
			managerStatus(writer, http.StatusForbidden, "Forbidden", "fixture rejects writes")
			return
		}
		target.reads.Add(1)
		switch {
		case request.URL.Path == "/version":
			_ = json.NewEncoder(writer).Encode(map[string]string{"major": "1", "minor": "35", "gitVersion": "v1.35.0"})
		case strings.HasSuffix(request.URL.Path, "/secrets"):
			if target.failDiff.Load() {
				managerStatus(writer, http.StatusForbidden, "Forbidden", "diff unavailable")
				return
			}
			secrets, err := client.CoreV1().Secrets("borq-system").List(request.Context(), metav1.ListOptions{LabelSelector: request.URL.Query().Get("labelSelector")})
			if err != nil {
				t.Error(err)
				http.Error(writer, "fixture", 500)
				return
			}
			secrets.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}
			_ = json.NewEncoder(writer).Encode(secrets)
		case strings.HasSuffix(request.URL.Path, "/deployments"):
			_, _ = writer.Write([]byte(`{"apiVersion":"apps/v1","kind":"DeploymentList","items":[]}`))
		case strings.HasSuffix(request.URL.Path, "/daemonsets"):
			_, _ = writer.Write([]byte(`{"apiVersion":"apps/v1","kind":"DaemonSetList","items":[]}`))
		case strings.HasSuffix(request.URL.Path, "/statefulsets"):
			_, _ = writer.Write([]byte(`{"apiVersion":"apps/v1","kind":"StatefulSetList","items":[]}`))
		default:
			managerStatus(writer, http.StatusNotFound, "NotFound", "resource not found")
		}
	}))
	t.Cleanup(server.Close)
	target.kubeconfig = writeComponentKubeconfig(t, server.URL)
	return target
}

func writeComponentKubeconfig(t *testing.T, address string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    server: %s\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: target\ncurrent-context: target\nusers:\n- name: target\n  user: {}\n", address)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func componentManagerInput(t *testing.T, kubeconfig string) NewInput {
	t.Helper()
	return NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), InfrastructureEnabled: true, ManagementEnabled: true,
		ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"}}
}

func waitComponentStatus(t *testing.T, manager Manager, status string) ComponentInstallation {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		state, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if state.Status == status {
			return state
		}
		select {
		case <-deadline.C:
			t.Fatalf("component reconciliation did not reach %s: %+v", status, state)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestClusterComponentsIncludesInstalledPostgres reproduces already installed
// optional controllers missing from idle target reads and protects worker state.
func TestClusterComponentsIncludesInstalledPostgres(t *testing.T) {
	assert := is.New(t)
	// Helm's SDK requires concrete chart and status pointers in release fixtures.
	target := newComponentTarget(t, releasev1.Release{Name: "cloudnative-pg", Namespace: "borq-system", Version: 4, Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{Name: "cloudnative-pg", Version: "0.28.0"}}, Info: &releasev1.Info{Status: common.StatusFailed}})
	manager, err := NewManager(context.Background(), componentManagerInput(t, target.kubeconfig))
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(context.Background(), "acme")
	assert.NoErr(err)
	disabled := false
	cluster, err := manager.RegisterCluster(context.Background(), ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig, Reconcile: &disabled})
	assert.NoErr(err)
	stored, _, err := loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
	assert.NoErr(err)
	stored.Installation.Status = "ready"
	stored.Installation.Message = "saved baseline result"
	stored.Installation.ID = "persisted-run"
	stored.Installation.Diff.Components = []platform.ComponentChange{{Name: "cert-manager", Action: "none"}}
	stored.Installation.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	stored.Installation.NextRetryAt = stored.Installation.UpdatedAt.Add(time.Hour)
	stored.Installation.Result = platform.Result{Profile: "dev", Namespace: "borq-system", Components: []platform.ComponentState{{Name: "cert-manager", Status: "deployed"}}}
	assert.NoErr(manager.saveComponentRecord(context.Background(), cluster.ID, stored))
	stored, _, err = loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
	assert.NoErr(err)
	observed, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
	assert.NoErr(err)
	assert.Equal(len(observed.Result.Components), 2)
	if len(observed.Result.Components) != 2 {
		return
	}
	assert.Equal(observed.Result.Components[1].Name, "cloudnative-pg")
	observed.Result = stored.Installation.Result
	assert.Equal(observed, stored.Installation)
	unchanged, _, err := loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
	assert.NoErr(err)
	assert.Equal(unchanged, stored)
	assert.Equal(target.writes.Load(), int64(0))
}

// TestClusterComponentInspectionAvailability keeps active progress and disabled
// infrastructure offline, while unavailable idle reads retain safe saved state.
func TestClusterComponentInspectionAvailability(t *testing.T) {
	for _, scenario := range []struct {
		status         string
		infrastructure bool
	}{
		{"queued", true}, {"diffing", true}, {"installing", true}, {"ready", false}, {"ready", true}, {"disabled", true},
	} {
		t.Run(fmt.Sprintf("%s/infrastructure=%v", scenario.status, scenario.infrastructure), func(t *testing.T) {
			assert := is.New(t)
			target := newComponentTarget(t)
			input := componentManagerInput(t, target.kubeconfig)
			input.InfrastructureEnabled = scenario.infrastructure
			input.ManagementEnabled = scenario.infrastructure
			manager, err := NewManager(context.Background(), input)
			assert.NoErr(err)
			defer manager.Close()
			_, err = manager.CreateOrganization(context.Background(), "acme")
			assert.NoErr(err)
			disabled := false
			cluster, err := manager.RegisterCluster(context.Background(), ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig, Reconcile: &disabled})
			assert.NoErr(err)
			stored, _, err := loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
			assert.NoErr(err)
			stored.Installation.Status = scenario.status
			stored.Installation.Result = platform.Result{Profile: "dev", Namespace: "borq-system", Components: []platform.ComponentState{{Name: "cert-manager", Status: "deployed"}}}
			assert.NoErr(manager.saveComponentRecord(context.Background(), cluster.ID, stored))
			stored, _, err = loadDocument[componentRecord](context.Background(), manager.database, "components", cluster.ID)
			assert.NoErr(err)
			before := target.reads.Load()
			target.failDiff.Store(true)
			observed, err := manager.GetClusterComponents(context.Background(), "acme", "remote")
			assert.NoErr(err)
			inspect := scenario.infrastructure && (scenario.status == "ready" || scenario.status == "disabled")
			if inspect {
				assert.Equal(observed.InspectionWarning, "Some installed PostgreSQL components could not be read.")
				assert.True(target.reads.Load() > before)
				observed.InspectionWarning = ""
			} else {
				assert.Equal(target.reads.Load(), before)
			}
			assert.Equal(observed, stored.Installation)
			assert.Equal(target.writes.Load(), int64(0))
		})
	}
}
