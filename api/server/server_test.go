package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func TestBuildServer(t *testing.T) {
	is := is.New(t)

	server, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusSuccess, Description: "ok"}
				},
			},
		},
		time.Second,
		time.Millisecond,
	)
	is.NoErr(err)
	is.Equal(server.address, ":7833")
}

func TestBuildServerFailsWhenStartupHealthcheckFails(t *testing.T) {
	is := is.New(t)

	_, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run: func(context.Context) HealthResult {
					return HealthResult{Name: kubernetesAuthCheckName, Status: healthStatusFail, Description: "denied"}
				},
			},
		},
		5*time.Millisecond,
		time.Millisecond,
	)

	is.True(err != nil)
	is.Equal(err.Error(), `server startup healthcheck failed: kubernetes-auth: denied`)
}

func TestBuildServerRetriesStartupHealthcheckUntilSuccess(t *testing.T) {
	is := is.New(t)

	checker := &flakyHealthCheck{failures: 2}
	_, err := buildServer(
		context.Background(),
		NewServerInput{Port: 7833, DataDir: t.TempDir(), Log: logger.Logger{}},
		[]HealthCheck{
			{
				Name: kubernetesAuthCheckName,
				Run:  checker.run,
			},
		},
		50*time.Millisecond,
		time.Millisecond,
	)

	is.NoErr(err)
	is.Equal(checker.calls(), 3)
}

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

func TestHandlerServesHealthAndRoot(t *testing.T) {
	is := is.New(t)

	handler := newHandler([]HealthCheck{
		{
			Name: "test-check",
			Run: func(context.Context) HealthResult {
				return HealthResult{Name: "test-check", Status: healthStatusSuccess, Description: "ok"}
			},
		},
	})

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
