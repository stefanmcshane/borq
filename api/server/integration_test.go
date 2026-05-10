//go:build integration
// +build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/reconciler"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	integrationServerURLEnvVar  = "BORQ_INTEGRATION_SERVER_URL"
	defaultIntegrationServerURL = "http://localhost:7833"
)

// TestServerIntegration verifies the server health, root, and method gating
// endpoints against a running borq instance (BORQ_INTEGRATION_SERVER_URL).
func TestServerIntegration(t *testing.T) {
	serverURL := strings.TrimSpace(os.Getenv(integrationServerURLEnvVar))
	if serverURL == "" {
		serverURL = defaultIntegrationServerURL
	}
	serverURL = strings.TrimRight(serverURL, "/")

	testContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := assertHealthResponse(testContext, serverURL+"/healthz"); err != nil {
		t.Fatalf("assert /healthz endpoint: %v", err)
	}
	if err := assertHTTPResponse(testContext, http.MethodGet, serverURL+"/", http.StatusOK, "borq"); err != nil {
		t.Fatalf("assert / endpoint: %v", err)
	}
	if err := assertHTTPResponse(testContext, http.MethodPost, serverURL+"/healthz", http.StatusMethodNotAllowed, "method not allowed"); err != nil {
		t.Fatalf("assert /healthz method handling: %v", err)
	}
}

func assertHealthResponse(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("perform request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: got %d want %d", response.StatusCode, http.StatusOK)
	}

	var report HealthReport
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("decode health response: %w (body=%q); expected JSON health report from the mirrord-routed local server on localhost:7833", err, strings.TrimSpace(string(body)))
	}
	if report.Status != healthStatusSuccess {
		return fmt.Errorf("unexpected health status: got %q want %q", report.Status, healthStatusSuccess)
	}
	for _, check := range report.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("health check is missing name")
		}
		if check.Status != healthStatusSuccess && check.Status != healthStatusFail {
			return fmt.Errorf("health check %q has invalid status %q", check.Name, check.Status)
		}
		if strings.TrimSpace(check.Description) == "" {
			return fmt.Errorf("health check %q is missing description", check.Name)
		}
	}
	if len(report.Checks) == 0 {
		return fmt.Errorf("health response contained no checks")
	}

	return nil
}

// TestReconcileIntegration verifies the full reconcile flow against a real
// Kubernetes cluster. It tests: initial creation, drift correction, and
// config-driven updates.
func TestReconcileIntegration(t *testing.T) {
	log := logger.Logger{}

	kubeconfigPath := strings.TrimSpace(os.Getenv("BORQ_KUBECONFIG"))
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG must be set to run reconciliation integration tests")
	}
	conn, err := kubernetes.NewConnection(
		kubernetes.Config{KubeconfigPath: kubeconfigPath},
		log,
	)
	if err != nil {
		t.Fatalf("create kubernetes connection: %v", err)
	}

	handlers := map[string]reconciler.ComponentHandler{
		"test-component": testComponentHandler{},
	}

	port := findFreePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	server, err := buildServer(
		ctx,
		NewServerInput{Port: port, DataDir: t.TempDir(), Log: log},
		conn,
		[]HealthCheck{
			{
				Name: "test-check",
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: "test-check", Status: healthStatusSuccess, Description: "ok"}
				},
			},
		},
		handlers,
		time.Second,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	go func() {
		if serveErr := server.Serve(ctx); serveErr != nil {
			t.Logf("server stopped: %v", serveErr)
		}
	}()

	waitForServer(t, port, 5*time.Second)

	cmName := "test-integration"
	cmNamespace := "default"
	client := &http.Client{Timeout: 5 * time.Second}
	reconcileURL := fmt.Sprintf("http://localhost:%d/reconcile?component=test-component", port)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = conn.ConfigMaps().Delete(cleanupCtx, cmNamespace, cmName)
	})

	t.Run("initial creation", func(t *testing.T) {
		is := is.New(t)
		cfg := testComponentConfig{Name: cmName, Namespace: cmNamespace, Data: map[string]string{"test": "integration"}}

		result := reconcileConfig(t, ctx, client, reconcileURL, cfg)
		is.True(result.Applied)
		is.True(len(result.Diffs) > 0)

		cm, err := conn.ConfigMaps().Get(ctx, cmNamespace, cmName)
		is.NoErr(err)
		is.Equal(cm.Name, cmName)
		is.Equal(cm.Data["test"], "integration")
		is.Equal(len(cm.Data), 1)
	})

	t.Run("drift correction", func(t *testing.T) {
		is := is.New(t)

		driftLabels := map[string]string{
			"app.kubernetes.io/managed-by": "borq",
			"app.kubernetes.io/component":  "test",
		}
		err := conn.ConfigMaps().Apply(ctx, kubernetes.ConfigMap{
			Name: cmName, Namespace: cmNamespace, Labels: driftLabels,
			Data: map[string]string{"test": "drifted"},
		})
		is.NoErr(err)

		cm, err := conn.ConfigMaps().Get(ctx, cmNamespace, cmName)
		is.NoErr(err)
		is.Equal(cm.Data["test"], "drifted")

		cfg := testComponentConfig{Name: cmName, Namespace: cmNamespace, Data: map[string]string{"test": "integration"}}
		result := reconcileConfig(t, ctx, client, reconcileURL, cfg)
		is.True(result.Applied)
		is.True(len(result.Diffs) > 0)

		cm, err = conn.ConfigMaps().Get(ctx, cmNamespace, cmName)
		is.NoErr(err)
		is.Equal(cm.Data["test"], "integration")
	})

	t.Run("config change", func(t *testing.T) {
		is := is.New(t)

		cfg := testComponentConfig{Name: cmName, Namespace: cmNamespace, Data: map[string]string{"test": "updated", "extra": "value"}}
		result := reconcileConfig(t, ctx, client, reconcileURL, cfg)
		is.True(result.Applied)
		is.True(len(result.Diffs) > 0)

		cm, err := conn.ConfigMaps().Get(ctx, cmNamespace, cmName)
		is.NoErr(err)
		is.Equal(cm.Data["test"], "updated")
		is.Equal(cm.Data["extra"], "value")
		is.Equal(len(cm.Data), 2)
	})

	t.Run("idempotent reconcile", func(t *testing.T) {
		is := is.New(t)

		cfg := testComponentConfig{Name: cmName, Namespace: cmNamespace, Data: map[string]string{"test": "updated", "extra": "value"}}
		result := reconcileConfig(t, ctx, client, reconcileURL, cfg)
		is.True(!result.Applied)
		is.Equal(len(result.Diffs), 0)
	})
}

// TestReconcileIntegrationCloudNativePG verifies the real server reconciles
// the CNPG operator first and the borq postgres cluster in a separate run.
func TestReconcileIntegrationCloudNativePG(t *testing.T) {
	log := logger.Logger{}

	kubeconfigPath := strings.TrimSpace(os.Getenv("BORQ_KUBECONFIG"))
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG must be set to run CloudNativePG reconciliation integration tests")
	}
	conn, err := kubernetes.NewConnection(
		kubernetes.Config{KubeconfigPath: kubeconfigPath},
		log,
	)
	if err != nil {
		t.Fatalf("create kubernetes connection: %v", err)
	}

	handlers := map[string]reconciler.ComponentHandler{
		"cloudnativepg-operator": reconciler.CloudNativePGOperatorHandler{},
		"borq-postgres":          reconciler.BorqPostgresHandler{},
	}

	port := findFreePort(t)
	testContext, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	server, err := buildServer(
		testContext,
		NewServerInput{Port: port, DataDir: t.TempDir(), Log: log},
		conn,
		[]HealthCheck{
			{
				Name: "test-check",
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: "test-check", Status: healthStatusSuccess, Description: "ok"}
				},
			},
		},
		handlers,
		time.Second,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	go func() {
		if serveErr := server.Serve(testContext); serveErr != nil {
			t.Logf("server stopped: %v", serveErr)
		}
	}()
	waitForServer(t, port, 5*time.Second)

	client := &http.Client{Timeout: 4 * time.Minute}
	reconcileURL := fmt.Sprintf("http://localhost:%d/reconcile", port)

	t.Run("initial operator reconcile", func(t *testing.T) {
		operatorResult := reconcileRaw(t, testContext, client, reconcileURL+"?component=cloudnativepg-operator", nil)
		if len(operatorResult.Diffs) > 0 && !operatorResult.Applied {
			t.Fatalf("operator reconcile returned diffs without applying them")
		}
	})

	if err := conn.Deployments().WaitForAvailable(testContext, "borq-system", "cnpg-controller-manager"); err != nil {
		t.Fatalf("wait for cloudnativepg operator: %v", err)
	}

	t.Run("idempotent operator reconcile", func(t *testing.T) {
		operatorResult := reconcileRaw(t, testContext, client, reconcileURL+"?component=cloudnativepg-operator", nil)
		if len(operatorResult.Diffs) > 0 {
			t.Fatalf("operator reconcile returned %d diffs when no changes expected", len(operatorResult.Diffs))
		}
	})

	clusterName := "borq-postgres-integration"
	clusterNamespace := "borq-system"
	clusterPVCName := clusterName + "-1"
	deleteCloudNativePGClusterStorage(t, testContext, conn, clusterNamespace, clusterName, clusterPVCName)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		deleteCloudNativePGClusterStorage(t, cleanupCtx, conn, clusterNamespace, clusterName, clusterPVCName)
	})

	cfg := reconciler.BorqPostgresConfig{
		Name:          clusterName,
		Namespace:     clusterNamespace,
		StorageSize:   "5Gi",
		Database:      "borq",
		Owner:         "borq",
		CPURequest:    "100m",
		MemoryRequest: "100Mi",
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal postgres config: %v", err)
	}

	t.Run("initial postgres reconcile", func(t *testing.T) {
		postgresResult := reconcileRaw(t, testContext, client, reconcileURL+"?component=borq-postgres", body)
		if len(postgresResult.Diffs) == 0 && postgresResult.Applied {
			t.Fatalf("postgres reconcile applied without reporting diffs")
		}
	})

	if err := waitForCloudNativePGClusterReady(testContext, conn, clusterNamespace, clusterName); err != nil {
		t.Fatalf("wait for cloudnativepg cluster: %v", err)
	}

	t.Run("idempotent postgres reconcile", func(t *testing.T) {
		postgresResult := reconcileRaw(t, testContext, client, reconcileURL+"?component=borq-postgres", body)
		for _, d := range postgresResult.Diffs {
			t.Logf("postgres idempotent pass1 diff: type=%s name=%s patch=%+v", d.Type, d.Name, d.Patch)
		}
		if len(postgresResult.Diffs) > 0 {
			postgresResult = reconcileRaw(t, testContext, client, reconcileURL+"?component=borq-postgres", body)
			for _, d := range postgresResult.Diffs {
				t.Logf("postgres idempotent pass2 diff: type=%s name=%s patch=%+v", d.Type, d.Name, d.Patch)
			}
		}
		if len(postgresResult.Diffs) > 0 {
			postgresResult = reconcileRaw(t, testContext, client, reconcileURL+"?component=borq-postgres", body)
		}
		if len(postgresResult.Diffs) > 0 {
			t.Fatalf("postgres reconcile not converging after 3 passes: %d diffs", len(postgresResult.Diffs))
		}
	})
}

func reconcileConfig(t *testing.T, ctx context.Context, client *http.Client, url string, cfg testComponentConfig) reconciler.ReconcileResult {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	return reconcileRaw(t, ctx, client, url, body)
}

func reconcileRaw(t *testing.T, ctx context.Context, client *http.Client, url string, body []byte) reconciler.ReconcileResult {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		responseBody, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			t.Fatalf("unexpected status: got %d want %d; also failed to read response body: %v", response.StatusCode, http.StatusOK, readErr)
		}
		t.Fatalf("unexpected status: got %d want %d body=%q", response.StatusCode, http.StatusOK, strings.TrimSpace(string(responseBody)))
	}

	var result reconciler.ReconcileResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
}

func waitForCloudNativePGClusterReady(ctx context.Context, conn kubernetes.Connection, namespace string, name string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	cluster := &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1.SchemeGroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	for {
		live, err := conn.Objects().Get(ctx, cluster)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			liveCluster, ok := live.(*apiv1.Cluster)
			if !ok {
				return fmt.Errorf("unexpected cluster type %T", live)
			}
			if liveCluster.Status.Phase == apiv1.PhaseHealthy {
				return nil
			}
			for _, condition := range liveCluster.Status.Conditions {
				if condition.Type == string(apiv1.ConditionClusterReady) && condition.Status == metav1.ConditionTrue {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster %q did not become ready before timeout: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func deleteCloudNativePGClusterStorage(t *testing.T, ctx context.Context, conn kubernetes.Connection, namespace string, clusterName string, pvcName string) {
	t.Helper()

	cluster := &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1.SchemeGroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
	}
	_ = conn.Objects().Delete(ctx, cluster)

	persistentVolumeClaim := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
		},
	}
	livePersistentVolumeClaim, err := conn.Objects().Get(ctx, persistentVolumeClaim)
	if err == nil {
		_ = conn.PersistentVolumeClaims().Delete(ctx, namespace, pvcName)
		waitForPersistentVolumeClaimDeleted(ctx, conn, namespace, pvcName)
		liveClaim, ok := livePersistentVolumeClaim.(*corev1.PersistentVolumeClaim)
		if !ok {
			t.Fatalf("unexpected pvc type %T", livePersistentVolumeClaim)
		}
		if persistentVolumeName := strings.TrimSpace(liveClaim.Spec.VolumeName); persistentVolumeName != "" {
			persistentVolume := &corev1.PersistentVolume{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"},
				ObjectMeta: metav1.ObjectMeta{Name: persistentVolumeName},
			}
			_ = conn.Objects().Delete(ctx, persistentVolume)
		}
		return
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get pvc %s/%s: %v", namespace, pvcName, err)
	}
}

func waitForPersistentVolumeClaimDeleted(ctx context.Context, conn kubernetes.Connection, namespace string, name string) {
	for {
		_, err := conn.PersistentVolumeClaims().Get(ctx, namespace, name)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type testComponentConfig struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Data      map[string]string `json:"data"`
}

type testComponentHandler struct {
	reconciler.CoreApplier
}

func (testComponentHandler) Name() string { return "test-component" }

func (testComponentHandler) DefaultConfig() any {
	return &testComponentConfig{Name: "test-component", Namespace: "default"}
}

func (testComponentHandler) Plan(config any) reconciler.Plan {
	cfg := config.(*testComponentConfig)
	if cfg.Name == "" {
		cfg.Name = "test-component"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Data == nil {
		cfg.Data = map[string]string{"test": "integration"}
	}

	return reconciler.Plan{
		ApplyOrder: []string{reconciler.TypeConfigMap},
		Desired: []reconciler.Resource{
			{
				Type:      reconciler.TypeConfigMap,
				Namespace: cfg.Namespace,
				Name:      cfg.Name,
				Desired: kubernetes.ConfigMap{
					Name:      cfg.Name,
					Namespace: cfg.Namespace,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/component": "test"},
					Data:      cfg.Data,
				},
			},
		},
		Current: func(ctx context.Context, conn kubernetes.Connection) ([]reconciler.Resource, error) {
			cms, err := conn.ConfigMaps().List(ctx, cfg.Namespace, "app.kubernetes.io/managed-by=borq,app.kubernetes.io/component=test")
			if err != nil {
				return nil, err
			}
			resources := make([]reconciler.Resource, 0, len(cms))
			for _, cm := range cms {
				resources = append(resources, reconciler.Resource{
					Type: reconciler.TypeConfigMap, Namespace: cm.Namespace, Name: cm.Name, Actual: cm,
				})
			}
			return resources, nil
		},
	}
}

func findFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func waitForServer(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not start on port %d within %v", port, timeout)
}

func assertHTTPResponse(ctx context.Context, method string, url string, expectedStatus int, expectedBodySubstring string) error {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("perform request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != expectedStatus {
		return fmt.Errorf("unexpected status: got %d want %d", response.StatusCode, expectedStatus)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if !strings.Contains(string(body), expectedBodySubstring) {
		return fmt.Errorf("response body %q does not contain %q", string(body), expectedBodySubstring)
	}

	return nil
}
