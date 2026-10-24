// Package observability reads bounded Prometheus and Loki query results.
package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"k8s.io/client-go/rest"
)

// Client queries the management stores without exposing their wire formats.
type Client struct {
	// A value client keeps the authenticated Kubernetes transport private to management stores.
	httpClient     http.Client
	managementUrls [2]string
	// PrometheusUrl is the private query endpoint, separate from remote write.
	PrometheusUrl string
	// LokiUrl is the private query endpoint, separate from log ingestion.
	LokiUrl string
}

// Sample is a finite measurement; missing samples remain gaps in the series.
type Sample struct {
	// Timestamp is the UTC observation time.
	Timestamp time.Time `json:"timestamp"`
	// Value is expressed in the unit documented by the caller.
	Value float64 `json:"value"`
}

// Entry is one log line, preserving nanosecond time and its originating container.
type Entry struct {
	// Timestamp is the UTC emission time.
	Timestamp time.Time `json:"timestamp"`
	// Pod identifies the replica emitting this line.
	Pod string `json:"pod"`
	// Container identifies the emitting container.
	Container string `json:"container"`
	// Revision identifies the immutable app runtime configuration; empty on older streams.
	Revision string `json:"revision"`
	// Image includes the image tag or digest observed on the emitting pod.
	Image string `json:"image"`
	// ReplicaSet identifies the Kubernetes rollout, including pods collected before revision labels.
	ReplicaSet string `json:"replicaSet"`
	// Message is untrusted application output and must be escaped when displayed.
	Message string `json:"message"`
}

// RangeInput bounds one PromQL range query.
type RangeInput struct {
	// Query is the complete PromQL expression supplied by the owning workflow.
	Query string
	// Start is the first requested evaluation time.
	Start time.Time
	// End is the last requested evaluation time.
	End time.Time
	// Step controls evaluation granularity.
	Step time.Duration
}

// QueryRange returns an aggregate series in time order, omitting nonfinite values.
func (client Client) QueryRange(ctx context.Context, input RangeInput) ([]Sample, error) {
	parameters := url.Values{"query": {input.Query}, "start": {input.Start.Format(time.RFC3339Nano)}, "end": {input.End.Format(time.RFC3339Nano)}, "step": {strconv.FormatFloat(input.Step.Seconds(), 'f', -1, 64)}}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := client.query(ctx, client.PrometheusUrl, "/api/v1/query_range", parameters, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" || len(response.Data.Result) > 1 {
		return nil, fmt.Errorf("unexpected Prometheus range result")
	}
	samples := []Sample{}
	for _, series := range response.Data.Result {
		for _, pair := range series.Values {
			var seconds float64
			var raw string
			if json.Unmarshal(pair[0], &seconds) != nil || json.Unmarshal(pair[1], &raw) != nil {
				return nil, fmt.Errorf("invalid Prometheus sample")
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid Prometheus value")
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			samples = append(samples, Sample{Timestamp: time.UnixMilli(int64(seconds * 1000)).UTC(), Value: value})
		}
	}
	sort.Slice(samples, func(left, right int) bool { return samples[left].Timestamp.Before(samples[right].Timestamp) })
	return samples, nil
}

// LogsInput bounds a LogQL query and its returned line count.
type LogsInput struct {
	// Query is a scoped stream selector with optional literal filtering.
	Query string
	// Start and End bound the requested log history.
	Start, End time.Time
	// Limit caps the number of lines returned across all streams.
	Limit int
}

// Logs returns the newest matching lines, sorted oldest first for reading.
func (client Client) Logs(ctx context.Context, input LogsInput) ([]Entry, error) {
	parameters := url.Values{"query": {input.Query}, "start": {strconv.FormatInt(input.Start.UnixNano(), 10)}, "end": {strconv.FormatInt(input.End.UnixNano(), 10)}, "direction": {"backward"}, "limit": {strconv.Itoa(input.Limit)}}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := client.query(ctx, client.LokiUrl, "/loki/api/v1/query_range", parameters, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" || response.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected Loki log result")
	}
	entries := []Entry{}
	for _, stream := range response.Data.Result {
		for _, pair := range stream.Values {
			nanoseconds, err := strconv.ParseInt(pair[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid Loki timestamp")
			}
			entries = append(entries, Entry{Timestamp: time.Unix(0, nanoseconds).UTC(), Pod: stream.Stream["pod"], Container: stream.Stream["container"], Revision: stream.Stream["borq_revision"], Image: stream.Stream["image"], ReplicaSet: stream.Stream["replica_set"], Message: pair[1]})
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].Timestamp.Equal(entries[right].Timestamp) {
			return entries[left].Pod < entries[right].Pod
		}
		return entries[left].Timestamp.Before(entries[right].Timestamp)
	})
	if len(entries) > input.Limit {
		entries = entries[len(entries)-input.Limit:]
	}
	return entries, nil
}

// The output pointer is required by the JSON decoder at this transport boundary.
func (store Client) query(ctx context.Context, base, path string, parameters url.Values, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path+"?"+parameters.Encode(), nil)
	if err != nil {
		return fmt.Errorf("create telemetry query: %w", err)
	}
	// Private management traffic must not pass through an outbound HTTP proxy.
	client := http.Client{}
	if base == store.managementUrls[0] || base == store.managementUrls[1] {
		client = store.httpClient
	}
	if client.Transport == nil {
		transport := &http.Transport{Proxy: nil}
		defer transport.CloseIdleConnections()
		client.Transport = transport
	}
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("query telemetry store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry store returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return fmt.Errorf("telemetry response exceeds limit or could not be read")
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode telemetry response: %w", err)
	}
	return nil
}

// NewManagementClient reaches the private stores through the selected Kubernetes
// Service proxy, keeping in-cluster and external development connections equivalent.
func NewManagementClient(kubeconfigPath, namespace string) (Client, error) {
	if namespace == "" {
		namespace = "borq"
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	if err != nil {
		return Client{}, err
	}
	configuration := connection.RestConfig()
	// The SDK requires shared pointer ownership of its configured HTTP transport.
	httpClient, err := rest.HTTPClientFor(&configuration)
	if err != nil {
		return Client{}, fmt.Errorf("configure management query transport: %w", err)
	}
	base := strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/"
	prometheusUrl, lokiUrl := base+"http:prometheus-server:80/proxy", base+"http:loki:3100/proxy"
	return Client{PrometheusUrl: prometheusUrl, LokiUrl: lokiUrl, httpClient: *httpClient, managementUrls: [2]string{prometheusUrl, lokiUrl}}, nil
}
