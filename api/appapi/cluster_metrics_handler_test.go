package appapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestClusterMetricsReadback owns the complete saved-link observation path:
// pagination, cluster-scoped Prometheus usage, pod health, disk capacity and missing measurements.
// The fixture serves observations only; it does not simulate controller success.
func TestClusterMetricsReadback(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	mode := "complete"
	var observations atomic.Int64
	now := time.Now().UTC().Truncate(time.Second)
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/version" {
			_, _ = writer.Write([]byte(`{"gitVersion":"v1.35.3"}`))
			return
		}
		observations.Add(1)
		switch request.URL.Path {
		case "/api/v1/nodes":
			if mode == "node access denied" {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","reason":"Forbidden","message":"private-cluster-detail","code":403}`))
				return
			}
			if request.URL.Query().Get("continue") == "next-node" {
				_, _ = writer.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","items":[{"metadata":{"name":"node-a"},"status":{"capacity":{"ephemeral-storage":"100Gi"},"allocatable":{"ephemeral-storage":"90Gi","cpu":"2","memory":"4Gi","pods":"110"},"conditions":[{"type":"Ready","status":"True"}]}}]}`))
				return
			}
			assert.Equal(request.URL.Query().Get("limit"), "500")
			diskCapacity := "200Gi"
			if mode == "missing disk capacity" {
				diskCapacity = ""
			}
			if mode == "invalid disk capacity" {
				diskCapacity = "invalid"
			}
			if mode == "negative disk capacity" {
				diskCapacity = "-1Gi"
			}
			if mode == "inconsistent disk capacity" {
				diskCapacity = "100Gi"
			}
			_, _ = writer.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","metadata":{"continue":"next-node"},"items":[{"metadata":{"name":"node-b"},"spec":{"unschedulable":true},"status":{"capacity":{"ephemeral-storage":"` + diskCapacity + `"},"allocatable":{"ephemeral-storage":"180Gi","cpu":"1500m","memory":"2Gi","pods":"80"},"conditions":[{"type":"Ready","status":"False"},{"type":"DiskPressure","status":"True"},{"type":"MemoryPressure","status":"False"}]}}]}`))
		case "/api/v1/nodes/node-a/proxy/stats/summary", "/api/v1/nodes/node-b/proxy/stats/summary":
			if request.Header.Get("Authorization") != "Bearer private-observer-token" {
				t.Error("filesystem request omitted the registered cluster credentials")
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			if mode == "disk metrics unavailable" {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","reason":"Forbidden","message":"private-kubelet-detail","code":403}`))
				return
			}
			name, diskCapacity, used := "node-a", int64(64*1024*1024*1024), int64(8*1024*1024*1024)
			timestamp := now
			if strings.Contains(request.URL.Path, "/node-b/") {
				name, diskCapacity, used = "node-b", 128*1024*1024*1024, 0
				switch mode {
				case "malformed disk sample":
					_, _ = writer.Write([]byte(`{`))
					return
				case "wrong disk node":
					name = "different-node"
				case "stale disk sample":
					timestamp = now.Add(-10 * time.Minute)
				case "future disk sample":
					timestamp = now.Add(10 * time.Minute)
				case "negative disk usage":
					used = -1
				case "excessive disk usage":
					used = diskCapacity + 1
				case "zero disk sample capacity":
					diskCapacity = 0
				}
			}
			filesystem := map[string]any{"time": timestamp.Format(time.RFC3339), "usedBytes": used, "capacityBytes": diskCapacity}
			if name == "node-b" && mode == "missing disk usage" {
				delete(filesystem, "usedBytes")
			}
			if name == "node-b" && mode == "missing disk sample capacity" {
				delete(filesystem, "capacityBytes")
			}
			node := map[string]any{"nodeName": name, "fs": filesystem}
			if name == "node-b" && mode == "missing disk filesystem" {
				delete(node, "fs")
			}
			assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"node": node}))
		case "/api/v1/pods":
			_, _ = writer.Write([]byte(`{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"app-space"},"status":{"phase":"Running","containerStatuses":[{"restartCount":2}],"initContainerStatuses":[{"restartCount":1}]}},{"metadata":{"namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"restartCount":4}]}},{"status":{"phase":"Pending"}},{"status":{"phase":"Failed"}},{"status":{"phase":"Succeeded"}},{"status":{}}]}`))
		default:
			t.Errorf("unexpected cluster observation: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(cluster.Close)
	clusterID := ""
	prometheus := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observations.Add(1)
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/query")
		assert.Equal(request.Header.Get("Authorization"), "")
		query := request.URL.Query().Get("query")
		assert.True(strings.Contains(query, `borq_cluster=`+strconv.Quote(clusterID)+`,`))
		assert.True(strings.Contains(query, `job="borq-cadvisor",id="/"`))
		evaluation, err := time.Parse(time.RFC3339Nano, request.URL.Query().Get("time"))
		assert.NoErr(err)
		if mode == "metrics unavailable" {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(writer, "private-prometheus-diagnostic")
			return
		}
		first, second := "0.15", "0"
		timestampQuery := strings.Contains(query, "timestamp(")
		switch {
		case timestampQuery:
			first, second = strconv.FormatInt(now.Unix(), 10), strconv.FormatInt(now.Unix(), 10)
			if mode == "stale measurement" {
				second = strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
			}
			if mode == "future measurement" {
				second = strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
			}
		case strings.Contains(query, "rate("):
			assert.True(strings.Contains(query, `cpu="total"}[2m]`))
			if mode == "invalid measurement" {
				second = "NaN"
			}
			if mode == "negative cpu" {
				second = "-1"
			}
		case strings.Contains(query, "container_memory_working_set_bytes"):
			first = "268435456"
			if mode == "negative memory" {
				second = "-1"
			}
			if mode == "overflow memory" {
				second = "9223372036854775808"
			}
		default:
			t.Errorf("unexpected Prometheus query: %s", query)
		}
		items := []map[string]any{{"metric": map[string]string{"node": "node-a"}, "value": []any{evaluation.Unix(), first}}}
		if mode != "partial measurement" && !(mode == "missing source timestamp" && timestampQuery) {
			items = append(items, map[string]any{"metric": map[string]string{"node": "node-b"}, "value": []any{evaluation.Unix(), second}})
		}
		// Return an extra node to ensure only the saved cluster's live inventory is used.
		items = append(items, map[string]any{"metric": map[string]string{"node": "removed-node"}, "value": []any{evaluation.Unix(), "42"}})
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": items}}))
	}))
	t.Cleanup(prometheus.Close)
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "clusters.sqlite"), PrometheusQueryUrl: prometheus.URL})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	_, err = manager.CreateOrganization(ctx, "team")
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, "other")
	assert.NoErr(err)
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}))
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    server: " + cluster.URL + "\n    certificate-authority-data: " + certificate + "\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: observer\ncurrent-context: target\nusers:\n- name: observer\n  user:\n    token: private-observer-token\n"
	registered, err := manager.RegisterCluster(ctx, apps.ClusterInput{Organization: "team", Name: "target", Kubeconfig: kubeconfig})
	assert.NoErr(err)
	clusterID = registered.ID
	api, err := NewHandler(manager)
	assert.NoErr(err)
	for _, scenario := range []string{"complete", "partial measurement", "stale measurement", "future measurement", "invalid measurement", "negative cpu", "negative memory", "overflow memory", "missing source timestamp", "metrics unavailable", "missing disk capacity", "invalid disk capacity", "negative disk capacity", "inconsistent disk capacity", "disk metrics unavailable", "missing disk filesystem", "missing disk usage", "missing disk sample capacity", "stale disk sample", "future disk sample", "wrong disk node", "negative disk usage", "excessive disk usage", "zero disk sample capacity", "malformed disk sample", "node access denied"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			mode = scenario
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/organizations/team/clusters/target/metrics", nil))
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			assert.True(!strings.Contains(response.Body.String(), "private-"))
			if mode == "node access denied" {
				assert.Equal(response.Code, http.StatusInternalServerError)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			var metrics ClusterMetrics
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &metrics))
			assert.Equal(len(metrics.Nodes), 2)
			assert.Equal(metrics.Nodes[0].Name, "node-a")
			assert.Equal(metrics.Nodes[1].Name, "node-b")
			assert.True(metrics.Nodes[0].Ready)
			assert.True(!metrics.Nodes[1].Ready)
			assert.True(metrics.Nodes[1].Unschedulable)
			assert.Equal(metrics.Nodes[1].Conditions, []string{"DiskPressure"})
			assert.Equal(metrics.Nodes[0].CPUAllocatableCores, 2.0)
			assert.Equal(metrics.Nodes[1].CPUAllocatableCores, 1.5)
			assert.Equal(metrics.Nodes[0].MemoryAllocatableBytes, int64(4*1024*1024*1024))
			assert.Equal(metrics.Nodes[1].PodCapacity, int64(80))
			assert.True(metrics.Nodes[0].DiskCapacityAvailable)
			assert.Equal(metrics.Nodes[0].DiskCapacityBytes, int64(100*1024*1024*1024))
			assert.Equal(metrics.Nodes[0].DiskAllocatableBytes, int64(90*1024*1024*1024))
			if strings.Contains(mode, "disk capacity") {
				assert.True(!metrics.Nodes[1].DiskCapacityAvailable)
				assert.Equal(metrics.Nodes[1].DiskCapacityBytes, int64(0))
			} else {
				assert.True(metrics.Nodes[1].DiskCapacityAvailable)
				assert.Equal(metrics.Nodes[1].DiskCapacityBytes, int64(200*1024*1024*1024))
				assert.Equal(metrics.Nodes[1].DiskAllocatableBytes, int64(180*1024*1024*1024))
			}
			if mode == "disk metrics unavailable" {
				assert.True(!metrics.Nodes[0].DiskUsageAvailable)
				assert.True(!metrics.Nodes[1].DiskUsageAvailable)
			} else {
				assert.True(metrics.Nodes[0].DiskUsageAvailable)
				assert.Equal(metrics.Nodes[0].DiskUsageBytes, int64(8*1024*1024*1024))
				assert.Equal(metrics.Nodes[0].DiskUsageCapacityBytes, int64(64*1024*1024*1024))
				assert.Equal(metrics.Nodes[0].DiskUsageObservedAt, now)
				invalidDiskSample := strings.Contains(mode, "disk") && !strings.Contains(mode, "disk capacity")
				assert.Equal(metrics.Nodes[1].DiskUsageAvailable, !invalidDiskSample)
				if invalidDiskSample {
					assert.Equal(metrics.Nodes[1].DiskUsageBytes, int64(0))
					assert.Equal(metrics.Nodes[1].DiskUsageCapacityBytes, int64(0))
				} else {
					assert.Equal(metrics.Nodes[1].DiskUsageBytes, int64(0))
					assert.Equal(metrics.Nodes[1].DiskUsageCapacityBytes, int64(128*1024*1024*1024))
				}
			}
			assert.Equal(metrics.Pods, ClusterPodMetrics{Running: 2, Pending: 1, Failed: 1, Succeeded: 1, Unknown: 1, Restarts: 7})
			if mode == "metrics unavailable" {
				assert.True(!metrics.Nodes[0].UsageAvailable)
				assert.True(!metrics.Nodes[1].UsageAvailable)
				assert.True(strings.Contains(metrics.UsageNotice, "Prometheus"))
				return
			}
			assert.True(metrics.Nodes[0].UsageAvailable)
			assert.True(math.Abs(metrics.Nodes[0].CPUUsageCores-0.15) < 0.000001)
			assert.Equal(metrics.Nodes[0].MemoryUsageBytes, int64(256*1024*1024))
			assert.Equal(metrics.Nodes[0].UsageObservedAt, now)
			completeUsage := mode == "complete" || strings.Contains(mode, "disk")
			assert.Equal(metrics.Nodes[1].UsageAvailable, completeUsage)
			if completeUsage {
				assert.Equal(metrics.Nodes[1].CPUUsageCores, 0.0)
				assert.Equal(metrics.Nodes[1].MemoryUsageBytes, int64(0))
				assert.Equal(metrics.UsageNotice, "")
			} else {
				assert.True(strings.Contains(metrics.UsageNotice, "1 of 2 nodes"))
			}
		})
	}
	before := observations.Load()
	for _, path := range []string{"/api/v1/organizations/other/clusters/target/metrics", "/api/v1/organizations/team/clusters/missing/metrics"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(response.Code, http.StatusNotFound)
		assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	}
	assert.Equal(observations.Load(), before)
}
