package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/reconciler"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestBuildServer verifies that a server is constructed successfully when
// startup health checks pass.
func TestBuildServer(t *testing.T) {
	is := is.New(t)

	conn := testConnection()
	server, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		conn,
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusSuccess, Description: "ok"}
				},
			},
		},
		nil,
		time.Second,
		time.Millisecond,
	)
	is.NoErr(err)
	is.Equal(server.address, ":7833")
}

// TestBuildServerFailsWhenStartupHealthcheckFails verifies that NewServer
// returns an error when startup health checks never succeed.
func TestBuildServerFailsWhenStartupHealthcheckFails(t *testing.T) {
	is := is.New(t)

	conn := testConnection()
	_, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		conn,
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusFail, Description: "denied"}
				},
			},
		},
		nil,
		5*time.Millisecond,
		time.Millisecond,
	)

	is.True(err != nil)
	is.Equal(err.Error(), `server startup healthcheck failed: kubernetes-auth: denied`)
}

// TestBuildServerRetriesStartupHealthcheckUntilSuccess verifies that
// buildServer retries health checks and succeeds once they pass.
func TestBuildServerRetriesStartupHealthcheckUntilSuccess(t *testing.T) {
	is := is.New(t)

	conn := testConnection()
	checker := &flakyHealthCheck{failures: 2}
	_, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		conn,
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run:  checker.run,
			},
		},
		nil,
		50*time.Millisecond,
		time.Millisecond,
	)

	is.NoErr(err)
	is.Equal(checker.calls(), 3)
}

// TestNewServerRejectsInvalidConfig verifies that NewServer validates its
// input and returns descriptive errors for each invalid field.
func TestNewServerRejectsInvalidConfig(t *testing.T) {
	testCases := []struct {
		name    string
		input   NewServerInput
		wantErr string
	}{
		{
			name:    "empty data dir",
			input:   NewServerInput{Port: 7833, Log: logger.Logger{}},
			wantErr: "server data dir must not be empty",
		},
		{
			name:    "non positive port",
			input:   NewServerInput{Port: 0, DataDir: t.TempDir(), Log: logger.Logger{}},
			wantErr: "server port must be greater than zero",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			is := is.New(t)
			_, err := NewServer(context.Background(), testCase.input)
			is.True(err != nil)
			is.Equal(err.Error(), testCase.wantErr)
		})
	}
}

// TestHandlerServesHealthAndRoot verifies that the HTTP handler serves the
// root and healthz endpoints with correct status codes and response bodies.
func TestHandlerServesHealthAndRoot(t *testing.T) {
	is := is.New(t)

	handler := newHandler([]HealthCheck{
		{
			Name: "test-check",
			Run: func(context.Context) HealthResult {
				return HealthResult{Name: "test-check", Status: healthStatusSuccess, Description: "ok"}
			},
		},
	}, kubernetes.Connection{}, logger.Logger{}, nil)

	rootRequest := httptest.NewRequest("GET", "/", nil)
	rootRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rootRecorder, rootRequest)
	is.Equal(rootRecorder.Code, http.StatusOK)

	healthRequest := httptest.NewRequest("GET", "/healthz", nil)
	healthRecorder := httptest.NewRecorder()
	handler.ServeHTTP(healthRecorder, healthRequest)
	is.Equal(healthRecorder.Code, http.StatusOK)

	var report HealthReport
	is.NoErr(json.Unmarshal(healthRecorder.Body.Bytes(), &report))
	is.Equal(report.Status, healthStatusSuccess)
	is.Equal(len(report.Checks), 1)
	is.Equal(report.Checks[0].Name, "test-check")
	is.Equal(report.Checks[0].Status, healthStatusSuccess)
	is.Equal(report.Checks[0].Description, "ok")
}

func TestHandlerReconcilesAllComponentsWhenComponentIsOmitted(t *testing.T) {
	is := is.New(t)

	alpha := &testReconcileHandler{name: "alpha", defaultName: "alpha-default"}
	zeta := &testReconcileHandler{name: "zeta", defaultName: "zeta-default"}

	handler := newHandler(nil, kubernetes.Connection{}, logger.Logger{}, map[string]reconciler.ComponentHandler{
		"zeta":  zeta,
		"alpha": alpha,
	})

	request := httptest.NewRequest(http.MethodPost, "/reconcile?dry-run=true", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	is.Equal(recorder.Code, http.StatusOK)

	var result reconciler.ReconcileResult
	is.NoErr(json.Unmarshal(recorder.Body.Bytes(), &result))
	is.True(!result.Applied)
	is.Equal(len(result.Diffs), 2)
	is.Equal(result.Diffs[0].Name, "alpha-default")
	is.Equal(result.Diffs[1].Name, "zeta-default")
	is.Equal(alpha.planCalls, 1)
	is.Equal(zeta.planCalls, 1)
}

func TestHandlerRejectsBodyWhenReconcilingAllComponents(t *testing.T) {
	is := is.New(t)

	handler := newHandler(nil, kubernetes.Connection{}, logger.Logger{}, map[string]reconciler.ComponentHandler{
		"alpha": &testReconcileHandler{name: "alpha", defaultName: "alpha-default"},
	})

	request := httptest.NewRequest(http.MethodPost, "/reconcile", strings.NewReader(`{"name":"override"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	is.Equal(recorder.Code, http.StatusBadRequest)
	is.True(strings.Contains(recorder.Body.String(), "request body must be empty"))
}

func TestHandlerReconcileUsesDefaultConfigWhenBodyIsEmpty(t *testing.T) {
	is := is.New(t)

	handler := newHandler(nil, kubernetes.Connection{}, logger.Logger{}, map[string]reconciler.ComponentHandler{
		"alpha": &testReconcileHandler{name: "alpha", defaultName: "alpha-default"},
	})

	request := httptest.NewRequest(http.MethodPost, "/reconcile?component=alpha&dry-run=true", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	is.Equal(recorder.Code, http.StatusOK)

	var result reconciler.ReconcileResult
	is.NoErr(json.Unmarshal(recorder.Body.Bytes(), &result))
	is.Equal(len(result.Diffs), 1)
	is.Equal(result.Diffs[0].Name, "alpha-default")
}

type flakyHealthCheck struct {
	mu        sync.Mutex
	failures  int
	callCount int
}

func (checker *flakyHealthCheck) run(context.Context) HealthResult {
	checker.mu.Lock()
	defer checker.mu.Unlock()

	checker.callCount++
	if checker.failures > 0 {
		checker.failures--
		return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusFail, Description: "temporary connection reset"}
	}

	return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusSuccess, Description: "ok"}
}

func (checker *flakyHealthCheck) calls() int {
	checker.mu.Lock()
	defer checker.mu.Unlock()
	return checker.callCount
}

func testConnection() kubernetes.Connection {
	return kubernetes.Connection{}
}

type testReconcileConfig struct {
	Name string `json:"name"`
}

type testReconcileHandler struct {
	reconciler.CoreApplier
	name        string
	defaultName string
	planCalls   int
}

func (handler *testReconcileHandler) Name() string {
	return handler.name
}

func (handler *testReconcileHandler) DefaultConfig() any {
	return &testReconcileConfig{Name: handler.defaultName}
}

func (handler *testReconcileHandler) Plan(config any) reconciler.Plan {
	handler.planCalls++

	cfg, ok := config.(*testReconcileConfig)
	if !ok {
		panic("unexpected config type")
	}

	name := cfg.Name
	if name == "" {
		name = handler.defaultName
	}

	desired := kubernetes.ConfigMap{Name: name, Namespace: "default"}
	return reconciler.Plan{
		ApplyOrder: []string{reconciler.TypeConfigMap},
		Desired: []reconciler.Resource{
			{
				Type:      reconciler.TypeConfigMap,
				Namespace: desired.Namespace,
				Name:      desired.Name,
				Desired:   desired,
			},
		},
		Current: func(context.Context, kubernetes.Connection) ([]reconciler.Resource, error) {
			return nil, nil
		},
	}
}
