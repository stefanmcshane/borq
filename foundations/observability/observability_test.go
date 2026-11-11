package observability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestManagementProxyCredentials owns management service routing and prevents
// Kubernetes credentials from following a private query-store override.
func TestManagementProxyCredentials(t *testing.T) {
	assert := is.New(t)
	management := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/namespaces/monitoring/services/http:prometheus-server:80/proxy/api/v1/query_range")
		assert.Equal(request.Header.Get("Authorization"), "Bearer test-management-credential")
		_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer management.Close()
	configuration := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: management
  cluster:
    server: %s
    insecure-skip-tls-verify: true
users:
- name: management
  user:
    token: test-management-credential
contexts:
- name: management
  context:
    cluster: management
    user: management
current-context: management
`, management.URL)
	path := filepath.Join(t.TempDir(), "management.yaml")
	assert.NoErr(os.WriteFile(path, []byte(configuration), 0600))
	client, err := NewManagementClient(path, "monitoring")
	assert.NoErr(err)
	_, err = client.QueryRange(context.Background(), RangeInput{})
	assert.NoErr(err)
	override := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/api/v1/query_range")
		assert.Equal(request.Header.Get("Authorization"), "")
		_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer override.Close()
	client.PrometheusUrl = override.URL
	_, err = client.QueryRange(context.Background(), RangeInput{})
	assert.NoErr(err)
}

// TestStoreQueries owns real HTTP encoding, stream ordering and unit-preserving
// decoding, including gaps represented by nonfinite Prometheus samples.
func TestStoreQueries(t *testing.T) {
	assert := is.New(t)
	start := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		switch request.URL.Path {
		case "/api/v1/query_range":
			assert.Equal(query.Get("query"), `sum(rate(cpu{namespace="one"}[2m]))`)
			assert.Equal(query.Get("step"), "30")
			assert.Equal(query.Get("start"), start.Format(time.RFC3339Nano))
			_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1792803600,"0.25"],[1792803630,"NaN"],[1792803660,"+Inf"],[1792803690,"0"]]}]}}`))
		case "/loki/api/v1/query_range":
			assert.Equal(query.Get("query"), `{app="web"} |= "error"`)
			assert.Equal(query.Get("direction"), "backward")
			assert.Equal(query.Get("limit"), "2")
			_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"b","container":"web","borq_revision":"abc123","image":"nginx:1.27.5","replica_set":"7b89c56dd8"},"values":[["1792803600000000002","<script>bad()</script>"]]},{"stream":{"pod":"a","container":"web"},"values":[["1792803600000000003","newest"],["1792803600000000001","oldest"]]}]}}`))
		default:
			t.Errorf("unexpected query path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client := Client{PrometheusUrl: server.URL, LokiUrl: server.URL}
	series, err := client.QueryRange(context.Background(), RangeInput{Query: `sum(rate(cpu{namespace="one"}[2m]))`, Start: start, End: start.Add(time.Hour), Step: 30 * time.Second})
	assert.NoErr(err)
	assert.Equal(len(series), 2)
	assert.Equal(series[0], Sample{Timestamp: start, Value: 0.25})
	assert.Equal(series[1].Value, 0.0)
	logs, err := client.Logs(context.Background(), LogsInput{Query: `{app="web"} |= "error"`, Start: start, End: start.Add(time.Hour), Limit: 2})
	assert.NoErr(err)
	assert.Equal(len(logs), 2)
	assert.Equal(logs[0].Pod, "b")
	assert.Equal(logs[0].Revision, "abc123")
	assert.Equal(logs[0].Image, "nginx:1.27.5")
	assert.Equal(logs[0].ReplicaSet, "7b89c56dd8")
	assert.Equal(logs[0].Message, "<script>bad()</script>")
	assert.Equal(logs[1].Message, "newest")
	assert.Equal(logs[1].Timestamp.UnixNano(), int64(1792803600000000003))
}

// TestStoreFailures rejects backend errors and malformed successful responses
// rather than presenting an outage or invalid sample as an empty history.
func TestStoreFailures(t *testing.T) {
	for _, input := range []struct {
		name, body string
		status     int
		logs       bool
	}{
		{"unavailable", "private backend diagnostic", 503, false},
		{"redirect", "", 302, false},
		{"invalid JSON", "{", 200, false},
		{"query error", `{"status":"error"}`, 200, false},
		{"wrong result", `{"status":"success","data":{"resultType":"vector"}}`, 200, false},
		{"invalid metric", `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"bad"]]}]}}`, 200, false},
		{"invalid time", `{"status":"success","data":{"resultType":"streams","result":[{"values":[["bad","message"]]}]}}`, 200, true},
		{"log query error", `{"status":"error"}`, 200, true},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(input.status)
				_, _ = fmt.Fprint(writer, input.body)
			}))
			defer server.Close()
			client := Client{PrometheusUrl: server.URL, LokiUrl: server.URL}
			var err error
			if input.logs {
				_, err = client.Logs(context.Background(), LogsInput{Limit: 10})
			} else {
				_, err = client.QueryRange(context.Background(), RangeInput{})
			}
			assert.True(err != nil)
			assert.True(!strings.Contains(err.Error(), "private backend diagnostic"))
		})
	}
}
