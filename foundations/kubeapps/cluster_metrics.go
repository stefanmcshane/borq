package kubeapps

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ClusterMetrics is a live, read-only snapshot across all cluster namespaces.
type ClusterMetrics struct {
	// ObservedAt is when the Kubernetes resource snapshot was read.
	ObservedAt time.Time `json:"observedAt"`
	// Nodes reports readiness, pressure, allocatable capacity and measured usage.
	Nodes []NodeMetrics `json:"nodes"`
	// Pods counts current pod phases and container restarts across all namespaces.
	Pods ClusterPods `json:"pods"`
	// UsageNotice explains missing or partial CPU and memory measurements.
	UsageNotice string `json:"usageNotice"`
}

// NodeMetrics separates missing measurements from a measured zero.
type NodeMetrics struct {
	// Name identifies the Kubernetes node.
	Name string `json:"name"`
	// Ready is true only when the node's Ready condition is explicitly true.
	Ready bool `json:"ready"`
	// Unschedulable identifies nodes cordoned against new workloads.
	Unschedulable bool `json:"unschedulable"`
	// Conditions lists reported memory, disk, PID and network pressure conditions.
	Conditions []string `json:"conditions"`
	// CpuAllocatableCores is CPU capacity available to pods, in cores.
	CpuAllocatableCores float64 `json:"cpuAllocatableCores"`
	// MemoryAllocatableBytes is memory capacity available to pods, in bytes.
	MemoryAllocatableBytes int64 `json:"memoryAllocatableBytes"`
	// DiskCapacityAvailable means the node reports valid local ephemeral storage capacity.
	DiskCapacityAvailable bool `json:"diskCapacityAvailable"`
	// DiskCapacityBytes is total node ephemeral storage capacity, excluding persistent volumes.
	DiskCapacityBytes int64 `json:"diskCapacityBytes"`
	// DiskAllocatableBytes is ephemeral storage capacity available to pods, not current free space.
	DiskAllocatableBytes int64 `json:"diskAllocatableBytes"`
	// DiskUsageAvailable means recent node filesystem usage and capacity were read together.
	DiskUsageAvailable bool `json:"diskUsageAvailable"`
	// DiskUsageBytes is measured node filesystem consumption, not allocatable capacity.
	DiskUsageBytes int64 `json:"diskUsageBytes"`
	// DiskUsageCapacityBytes is capacity from the same filesystem sample as DiskUsageBytes.
	DiskUsageCapacityBytes int64 `json:"diskUsageCapacityBytes"`
	// DiskUsageObservedAt is the kubelet filesystem sample timestamp.
	DiskUsageObservedAt time.Time `json:"diskUsageObservedAt"`
	// PodCapacity is the node's reported allocatable pod count.
	PodCapacity int64 `json:"podCapacity"`
	// UsageAvailable means both CPU and memory have a recent valid measurement.
	UsageAvailable bool `json:"usageAvailable"`
	// CpuUsageCores is measured CPU consumption in cores when UsageAvailable.
	CpuUsageCores float64 `json:"cpuUsageCores"`
	// MemoryUsageBytes is measured memory working set in bytes when UsageAvailable.
	MemoryUsageBytes int64 `json:"memoryUsageBytes"`
	// UsageObservedAt is the oldest source sample timestamp used for Prometheus usage.
	UsageObservedAt time.Time `json:"usageObservedAt"`
}

// ClusterPods describes observed workloads, independent of Borq desired state.
type ClusterPods struct {
	// Running counts pods in the Running phase.
	Running int `json:"running"`
	// Pending counts pods waiting to run.
	Pending int `json:"pending"`
	// Failed counts retained pods in the Failed phase.
	Failed int `json:"failed"`
	// Succeeded counts retained pods that completed successfully.
	Succeeded int `json:"succeeded"`
	// Unknown counts pods without a recognized current phase.
	Unknown int `json:"unknown"`
	// Restarts totals restart counts for current regular and init containers.
	Restarts int64 `json:"restarts"`
}

// ClusterMetrics reads node health, capacity, disk usage and pod phases from Kubernetes.
func (client Client) ClusterMetrics(ctx context.Context) (ClusterMetrics, error) {
	result := ClusterMetrics{ObservedAt: time.Now().UTC(), Nodes: []NodeMetrics{}}
	nodes, err := client.listClusterResources(ctx, schema.GroupVersionResource{Version: "v1", Resource: "nodes"})
	if err != nil {
		return result, fmt.Errorf("read cluster nodes: %w", err)
	}
	pods, err := client.listClusterResources(ctx, schema.GroupVersionResource{Version: "v1", Resource: "pods"})
	if err != nil {
		return result, fmt.Errorf("read cluster pods: %w", err)
	}
	for _, pod := range pods {
		switch textAt(pod.Object, "status", "phase") {
		case "Running":
			result.Pods.Running++
		case "Pending":
			result.Pods.Pending++
		case "Failed":
			result.Pods.Failed++
		case "Succeeded":
			result.Pods.Succeeded++
		default:
			result.Pods.Unknown++
		}
		for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
			statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", field)
			for _, status := range statuses {
				container, _ := status.(map[string]any)
				result.Pods.Restarts += integerAt(container, "restartCount")
			}
		}
	}
	for _, node := range nodes {
		cpu, cpuError := resource.ParseQuantity(textAt(node.Object, "status", "allocatable", "cpu"))
		memory, memoryError := resource.ParseQuantity(textAt(node.Object, "status", "allocatable", "memory"))
		capacity, capacityError := resource.ParseQuantity(textAt(node.Object, "status", "allocatable", "pods"))
		if cpuError != nil || memoryError != nil || capacityError != nil {
			return result, fmt.Errorf("node %s has invalid allocatable capacity", node.GetName())
		}
		summary := NodeMetrics{Name: node.GetName(), Conditions: []string{}, CpuAllocatableCores: cpu.AsApproximateFloat64(), MemoryAllocatableBytes: memory.Value(), PodCapacity: capacity.Value()}
		diskCapacity, diskCapacityError := resource.ParseQuantity(textAt(node.Object, "status", "capacity", "ephemeral-storage"))
		diskAllocatable, diskAllocatableError := resource.ParseQuantity(textAt(node.Object, "status", "allocatable", "ephemeral-storage"))
		if diskCapacityError == nil && diskAllocatableError == nil && diskCapacity.Sign() > 0 && diskAllocatable.Sign() >= 0 && diskAllocatable.Cmp(diskCapacity) <= 0 {
			summary.DiskCapacityAvailable = true
			summary.DiskCapacityBytes, summary.DiskAllocatableBytes = diskCapacity.Value(), diskAllocatable.Value()
		}
		summary.Unschedulable, _, _ = unstructured.NestedBool(node.Object, "spec", "unschedulable")
		conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
		for _, value := range conditions {
			condition, _ := value.(map[string]any)
			if textAt(condition, "status") != "True" {
				continue
			}
			switch textAt(condition, "type") {
			case "Ready":
				summary.Ready = true
			case "MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable":
				summary.Conditions = append(summary.Conditions, textAt(condition, "type"))
			}
		}
		result.Nodes = append(result.Nodes, summary)
	}
	result.Nodes = client.readNodeDiskUsage(ctx, result.Nodes, result.ObservedAt)
	sort.Slice(result.Nodes, func(left, right int) bool { return result.Nodes[left].Name < result.Nodes[right].Name })
	return result, nil
}

func (client Client) readNodeDiskUsage(ctx context.Context, nodes []NodeMetrics, observedAt time.Time) []NodeMetrics {
	// Optional filesystem reads share a deadline so unavailable kubelets cannot hide node health.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(nodes)) {
		workers.Go(func() {
			for index := range jobs {
				nodes[index] = client.withNodeDiskUsage(ctx, nodes[index], observedAt)
			}
		})
	}
	for index := range nodes {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return nodes
}

func (client Client) withNodeDiskUsage(ctx context.Context, node NodeMetrics, observedAt time.Time) NodeMetrics {
	if ctx.Err() != nil {
		return node
	}
	data, err := client.discovery.RESTClient().Get().AbsPath("/api/v1/nodes", node.Name, "proxy/stats/summary").Do(ctx).Raw()
	if err != nil {
		return node
	}
	var summary struct {
		Node struct {
			Name string `json:"nodeName"`
			Fs   struct {
				Time time.Time `json:"time"`
				// Kubelet omits missing measurements; pointers preserve a measured zero.
				UsedBytes     *int64 `json:"usedBytes"`
				CapacityBytes *int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if json.Unmarshal(data, &summary) != nil || summary.Node.Name != node.Name {
		return node
	}
	filesystem := summary.Node.Fs
	if filesystem.UsedBytes == nil || filesystem.CapacityBytes == nil || *filesystem.UsedBytes < 0 || *filesystem.CapacityBytes <= 0 || *filesystem.UsedBytes > *filesystem.CapacityBytes || filesystem.Time.Before(observedAt.Add(-5*time.Minute)) || filesystem.Time.After(observedAt.Add(time.Minute)) {
		return node
	}
	node.DiskUsageAvailable = true
	node.DiskUsageBytes, node.DiskUsageCapacityBytes = *filesystem.UsedBytes, *filesystem.CapacityBytes
	node.DiskUsageObservedAt = filesystem.Time
	return node
}

func (client Client) listClusterResources(ctx context.Context, resource schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	items := []unstructured.Unstructured{}
	options := metav1.ListOptions{Limit: 500}
	for {
		page, err := client.dynamic.Resource(resource).List(ctx, options)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		options.Continue = page.GetContinue()
		if options.Continue == "" {
			return items, nil
		}
	}
}
