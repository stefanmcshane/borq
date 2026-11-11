package appapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestSpaceMonitoring owns the saved-space telemetry scope and HTTP bounds,
// including same-named spaces in different organizations and unavailable stores.
func TestSpaceMonitoring(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	queries := make(chan url.Values, 20)
	var unavailable atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries <- request.URL.Query()
		if unavailable.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(writer, "private-store-diagnostic")
			return
		}
		if request.URL.Path == "/api/v1/query_range" {
			_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1790812800,"0.5"]]}]}}`)
			return
		}
		_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"database-1","container":"postgres"},"values":[["1790812800000000000","<script>addon output</script>"]]},{"stream":{"pod":"web-abc-123","container":"web"},"values":[["1790812801000000000","application output"]]}]}}`)
	}))
	t.Cleanup(backend.Close)
	cluster := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/version" {
			_, _ = fmt.Fprint(writer, `{"gitVersion":"v1.35.3"}`)
			return
		}
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(cluster.Close)
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "spaces.sqlite"), PrometheusQueryUrl: backend.URL, LokiQueryUrl: backend.URL})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := NewHandler(manager)
	assert.NoErr(err)
	for _, organization := range []string{"team", "other"} {
		_, err := manager.CreateOrganization(ctx, organization)
		assert.NoErr(err)
		kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    server: " + cluster.URL + "\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: observer\ncurrent-context: target\nusers:\n- name: observer\n  user: {}\n"
		registered, err := manager.RegisterCluster(ctx, apps.ClusterInput{Organization: organization, Name: "target", Kubeconfig: kubeconfig})
		assert.NoErr(err)
		space, err := manager.CreateSpace(ctx, apps.SpaceInput{Organization: organization, Name: "dev", Cluster: "target"})
		assert.NoErr(err)
		base := "/api/v1/organizations/" + organization + "/spaces/dev/"
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, base+"metrics?range=12h&end=2026-10-01T12:00:00Z&cluster=untrusted&namespace=untrusted", nil))
		assert.Equal(response.Code, http.StatusOK)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
		var metrics AppMetrics
		assert.NoErr(json.Unmarshal(response.Body.Bytes(), &metrics))
		assert.Equal(metrics.StepSeconds, 300)
		assert.Equal(metrics.End.Sub(metrics.Start), 12*time.Hour)
		assert.Equal(metrics.CPUCores[0].Value, 0.5)
		selector := `{borq_cluster=` + strconv.Quote(registered.ID) + `,namespace=` + strconv.Quote(space.Namespace) + `,container!="",container!="POD",job="borq-cadvisor"}`
		assert.Equal((<-queries).Get("query"), "sum(rate(container_cpu_usage_seconds_total"+selector+"[300s]))")
		assert.Equal((<-queries).Get("query"), "sum(avg_over_time(container_memory_working_set_bytes"+selector+"[300s]))")
		search := `error " |= "other`
		response = httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, base+"logs?"+url.Values{"search": {search}, "limit": {"2"}, "range": {"30m"}}.Encode(), nil))
		assert.Equal(response.Code, http.StatusOK)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
		var logs AppLogs
		assert.NoErr(json.Unmarshal(response.Body.Bytes(), &logs))
		assert.Equal(len(logs.Entries), 2)
		assert.Equal(logs.Entries[0].Container, "postgres")
		assert.Equal(logs.Entries[1].Container, "web")
		assert.True(logs.LimitReached)
		assert.True(!strings.Contains(response.Body.String(), "<script>"))
		query := <-queries
		assert.Equal(query.Get("limit"), "2")
		assert.Equal(query.Get("query"), `{borq_cluster=`+strconv.Quote(registered.ID)+`,namespace=`+strconv.Quote(space.Namespace)+`,container!=""} |= `+strconv.Quote(search))
	}
	for _, scenario := range []struct {
		path string
		code int
	}{
		{"team/spaces/missing/metrics", 404},
		{"missing/spaces/dev/logs", 404},
		{"team/spaces/dev/metrics?range=90d", 400},
		{"team/spaces/dev/logs?limit=0", 400},
		{"team/spaces/dev/logs?limit=1001", 400},
		{"team/spaces/dev/logs?search=" + strings.Repeat("x", 257), 400},
	} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/"+scenario.path, nil))
		assert.Equal(response.Code, scenario.code)
		assert.Equal(len(queries), 0)
	}
	unavailable.Store(true)
	for _, panel := range []string{"metrics", "logs"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/team/spaces/dev/"+panel, nil))
		assert.Equal(response.Code, http.StatusServiceUnavailable)
		assert.True(!strings.Contains(response.Body.String(), "private-store-diagnostic"))
		<-queries
	}
}
