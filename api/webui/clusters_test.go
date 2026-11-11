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

// TestClusterWorkspaceNavigation owns list-to-detail navigation, tab scope and
// target component actions without treating API acceptance as controller success.
func TestClusterWorkspaceNavigation(t *testing.T) {
	assert := is.New(t)
	var componentActions []string
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}, {Name: "other"}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{{Name: "production"}, {Name: "preview"}}
		case "/api/v1/organizations/other/clusters":
			output = []appapi.Cluster{{Name: "production"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{
				{Name: "prod", ID: "prod-id", Cluster: "production", Namespace: "borq-production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api", Image: "api:v1", Replicas: 2, Command: "cluster-app-private"}, {Name: "worker", Image: "worker:v1", Replicas: 1}}}},
				{Name: "preview", ID: "preview-id", Cluster: "preview", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "preview-app"}}}},
			}
		case "/api/v1/organizations/other/spaces":
			output = []appapi.Space{{Name: "other-space", ID: "other-id", Cluster: "production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "foreign-app"}}}}}
		case "/api/v1/management/component-catalog":
			output = appapi.ComponentCatalog{}
		case "/api/v1/organizations/team/clusters/production/components/reconcile", "/api/v1/organizations/team/clusters/production/components/upgrade":
			assert.Equal(request.Method, http.MethodPost)
			componentActions = append(componentActions, request.URL.Path)
			output = appapi.ComponentInstallation{Status: "queued"}
		default:
			t.Errorf("unexpected API request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)

	list := requestPage(t, web, "view=clusters", http.StatusOK)
	assert.Equal(list.Infrastructure.Tab, "clusters")
	assert.Equal(len(list.ClusterSummaries), 3)
	for _, tab := range []string{"metrics", "components", "spaces", "applications"} {
		t.Run(tab, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=clusters&organization=team&cluster=production&space=stale&tab="+tab, nil))
			page := decodeView[pageData](t, response, "workspace")
			assert.True(!strings.Contains(response.Body.String(), "cluster-app-private"))
			assert.Equal(page.Infrastructure.Node, applicationNode)
			assert.Equal(page.Title, "production")
			assert.Equal(page.Infrastructure.Tab, tab)
			assert.Equal(page.Space.ID, "")
			assert.Equal(page.SelectedCluster.Organization, "team")
			assert.Equal(page.SelectedCluster.ApplicationCount, 2)
			assert.Equal(len(page.SelectedCluster.Spaces), 1)
			assert.Equal(page.SelectedCluster.Spaces[0].Name, "prod")
		})
	}
	for _, action := range []string{"reconcile-cluster", "upgrade-cluster"} {
		page := decodeView[pageData](t, postForm(web, url.Values{"action": {action}, "view": {"cluster"}, "organization": {"team"}, "cluster": {"production"}, "space": {"stale"}}), "workspace")
		assert.Equal(page.Infrastructure.Tab, "components")
	}
	assert.Equal(componentActions, []string{"/api/v1/organizations/team/clusters/production/components/reconcile", "/api/v1/organizations/team/clusters/production/components/upgrade"})
	for _, input := range []struct {
		query  string
		status int
	}{{"organization=team&cluster=production&tab=missing", 404}, {"organization=team&cluster=missing", 404}, {"cluster=production", 400}} {
		requestPage(t, web, "view=cluster&"+input.query, input.status)
	}

}

// TestClusterMetricsPanel owns readable units, complete-versus-partial totals
// and recoverable monitoring failures while retaining the exact saved link.
func TestClusterMetricsPanel(t *testing.T) {
	for _, scenario := range []string{"complete", "partial", "partial disk capacity", "partial disk usage", "disk unavailable", "zero disk usage", "unavailable", "missing link"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters/production/metrics")
				if scenario == "unavailable" || scenario == "missing link" {
					status := http.StatusInternalServerError
					if scenario == "missing link" {
						status = http.StatusNotFound
					}
					writer.WriteHeader(status)
					_, _ = writer.Write([]byte(`{"message":"Cluster observations unavailable"}`))
					return
				}
				snapshot := appapi.ClusterMetrics{ObservedAt: time.Now().UTC(), Nodes: []appapi.ClusterNodeMetrics{
					{Name: "node-a", Ready: true, CPUAllocatableCores: 4, MemoryAllocatableBytes: 8 * 1024 * 1024 * 1024, DiskCapacityAvailable: true, DiskCapacityBytes: 1024 * 1024 * 1024 * 1024, DiskAllocatableBytes: 900 * 1024 * 1024 * 1024, DiskUsageAvailable: scenario != "disk unavailable", DiskUsageBytes: 100 * 1024 * 1024 * 1024, DiskUsageCapacityBytes: 800 * 1024 * 1024 * 1024, PodCapacity: 10, UsageAvailable: true, CPUUsageCores: 1, MemoryUsageBytes: 1024 * 1024 * 1024},
					{Name: "node-b", CPUAllocatableCores: 4, MemoryAllocatableBytes: 8 * 1024 * 1024 * 1024, DiskCapacityAvailable: scenario != "partial disk capacity" && scenario != "disk unavailable", DiskCapacityBytes: 512 * 1024 * 1024 * 1024, DiskAllocatableBytes: 400 * 1024 * 1024 * 1024, DiskUsageAvailable: scenario != "partial disk usage" && scenario != "disk unavailable", DiskUsageCapacityBytes: 200 * 1024 * 1024 * 1024, PodCapacity: 10, UsageAvailable: scenario != "partial", CPUUsageCores: 1, MemoryUsageBytes: 3 * 1024 * 1024 * 1024, Conditions: []string{"DiskPressure"}},
				}, Pods: appapi.ClusterPodMetrics{Running: 5, Pending: 2, Unknown: 1, Failed: 1, Succeeded: 3, Restarts: 8}}
				if scenario == "zero disk usage" {
					snapshot.Nodes[0].DiskUsageBytes = 0
				}
				if scenario == "partial" {
					snapshot.UsageNotice = "Recent measurements are available for 1 of 2 nodes."
				}
				assert.NoErr(json.NewEncoder(writer).Encode(snapshot))
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/cluster?organization=team&cluster=production", nil))
			if scenario == "missing link" {
				assert.Equal(response.Code, http.StatusNotFound)
				return
			}

			data := decodeView[clusterMetricsData](t, response, "cluster-metrics")
			assert.Equal(data.Location, "/api/ui/cluster?cluster=production&organization=team")
			if scenario == "unavailable" {
				assert.True(data.Problem != "")
				assert.True(!data.Cpu.Available)
				return
			}
			assert.Equal(data.Cpu.Capacity, "8.00 cores")
			assert.Equal(data.Memory.Capacity, "16.0 GiB")
			assert.Equal(data.ActivePods, 8)
			assert.Equal(data.PodCapacity, int64(20))
			assert.Equal(data.ReadyNodes, 1)
			assert.Equal(data.PressureNodes, 1)
			assert.Equal(data.Nodes[1].Pressure, "Disk pressure")
			assert.Equal(data.Cpu.Available, scenario != "partial")
			assert.Equal(data.Memory.Available, scenario != "partial")
			assert.Equal(data.Disk.Available, scenario != "disk unavailable" && scenario != "partial disk usage")
			if data.Disk.Available {
				if scenario == "zero disk usage" {
					assert.Equal(data.Disk.Percent, float64(0))
					assert.Equal(data.Disk.Value, "0.0 MiB")
				} else {
					assert.Equal(data.Disk.Percent, float64(10))
					assert.Equal(data.Disk.Value, "100.0 GiB")
				}
				assert.Equal(data.Disk.Capacity, "1000.0 GiB")
			}
			if scenario == "partial" {
				assert.Equal(data.Snapshot.UsageNotice, "Recent measurements are available for 1 of 2 nodes.")
				return
			}
			assert.Equal(data.Cpu.Value, "2.00 cores")
			assert.Equal(data.Memory.Value, "4.0 GiB")
			assert.Equal(data.Cpu.Percent, float64(25))
			assert.Equal(data.Memory.Percent, float64(25))

		})
	}
}
