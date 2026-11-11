package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// A health read must enforce the execution deadline even before a worker records
// its timeout, while allowing queued work its full grace period after starting.
func TestApplicationReadinessGracePeriod(t *testing.T) {
	for _, input := range []struct {
		name, status, want string
		executionAge       time.Duration
	}{
		{name: "executing recently", status: "waiting", executionAge: time.Second, want: "deploying"},
		{name: "execution overdue", status: "waiting", executionAge: time.Hour, want: "degraded"},
		{name: "queued", status: "queued", executionAge: time.Hour, want: "deploying"},
		{name: "legacy execution timestamp", status: "waiting", want: "degraded"},
		{name: "unready after success", status: "succeeded", executionAge: time.Hour, want: "degraded"},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			ctx := context.Background()
			cluster := newManagerCluster(t, managerClusterOptions{})
			manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Minute)
			space := createManagerEnvironment(t, manager, cluster, "production")
			plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
			assert.NoErr(err)
			stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey("acme", "production"))
			assert.NoErr(err)
			stored.Space.Configuration, stored.Manifests = plan.Configuration, plan.Manifests
			spaceWrite, err := documentWrite("space", record.Key, stored, record.Revision)
			assert.NoErr(err)
			operation := Operation{ID: plan.ID, PlanID: plan.ID, SpaceID: space.ID, Status: input.status, StartedAt: time.Now().Add(-time.Hour)}
			operationWrite, err := documentWrite("operation", plan.ID, operation, 0)
			assert.NoErr(err)
			state := operationState{}
			if input.executionAge > 0 {
				state.ExecutionStartedAt = time.Now().Add(-input.executionAge)
			}
			stateWrite, err := documentWrite("operation-state", plan.ID, state, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{spaceWrite, operationWrite, stateWrite}))

			health, err := manager.GetApp(ctx, "acme", "production", "backend")
			assert.NoErr(err)
			assert.Equal(health.State, input.want)
			assert.True(strings.Contains(health.Message, "waiting for resource creation"))
			if input.want == "degraded" {
				assert.True(strings.Contains(health.Message, "deadline exceeded"))
			}
		})
	}
}

// TestApplicationTelemetryScope owns application-to-store query scoping,
// literal search escaping, bounded granularity and errors before backend access.
func TestApplicationTelemetryScope(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	queries := make(chan string, 10)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Query().Get("query"), `job="borq-kubernetes-events"`) {
			queries <- request.URL.Query().Get("query")
		}
		if request.URL.Path == "/api/v1/query_range" {
			_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer backend.Close()
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PrometheusQueryUrl: backend.URL, LokiQueryUrl: backend.URL})
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	end := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	input := TelemetryInput{Organization: "acme", Space: "production", Application: "backend", End: end}
	for _, window := range []struct {
		name     string
		duration time.Duration
		step     int
	}{{"30m", 30 * time.Minute, 30}, {"1h", time.Hour, 30}, {"12h", 12 * time.Hour, 300}, {"3d", 72 * time.Hour, 1800}} {
		input.Range = window.name
		metrics, err := manager.GetAppMetrics(ctx, input)
		assert.NoErr(err)
		assert.Equal(metrics.StepSeconds, window.step)
		assert.Equal(metrics.Start, end.Add(-window.duration))
		assert.Equal(metrics.End, end)
		assert.Equal(len(metrics.CpuCores), 0)
		for index := 0; index < 2; index++ {
			query := <-queries
			assert.True(strings.Contains(query, `namespace="`+space.Namespace+`"`))
			assert.True(strings.Contains(query, `borq_cluster="`+stableId("acme/local")+`"`))
			assert.True(strings.Contains(query, `container="backend"`))
			assert.True(strings.Contains(query, `job="borq-cadvisor"`))
			assert.True(strings.Contains(query, `pod=~"backend-[^-]+-[^-]+"`))
		}
	}
	input.Range = "30m"
	input.Search = `error " |= "other`
	logs, err := manager.GetAppLogs(ctx, input)
	assert.NoErr(err)
	assert.Equal(logs.Start, end.Add(-30*time.Minute))
	assert.Equal(logs.End, end)
	assert.Equal(len(logs.Entries), 0)
	query := <-queries
	assert.True(strings.Contains(query, `borq_space="`+space.ID+`"`))
	assert.True(strings.Contains(query, `borq_app="backend"`))
	assert.True(strings.HasSuffix(query, ` |= "error \" |= \"other"`))
	for _, invalid := range []TelemetryInput{
		{Organization: "acme", Space: "production", Application: "missing"},
		{Organization: "acme", Space: "production", Application: "backend", Range: "90d"},
		{Organization: "acme", Space: "production", Application: "backend", Limit: 1001},
		{Organization: "acme", Space: "production", Application: "backend", Search: strings.Repeat("x", 257)},
	} {
		_, err := manager.GetAppLogs(ctx, invalid)
		if invalid.Application == "missing" {
			assert.True(errors.Is(err, ErrNotFound))
		} else {
			assert.True(errors.Is(err, ErrInvalid))
		}
	}
	assert.Equal(len(queries), 0)
}

// TestFailedReleaseHistory verifies failed releases cannot be restored and
// their history survives closing and reopening the application database.
func TestFailedReleaseHistory(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	path := filepath.Join(t.TempDir(), "state.sqlite")
	manager := openTestManager(t, path, time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	_, err = manager.PlanAppRollback(ctx, "acme", "production", "backend", plan.ID)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "successful"))
	_, err = manager.GetAppLogs(ctx, TelemetryInput{Organization: "acme", Space: "production", Application: "backend"})
	assert.True(errors.Is(err, ErrTelemetryUnavailable))
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Second)
	history, err := reopened.ListAppDeployments(ctx, "acme", "production", "backend")
	assert.NoErr(err)
	assert.Equal(len(history), 1)
	assert.Equal(history[0].Operation.ID, plan.ID)
	assert.Equal(history[0].Operation.Status, "failed")
	assert.Equal(history[0].Configuration.Image, "backend:v1")
	_, err = reopened.ListAppDeployments(ctx, "acme", "production", "missing")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestApplicationTimeline proves the API scopes persisted lifecycle events to
// exact app object shapes and retains durable deployment markers after reopen.
func TestApplicationTimeline(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	var query string
	now := time.Now().UTC()
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query().Get("query")
		type stream struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		}
		lines := [][2]string{}
		for index, event := range []string{
			`{"name":"backend-7b89c56dd8-abc12","kind":"Pod","reason":"Started","count":2,"msg":"Started container backend"}`,
			`{"name":"backend-7b89c56dd8-abc12","kind":"Pod","reason":"BackOff","count":3,"msg":"Back-off restarting failed container"}`,
			`{"name":"backend","kind":"Deployment","reason":"ScalingReplicaSet","msg":"Scaled up replica set"}`,
			`{"name":"backend-api","kind":"Deployment","reason":"ScalingReplicaSet","msg":"another app"}`,
			`{"name":"backend-other","kind":"Pod","reason":"BackOff","msg":"unowned pod"}`,
			`invalid json`,
		} {
			lines = append(lines, [2]string{strconv.FormatInt(now.Add(time.Duration(index)*time.Millisecond).UnixNano(), 10), event})
		}
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []stream{{Stream: map[string]string{}, Values: lines}}}}))
	}))
	defer backend.Close()
	path := filepath.Join(t.TempDir(), "timeline.sqlite")
	manager, err := NewManager(ctx, NewInput{DatabasePath: path, LokiQueryUrl: backend.URL})
	assert.NoErr(err)
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(ctx, managerPlanInput(t, "production", managerInitialApps()))
	assert.NoErr(err)
	_, err = manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, NewInput{DatabasePath: path, LokiQueryUrl: backend.URL})
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	stored, _, err := manager.applicationRecord(ctx, "acme", "production", "backend")
	assert.NoErr(err)
	events, notice := manager.applicationEvents(ctx, TelemetryInput{Organization: "acme", Space: "production", Application: "backend"}, stored, now.Add(-time.Hour), time.Now().Add(time.Second))
	assert.Equal(notice, "")
	assert.Equal(len(events), 5)
	assert.Equal(events[0].Kind, "restart")
	assert.Equal(events[0].Pod, "backend-7b89c56dd8-abc12")
	deployments := 0
	for _, event := range events {
		if event.Deployment == plan.ID {
			deployments++
			assert.Equal(event.Revision, applicationRevision(plan.Configuration.Apps[0]))
		}
	}
	assert.Equal(deployments, 2)
	assert.True(strings.Contains(query, `borq_cluster="`+stableId("acme/local")+`"`))
	assert.True(strings.Contains(query, `namespace="`+space.Namespace+`"`))
	assert.True(strings.Contains(query, `job="borq-kubernetes-events"`))
	_, pattern, ok := strings.Cut(query, `| name=~`)
	assert.True(ok)
	pattern, err = strconv.Unquote(pattern)
	assert.NoErr(err)
	matcher, err := regexp.Compile("^(?:" + pattern + ")$")
	assert.NoErr(err)
	assert.True(matcher.MatchString("backend-7b89c56dd8-abc12"))
	assert.True(!matcher.MatchString("backend-api"))
}
