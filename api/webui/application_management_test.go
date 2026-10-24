package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestUsageChartAppEvents keeps markers focused on app transitions and prevents
// Kubernetes object names and runtime diagnostics from filling the hover card.
func TestUsageChartAppEvents(t *testing.T) {
	for _, input := range []struct {
		kind, message, pod, label string
	}{
		{"deployment", "Deployment accepted · registry.example/web:release", "", "Deployment started"},
		{"succeeded", "Deployment succeeded · all resources healthy", "", "Deployment completed"},
		{"failed", "Deployment failed · long diagnostic", "", "Deployment failed"},
		{"lifecycle", "Scheduled · assigned borq-namespace/web-hash-pod to k3d-server", "web-abc1234567-ab123", ""},
		{"lifecycle", "SuccessfulCreate · Created pod: web-hash-pod", "", ""},
		{"lifecycle", "Pulling · pulling image registry.example/web:release", "web-abc1234567-ab123", ""},
		{"lifecycle", "Failed · Error: failed to create containerd task: failed to create shim task: OCI runtime create failed", "web-abc1234567-ab123", "Replica failed to start"},
		{"lifecycle", "Failed · Error: OCI runtime create failed", "web-012345abcdef-pre-ab123", "Predeploy failed to start"},
		{"restart", "Started · Started container web (observed 2 times)", "web-abc1234567-ab123", "Replica restarted"},
		{"restart", "BackOff · Back-off restarting failed container web", "web-abc1234567-ab123", "Replica keeps restarting"},
		{"restart", "BackOff · Back-off pulling image registry.example/web:bad", "web-abc1234567-ab123", "Image pull failed"},
		{"lifecycle", "Unhealthy · Readiness probe failed: dial tcp 10.1.1.3:8080", "web-abc1234567-ab123", "Replica not ready"},
		{"lifecycle", "OOMKilled · Container exceeded memory limit", "web-abc1234567-ab123", "Replica ran out of memory"},
		{"lifecycle", "Started · Started container web", "web-pre-api-abc1234567-ab123", "Replica started"},
		{"lifecycle", "Started · Started container web", "web-012345abcdef-pre-ab123", "Predeploy started"},
		{"lifecycle", "BackoffLimitExceeded · Job has reached its limit", "", "Predeploy failed"},
		{"lifecycle", "DeadlineExceeded · Job exceeded active deadline", "", "Predeploy timed out"},
		{"lifecycle", "Completed · Job finished", "", "Predeploy completed"},
		{"lifecycle", "Killing · Stopping container web", "web-abc1234567-ab123", "Replica stopped"},
		{"lifecycle", "Unhealthy · Liveness probe failed", "web-abc1234567-ab123", "Replica failed health check"},
		{"lifecycle", "FailedScheduling · 0/1 nodes are available", "web-abc1234567-ab123", "Replica waiting to start"},
		{"lifecycle", "FailedMount · Volume unavailable", "web-abc1234567-ab123", "Replica storage unavailable"},
		{"lifecycle", "Evicted · Node was low on memory", "web-abc1234567-ab123", "Replica evicted"},
		{"lifecycle", "ProgressDeadlineExceeded · Replica set did not progress", "", "Deployment stalled"},
		{"lifecycle", "FailedCreate · Cannot create pod", "", "Replica failed to start"},
		{"restart", "Restart reported without a Kubernetes reason", "web-abc1234567-ab123", "Replica restarted"},
		{"lifecycle", "ScalingReplicaSet · Scaled up replica set web-abc1234567 to 2", "", "Scaling up"},
		{"lifecycle", "ScalingReplicaSet · Scaled down replica set web-abc1234567 to 0", "", "Scaling down"},
		{"lifecycle", "Unknown · irrelevant controller housekeeping", "", ""},
	} {
		t.Run(input.label+input.message[:min(20, len(input.message))], func(t *testing.T) {
			assert := is.New(t)
			start := time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
			metrics := appapi.AppMetrics{Start: start, End: start.Add(time.Hour), StepSeconds: 30, Events: []appapi.ApplicationEvent{
				{Timestamp: start.Add(30 * time.Second), Kind: input.kind, Message: input.message, Pod: input.pod, Revision: "012345abcdef"},
				{Timestamp: start.Add(-time.Second), Kind: "deployment", Message: "Outside the selected range"},
			}}
			chart := makeUsageChart("CPU", "mCPU", nil, 1000, metrics)
			if input.label == "" {
				assert.Equal(len(chart.Events), 0)
				return
			}
			assert.Equal(len(chart.Events), 1)
			assert.Equal(chart.Events[0].Message, input.label)
			assert.Equal(chart.Events[0].Clock, "04:00:30")
		})
	}
}

// TestApplicationMonitoringPanels owns the browser contract: scoped API reads,
// literal log filtering, safe output, readable units, gaps and recoverable outages.
func TestApplicationMonitoringPanels(t *testing.T) {
	for _, input := range []struct {
		panel, query, present, absent string
		unavailable                   bool
	}{
		{"logs", "&search=error&follow=on", "&lt;script&gt;untrusted&lt;/script&gt;", "<script>untrusted</script>", false},
		{"logs", "", "auto-refresh paused", "every 5s", false},
		{"logs", "&range=30m", "auto-refresh paused", "every 5s", false},
		{"logs", "", "Logs unavailable", "untrusted", true},
		{"metrics", "&range=12h", "250.0", "No metrics yet", false},
		{"metrics", "&range=30m", "Last 30 minutes · 30-second intervals · CPU averaged over 2 minutes", "No metrics yet", false},
		{"metrics", "&range=3d", "Metrics unavailable", "usage-chart", true},
	} {
		t.Run(input.panel+input.query+input.present, func(t *testing.T) {
			assert := is.New(t)
			selection, err := url.ParseQuery(strings.TrimPrefix(input.query, "&"))
			assert.NoErr(err)
			history := selection.Get("range")
			if history == "" {
				history = "1h"
			}
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/organizations/acme/spaces/dev/apps/web/"+input.panel)
				assert.Equal(request.URL.Query().Get("range"), history)
				if input.unavailable {
					writer.WriteHeader(503)
					_, _ = writer.Write([]byte(`{"message":"unavailable"}`))
					return
				}
				start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				if input.panel == "logs" {
					assert.Equal(request.URL.Query().Get("limit"), "200")
					assert.Equal(request.URL.Query().Get("search"), selection.Get("search"))
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.AppLogs{Entries: []appapi.AppLogEntry{{Timestamp: start, Pod: "web-abc-123", Container: "web", Revision: "a1b2c3", Image: "nginx:1.27.5", ReplicaSet: "7b89c56dd8", Message: "<script>untrusted</script>"}}, Events: []appapi.ApplicationEvent{{Timestamp: start.Add(time.Second), Kind: "restart", Pod: "web-abc-123", Message: "<script>event</script>"}}}))
					return
				}
				duration, err := time.ParseDuration(history)
				assert.NoErr(err)
				step := 30
				if history == "12h" {
					step = 300
				}
				assert.NoErr(json.NewEncoder(writer).Encode(appapi.AppMetrics{Start: start, End: start.Add(duration), StepSeconds: step, CPUCores: []appapi.MetricSample{{Timestamp: start, Value: 0.25}}, MemoryBytes: []appapi.MetricSample{{Timestamp: start, Value: 128 * 1024 * 1024}}}))
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/application?organization=acme&space=dev&app=web&panel="+input.panel+input.query, nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			assert.True(strings.Contains(response.Body.String(), input.present))
			assert.True(!strings.Contains(response.Body.String(), input.absent))
			if input.panel == "logs" && !input.unavailable {
				assert.True(strings.Contains(response.Body.String(), "rev a1b2c3"))
				assert.True(strings.Contains(response.Body.String(), "nginx:1.27.5"))
				assert.True(strings.Contains(response.Body.String(), "&lt;script&gt;event&lt;/script&gt;"))
			}
		})
	}
}
