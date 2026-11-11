package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

// QueryInput evaluates one PromQL expression at a fixed time.
type QueryInput struct {
	// Query is the complete PromQL expression supplied by the owning workflow.
	Query string
	// Time is the evaluation time shared by related snapshot queries.
	Time time.Time
}

// VectorSample retains the labels needed to match a measurement to its resource.
type VectorSample struct {
	// Labels identifies the series returned by Prometheus.
	Labels map[string]string
	// Sample holds the finite value and its returned timestamp.
	Sample Sample
}

// Query returns an instant vector, omitting nonfinite measurements.
func (client Client) Query(ctx context.Context, input QueryInput) ([]VectorSample, error) {
	parameters := url.Values{"query": {input.Query}, "time": {input.Time.Format(time.RFC3339Nano)}}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := client.query(ctx, client.PrometheusUrl, "/api/v1/query", parameters, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("unexpected Prometheus instant result")
	}
	samples := []VectorSample{}
	for _, series := range response.Data.Result {
		var seconds float64
		var raw string
		if json.Unmarshal(series.Value[0], &seconds) != nil || json.Unmarshal(series.Value[1], &raw) != nil {
			return nil, fmt.Errorf("invalid Prometheus sample")
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Prometheus value")
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		samples = append(samples, VectorSample{Labels: series.Metric, Sample: Sample{Timestamp: time.UnixMilli(int64(seconds * 1000)).UTC(), Value: value}})
	}
	return samples, nil
}
