package observability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestInstantQuery owns instant-vector wire decoding, including labels and gaps,
// without turning malformed responses or backend outages into measured zeroes.
func TestInstantQuery(t *testing.T) {
	for _, input := range []struct {
		name, body string
		status     int
		count      int
		failure    bool
	}{
		{"samples", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"node":"node-a"},"value":[123.5,"0"]},{"metric":{"node":"node-b"},"value":[124,"NaN"]},{"value":[124,"+Inf"]}]}}`, 200, 1, false},
		{"empty", `{"status":"success","data":{"resultType":"vector","result":[]}}`, 200, 0, false},
		{"query error", `{"status":"error"}`, 200, 0, true},
		{"wrong result", `{"status":"success","data":{"resultType":"matrix","result":[]}}`, 200, 0, true},
		{"invalid sample", `{"status":"success","data":{"resultType":"vector","result":[{"value":["bad","1"]}]}}`, 200, 0, true},
		{"missing value", `{"status":"success","data":{"resultType":"vector","result":[{}]}}`, 200, 0, true},
		{"invalid value", `{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"bad"]}]}}`, 200, 0, true},
		{"unavailable", "private-store-diagnostic", 503, 0, true},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/v1/query")
				assert.Equal(request.URL.Query().Get("query"), `metric{node="a&b"}`)
				assert.Equal(request.URL.Query().Get("time"), at.Format(time.RFC3339Nano))
				writer.WriteHeader(input.status)
				_, _ = fmt.Fprint(writer, input.body)
			}))
			defer server.Close()
			client := Client{PrometheusUrl: server.URL}
			samples, err := client.Query(context.Background(), QueryInput{Query: `metric{node="a&b"}`, Time: at})
			assert.Equal(err != nil, input.failure)
			if err != nil {
				assert.True(!strings.Contains(err.Error(), "private-store-diagnostic"))
				return
			}
			assert.Equal(len(samples), input.count)
			if input.count > 0 {
				assert.Equal(samples[0].Labels, map[string]string{"node": "node-a"})
				assert.Equal(samples[0].Sample, Sample{Timestamp: time.UnixMilli(123500).UTC(), Value: 0})
			}
		})
	}
}
