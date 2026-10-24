package webui

import (
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) loadClustersPage(request *http.Request, page pageData) (pageData, error) {
	for _, organization := range page.Organizations {
		base := organizationPath(organization.Name)
		clusters, err := requestApi[[]appapi.Cluster](request.Context(), web.api, http.MethodGet, base+"/clusters", nil)
		if err != nil {
			return page, err
		}
		if len(clusters) == 0 {
			continue
		}
		spaces, err := requestApi[[]appapi.Space](request.Context(), web.api, http.MethodGet, base+"/spaces", nil)
		if err != nil {
			return page, err
		}
		page.Clusters = append(page.Clusters, clusters...)
		for _, cluster := range clusters {
			summary := clusterSummary{Name: cluster.Name, Organization: organization.Name}
			for _, space := range spaces {
				if space.Cluster == cluster.Name {
					summary.Spaces = append(summary.Spaces, space)
					summary.ApplicationCount += len(space.Configuration.Apps)
				}
			}
			page.ClusterSummaries = append(page.ClusterSummaries, summary)
			if page.Cluster != "" && page.Cluster == cluster.Name && (page.Organization == "" || page.Organization == organization.Name) {
				if page.SelectedCluster.Name != "" {
					return page, apiProblem{status: http.StatusBadRequest, message: "Choose an organization for this cluster link."}
				}
				page.SelectedCluster = summary
			}
		}
	}
	page.Location = pageUrl("clusters", "", "")
	if page.View == "clusters" {
		return page, nil
	}
	if page.SelectedCluster.Name == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "The selected cluster was not found in this organization."}
	}
	page.Organization = page.SelectedCluster.Organization
	page.Title = page.SelectedCluster.Name
	if page.ClusterTab == "" {
		page.ClusterTab = "metrics"
	}
	switch page.ClusterTab {
	case "metrics", "components", "spaces", "applications":
	default:
		return page, apiProblem{status: http.StatusNotFound, message: "This cluster section was not found."}
	}
	page.Location = pageUrl("cluster", page.Organization, "", "cluster", page.Cluster, "tab", page.ClusterTab)
	page.ClusterPath = "/ui/cluster?" + url.Values{"organization": {page.Organization}, "cluster": {page.Cluster}}.Encode()
	return page, nil
}
