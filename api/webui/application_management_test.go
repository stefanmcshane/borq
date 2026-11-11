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

// TestApplicationDeletionAction owns scoped deletion routing, error propagation and leaving the removed application's page.
func TestApplicationDeletionAction(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodDelete)
				assert.Equal(request.URL.Path, "/api/v1/organizations/acme/spaces/dev/apps/web")
				writer.WriteHeader(status)
				if status == http.StatusConflict {
					_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "Wait for the active operation."})
					return
				}
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "delete-web", Status: "pending", Phase: "deleting"})
			})}
			request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
			request.PostForm = url.Values{"action": {"delete-app"}, "organization": {"acme"}, "space": {"dev"}, "application": {"web"}, "tab": {"configuration"}, "plan": {"stale-plan"}}
			result, err := web.execute(request)
			if status == http.StatusConflict {
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), "Wait for the active operation"))
				return
			}
			assert.NoErr(err)
			assert.Equal(result.selection, url.Values{"view": {"applications"}, "organization": {"acme"}, "space": {"dev"}, "operation": {"delete-web"}})
			assert.True(strings.Contains(result.notice, "deletion started"))
		})
	}
}

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
		{"lifecycle", "FailedToRetrieveImagePullSecret · Unable to retrieve image pull secret ghcr", "web-abc1234567-ab123", "Image pull secret unavailable"},
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
		unavailable, space            bool
	}{
		{"logs", "&search=error&follow=on", "&lt;script&gt;untrusted&lt;/script&gt;", "<script>untrusted</script>", false, false},
		{"logs", "", "auto-refresh paused", "every 5s", false, false},
		{"logs", "&range=30m", "auto-refresh paused", "every 5s", false, false},
		{"logs", "", "Logs unavailable", "untrusted", true, false},
		{"metrics", "&range=12h", "250.0", "No metrics yet", false, false},
		{"metrics", "&range=30m", "Last 30 minutes · 30-second intervals · CPU averaged over 2 minutes", "No metrics yet", false, false},
		{"metrics", "&range=3d", "Metrics unavailable", "usage-chart", true, false},
		{"metrics", "&range=12h", "250.0", "No metrics yet", false, true},
		{"logs", "&search=error&follow=on", "&lt;script&gt;untrusted&lt;/script&gt;", "<script>untrusted</script>", false, true},
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
				base := "/api/v1/organizations/acme/spaces/dev"
				if !input.space {
					base += "/apps/web"
				}
				assert.Equal(request.URL.Path, base+"/"+input.panel)
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
			endpoint, workload := "/api/ui/application", "&app=web"
			if input.space {
				endpoint, workload = "/api/ui/space", ""
			}
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint+"?organization=acme&space=dev"+workload+"&panel="+input.panel+input.query, nil))

			data := decodeView[applicationPanelData](t, response, "application-"+input.panel)
			assert.Equal(data.Range, history)
			assert.Equal(data.Search, selection.Get("search"))
			assert.Equal(data.Follow, selection.Get("follow") == "on")
			if input.unavailable {
				assert.True(data.Problem != "")
				assert.Equal(len(data.Rows), 0)
				return
			}
			if input.panel == "logs" {
				assert.Equal(len(data.Rows), 2)
				assert.True(data.Rows[0].Entry != nil)
				assert.True(data.Rows[1].Event != nil)
				assert.True(data.Rows[0].Timestamp.Before(data.Rows[1].Timestamp))
				assert.Equal(data.Logs.Entries[0].Message, "<script>untrusted</script>")
				assert.Equal(data.Logs.Entries[0].Revision, "a1b2c3")
				assert.Equal(data.Logs.Entries[0].Image, "nginx:1.27.5")
				assert.Equal(data.Rows[1].Event.Message, "<script>event</script>")
				assert.True(!strings.Contains(response.Body.String(), "<script>"))
				return
			}
			assert.Equal(data.Cpu.Latest, "250.0")
			assert.Equal(data.Cpu.Unit, "mCPU")
			assert.Equal(data.Memory.Latest, "128.0")
			assert.Equal(data.Memory.Unit, "MiB")

		})
	}
}
