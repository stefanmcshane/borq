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
	"golang.org/x/net/html"
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
				{Name: "prod", ID: "prod-id", Cluster: "production", Namespace: "borq-production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "api", Image: "api:v1", Replicas: 2}, {Name: "worker", Image: "worker:v1", Replicas: 1}}}},
				{Name: "preview", ID: "preview-id", Cluster: "preview", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "preview-app"}}}},
			}
		case "/api/v1/organizations/other/spaces":
			output = []appapi.Space{{Name: "other-space", ID: "other-id", Cluster: "production", Configuration: appapi.Configuration{Apps: []appapi.App{{Name: "foreign-app"}}}}}
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
	list := httptest.NewRecorder()
	web.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/?view=clusters", nil))
	assert.Equal(list.Code, http.StatusOK)
	assert.True(strings.Contains(list.Body.String(), `href="/?cluster=production&amp;organization=team&amp;view=cluster"`))
	assert.True(strings.Contains(list.Body.String(), "Select a cluster to inspect"))
	assert.True(!strings.Contains(list.Body.String(), `aria-label="Cluster sections"`))
	assert.True(!strings.Contains(list.Body.String(), "Reading cluster health"))
	assert.True(!strings.Contains(list.Body.String(), `name="context"`))
	assert.True(!strings.Contains(list.Body.String(), `name="reconcile"`))
	for _, input := range []struct{ tab, present, absent string }{
		{"metrics", "Reading cluster health and resource usage", "Loading installed components"},
		{"components", "/ui/cluster-management-components?organization=team&amp;cluster=production", "/ui/management/components"},
		{"spaces", `href="/?organization=team&amp;space=prod&amp;view=overview"`, "borq-preview"},
		{"applications", `href="/?app=api&amp;organization=team&amp;space=prod&amp;view=application"`, "preview-app"},
	} {
		t.Run(input.tab, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			// Old list bookmarks still open the selected cluster, discarding stale space state.
			request := httptest.NewRequest(http.MethodGet, "/?view=clusters&organization=team&cluster=production&space=stale&tab="+input.tab, nil)
			request.Header.Set("HX-Request", "true")
			web.ServeHTTP(response, request)
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("HX-Push-Url"), "/?cluster=production&organization=team&tab="+input.tab+"&view=cluster")
			assert.True(strings.Contains(response.Body.String(), "<h1>production</h1>"))
			assert.True(strings.Contains(response.Body.String(), input.present))
			assert.True(!strings.Contains(response.Body.String(), input.absent))
			assert.True(!strings.Contains(response.Body.String(), "foreign-app"))
			document, err := html.Parse(strings.NewReader(response.Body.String()))
			assert.NoErr(err)
			var tabs []string
			var current []string
			for node := range document.Descendants() {
				for _, attribute := range node.Attr {
					if attribute.Key == "id" && attribute.Val == "workspace-breadcrumb" {
						assert.Equal(navigationText(node), "Admin › Clusters › production")
					}
					if attribute.Key != "aria-label" || attribute.Val != "Cluster sections" {
						continue
					}
					for child := range node.Descendants() {
						if child.Data != "a" {
							continue
						}
						selected, tab := false, ""
						for _, attribute := range child.Attr {
							if attribute.Key == "href" {
								link, err := url.Parse(attribute.Val)
								assert.NoErr(err)
								assert.Equal(link.Query().Get("organization"), "team")
								assert.Equal(link.Query().Get("cluster"), "production")
								tab = link.Query().Get("tab")
							}
							if attribute.Key == "aria-current" && attribute.Val == "page" {
								selected = true
							}
						}
						tabs = append(tabs, tab)
						if selected {
							current = append(current, tab)
						}
					}
				}
			}
			assert.Equal(tabs, []string{"metrics", "components", "spaces", "applications"})
			assert.Equal(current, []string{input.tab})
		})
	}
	for _, action := range []string{"reconcile-cluster", "upgrade-cluster"} {
		response := postForm(web, url.Values{"action": {action}, "view": {"cluster"}, "organization": {"team"}, "cluster": {"production"}, "space": {"stale"}}, true)
		assert.Equal(response.Code, http.StatusOK)
		assert.Equal(response.Header().Get("HX-Push-Url"), "/?cluster=production&organization=team&tab=components&view=cluster")
		assert.True(strings.Contains(response.Body.String(), "Loading installed components"))
	}
	assert.Equal(componentActions, []string{"/api/v1/organizations/team/clusters/production/components/reconcile", "/api/v1/organizations/team/clusters/production/components/upgrade"})
	for _, input := range []struct {
		query  string
		status int
	}{
		{"organization=team&cluster=production&tab=missing", http.StatusNotFound},
		{"organization=team&cluster=missing", http.StatusNotFound},
		{"cluster=production", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=cluster&"+input.query, nil))
		assert.Equal(response.Code, input.status)
	}
}

// TestClusterManagementComponentsPanel keeps management inspection attached to
// the selected cluster, distinguishes readiness from discovery, and explains convergence.
func TestClusterManagementComponentsPanel(t *testing.T) {
	for _, scenario := range []string{"installed", "pending", "failed", "absent", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters/production/management-components")
				assert.Equal(request.Method, http.MethodGet)
				if scenario == "unavailable" {
					writer.WriteHeader(http.StatusInternalServerError)
					_, _ = writer.Write([]byte(`{"message":"Private connection detail"}`))
					return
				}
				result := appapi.PlatformResult{Components: []appapi.ComponentState{}}
				if scenario == "installed" || scenario == "pending" || scenario == "failed" {
					result.Components = []appapi.ComponentState{{Name: "prometheus", Version: "29.36.2", Revision: 3, Status: "deployed"}}
					if scenario != "installed" {
						status := "failed"
						if scenario == "pending" {
							status = "pending-upgrade"
						}
						result.Components = append(result.Components, appapi.ComponentState{Name: "loki", Status: status})
					}
				}
				assert.NoErr(json.NewEncoder(writer).Encode(result))
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/cluster-management-components?organization=team&cluster=production", nil))
			assert.Equal(response.Code, http.StatusOK)
			assert.True(strings.Contains(response.Body.String(), "<h2>On management cluster</h2>"))
			assert.True(strings.Contains(response.Body.String(), `/ui/cluster-management-components?cluster=production&amp;organization=team`))
			assert.True(!strings.Contains(response.Body.String(), "Private connection detail"))
			assert.True(!strings.Contains(response.Body.String(), ">Observed</span>"))
			assert.Equal(strings.Contains(response.Body.String(), "This is a converged cluster:"), scenario == "installed" || scenario == "pending" || scenario == "failed")
			switch scenario {
			case "installed":
				assert.True(strings.Contains(response.Body.String(), "prometheus"))
				assert.True(strings.Contains(response.Body.String(), "29.36.2"))
				assert.True(strings.Contains(response.Body.String(), ">Ready</span>"))
				assert.True(strings.Contains(response.Body.String(), ">Deployed</span>"))
			case "pending", "failed":
				assert.True(strings.Contains(response.Body.String(), ">Not ready</span>"))
				assert.True(!strings.Contains(response.Body.String(), ">Ready</span>"))
			case "absent":
				assert.True(strings.Contains(response.Body.String(), "No management services were found on this cluster."))
			case "unavailable":
				assert.True(strings.Contains(response.Body.String(), "Management components could not be read."))
			}
		})
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
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/cluster?organization=team&cluster=production", nil))
			if scenario == "missing link" {
				assert.Equal(response.Code, http.StatusNotFound)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			assert.True(strings.Contains(response.Body.String(), `hx-get="/ui/cluster?cluster=production&amp;organization=team"`))
			if scenario == "unavailable" {
				assert.True(strings.Contains(response.Body.String(), "Cluster metrics unavailable"))
				assert.True(strings.Contains(response.Body.String(), "Try again"))
				assert.True(!strings.Contains(response.Body.String(), "<meter"))
				return
			}
			for _, label := range []string{"Running", "Pending", "Failed", "Completed", "Unknown", "Container restarts", "Disk pressure", "8.00 cores", "16.0 GiB", "of 20 allocatable pod slots"} {
				assert.True(strings.Contains(response.Body.String(), label))
			}
			assert.True(strings.Contains(response.Body.String(), "Disk usage"))
			assert.True(strings.Contains(response.Body.String(), "Total capacity"))
			assert.True(strings.Contains(response.Body.String(), "Disk usage measures space consumed on each node’s root filesystem."))
			assert.True(!strings.Contains(response.Body.String(), "% allocatable"))
			if scenario == "disk unavailable" || scenario == "partial disk usage" {
				assert.True(strings.Contains(response.Body.String(), "Recent filesystem readings are required from every node."))
				assert.True(!strings.Contains(response.Body.String(), `aria-label="Disk usage as a percentage of total filesystem capacity"`))
				assert.Equal(strings.Count(response.Body.String(), "<meter"), 2)
				if scenario == "partial disk usage" {
					assert.True(strings.Contains(response.Body.String(), "1.5 TiB"))
				}
			} else {
				assert.True(strings.Contains(response.Body.String(), "1000.0 GiB"))
				assert.True(strings.Contains(response.Body.String(), `aria-label="Disk usage as a percentage of total filesystem capacity"`))
				if scenario == "zero disk usage" {
					assert.True(strings.Contains(response.Body.String(), "0.0% used"))
					assert.True(strings.Contains(response.Body.String(), `value="0"`))
				} else {
					assert.True(strings.Contains(response.Body.String(), "100.0 GiB"))
					assert.True(strings.Contains(response.Body.String(), "10.0% used"))
					assert.True(strings.Contains(response.Body.String(), `value="10"`))
				}
			}
			if scenario == "partial disk capacity" {
				assert.True(strings.Contains(response.Body.String(), "Unavailable"))
			}
			if scenario == "partial" {
				assert.True(strings.Contains(response.Body.String(), "Unavailable"))
				assert.True(strings.Contains(response.Body.String(), "1 of 2 nodes"))
				assert.Equal(strings.Count(response.Body.String(), "<meter"), 1)
				return
			}
			if scenario == "complete" {
				assert.Equal(strings.Count(response.Body.String(), "<meter"), 3)
			}
			assert.True(strings.Contains(response.Body.String(), "2.00 cores"))
			assert.True(strings.Contains(response.Body.String(), "4.0 GiB"))
			assert.Equal(strings.Count(response.Body.String(), `value="25"`), 2)
		})
	}
}
