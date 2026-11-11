package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

type clusterMetricsData struct {
	// Location refreshes the same organization-to-cluster link.
	Location string
	// Problem reports unavailable cluster observations without hiding navigation.
	Problem string
	// Snapshot retains the measurement time, pod phases and partial usage notice.
	Snapshot appapi.ClusterMetrics
	// ReadyNodes and PressureNodes summarize observed readiness and pressure.
	ReadyNodes, PressureNodes int
	// ActivePods excludes completed and failed pods when comparing with pod capacity.
	ActivePods int
	// PodCapacity is the total allocatable pod count reported by nodes.
	PodCapacity int64
	// Cpu and Memory compare usage with allocatable capacity; Disk uses measured filesystem capacity.
	Cpu, Memory, Disk clusterCapacityData
	// Nodes presents per-node usage without replacing missing values with zeroes.
	Nodes []clusterNodeData
}

type clusterCapacityData struct {
	// Title, Value and Capacity label the observed resource and its reported capacity.
	Title, Value, Capacity string
	// Available distinguishes a complete usage measurement from partial or missing data.
	Available bool
	// Percent is measured consumption divided by the corresponding resource capacity.
	Percent float64
}

type clusterNodeData struct {
	// Name and State identify the node and its explicit Ready condition.
	Name, State string
	// Unschedulable indicates a cordoned node independently from readiness.
	Unschedulable bool
	// Pressure translates the observed condition types into readable labels.
	Pressure string
	// Cpu and Memory distinguish missing readings from actual zero consumption.
	Cpu, Memory string
	// CpuCapacity and MemoryCapacity present node allocatable capacity.
	CpuCapacity, MemoryCapacity string
	// DiskCapacity and DiskAllocatable distinguish reported storage from missing data.
	DiskCapacity, DiskAllocatable string
}

func (web handler) clusterMetrics(writer http.ResponseWriter, request *http.Request) {
	selection := request.URL.Query()
	organization, cluster := selection.Get("organization"), selection.Get("cluster")
	if organization == "" || cluster == "" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an organization and cluster link."})
		return
	}
	location := "/api/ui/cluster?" + url.Values{"organization": {organization}, "cluster": {cluster}}.Encode()
	snapshot, err := requestApi[appapi.ClusterMetrics](request.Context(), web.api, http.MethodGet, organizationPath(organization)+"/clusters/"+url.PathEscape(cluster)+"/metrics", nil)
	if problem, ok := err.(apiProblem); ok && (problem.status == http.StatusBadRequest || problem.status == http.StatusNotFound) {
		web.problem(writer, request, err)
		return
	}
	data := makeClusterMetricsData(snapshot)
	data.Location = location
	if err != nil {
		data.Problem = "Could not read cluster metrics. Check the cluster connection and permission to read nodes and pods."
	}
	web.respond(writer, request, "cluster-metrics", data, http.StatusOK)
}

func makeClusterMetricsData(snapshot appapi.ClusterMetrics) clusterMetricsData {
	data := clusterMetricsData{Snapshot: snapshot, ActivePods: snapshot.Pods.Running + snapshot.Pods.Pending + snapshot.Pods.Unknown}
	var cpuCapacity, cpuUsage float64
	var memoryCapacity, memoryUsage int64
	var diskCapacity int64
	var diskUsage, diskUsageCapacity int64
	usageAvailable := len(snapshot.Nodes) > 0
	diskCapacityAvailable, diskUsageAvailable := len(snapshot.Nodes) > 0, len(snapshot.Nodes) > 0
	for _, node := range snapshot.Nodes {
		cpuCapacity += float64(node.CPUAllocatableCores)
		memoryCapacity += node.MemoryAllocatableBytes
		data.PodCapacity += node.PodCapacity
		state := "Not ready"
		if node.Ready {
			data.ReadyNodes++
			state = "Ready"
		}
		var conditions []string
		for _, condition := range node.Conditions {
			switch condition {
			case "MemoryPressure":
				conditions = append(conditions, "Memory pressure")
			case "DiskPressure":
				conditions = append(conditions, "Disk pressure")
			case "PIDPressure":
				conditions = append(conditions, "Process pressure")
			case "NetworkUnavailable":
				conditions = append(conditions, "Network unavailable")
			}
		}
		if len(conditions) > 0 {
			data.PressureNodes++
		}
		row := clusterNodeData{Name: node.Name, State: state, Unschedulable: node.Unschedulable, Pressure: strings.Join(conditions, ", "), Cpu: "Unavailable", Memory: "Unavailable", CpuCapacity: fmt.Sprintf("%.2f cores", node.CPUAllocatableCores), MemoryCapacity: clusterBytesLabel(node.MemoryAllocatableBytes), DiskCapacity: "Unavailable", DiskAllocatable: "Unavailable"}
		if node.DiskCapacityAvailable {
			diskCapacity += node.DiskCapacityBytes
			row.DiskCapacity, row.DiskAllocatable = clusterBytesLabel(node.DiskCapacityBytes), clusterBytesLabel(node.DiskAllocatableBytes)
		} else {
			diskCapacityAvailable = false
		}
		if node.DiskUsageAvailable {
			diskUsage += node.DiskUsageBytes
			diskUsageCapacity += node.DiskUsageCapacityBytes
		} else {
			diskUsageAvailable = false
		}
		if node.UsageAvailable {
			cpuUsage += float64(node.CPUUsageCores)
			memoryUsage += node.MemoryUsageBytes
			row.Cpu, row.Memory = fmt.Sprintf("%.2f cores", node.CPUUsageCores), clusterBytesLabel(node.MemoryUsageBytes)
		} else {
			usageAvailable = false
		}
		data.Nodes = append(data.Nodes, row)
	}
	data.Cpu = clusterCapacityData{Title: "CPU usage", Value: fmt.Sprintf("%.2f cores", cpuUsage), Capacity: fmt.Sprintf("%.2f cores", cpuCapacity), Available: usageAvailable && cpuCapacity > 0}
	data.Memory = clusterCapacityData{Title: "Memory usage", Value: clusterBytesLabel(memoryUsage), Capacity: clusterBytesLabel(memoryCapacity), Available: usageAvailable && memoryCapacity > 0}
	data.Disk = clusterCapacityData{Title: "Disk usage", Value: clusterBytesLabel(diskUsage), Capacity: "Unavailable", Available: diskUsageAvailable && diskUsageCapacity > 0}
	if diskCapacityAvailable {
		data.Disk.Capacity = clusterBytesLabel(diskCapacity)
	}
	if data.Disk.Available {
		data.Disk.Capacity = clusterBytesLabel(diskUsageCapacity)
		data.Disk.Percent = float64(diskUsage) / float64(diskUsageCapacity) * 100
	}
	if data.Cpu.Available {
		data.Cpu.Percent = cpuUsage / cpuCapacity * 100
	}
	if data.Memory.Available {
		data.Memory.Percent = float64(memoryUsage) / float64(memoryCapacity) * 100
	}
	return data
}

func clusterBytesLabel(bytes int64) string {
	if bytes >= 1024*1024*1024*1024 {
		return fmt.Sprintf("%.1f TiB", float64(bytes)/(1024*1024*1024*1024))
	}
	if bytes >= 1024*1024*1024 {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1024*1024*1024))
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
}
