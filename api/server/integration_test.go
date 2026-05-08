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

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/reconciler"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
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
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if report.Status != healthStatusSuccess {
		return fmt.Errorf("unexpected health status: got %q want %q", report.Status, healthStatusSuccess)
	}
	if len(report.Checks) == 0 {
		return fmt.Errorf("health response contained no checks")
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

func reconcileConfig(t *testing.T, ctx context.Context, client *http.Client, url string, cfg testComponentConfig) reconciler.ReconcileResult {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", response.StatusCode, http.StatusOK)
	}

	var result reconciler.ReconcileResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
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
