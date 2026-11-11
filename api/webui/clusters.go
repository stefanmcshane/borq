package webui

import (
	"net/http"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) loadClusterSummaries(request *http.Request, page pageData) (pageData, error) {
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
		for _, cluster := range clusters {
			summary := clusterSummary{Name: cluster.Name, Organization: organization.Name, Context: cluster.Context, ClientAPIServer: cluster.ClientApiServer, InCluster: cluster.InCluster}
			for _, space := range spaces {
				if space.Cluster == cluster.Name {
					summary.Spaces = append(summary.Spaces, summarizeSpace(space))
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
	return page, nil
}
