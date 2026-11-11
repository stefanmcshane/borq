package webui

import (
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
)

type infrastructureNode string

const (
	managementNode  infrastructureNode = "management"
	applicationNode infrastructureNode = "application"
)

type infrastructureSelection struct {
	Node                                                             infrastructureNode
	Organization, Cluster, Tab, ComponentScope, Component, Operation string
}

func infrastructureFromQuery(query url.Values) infrastructureSelection {
	selection := infrastructureSelection{Node: infrastructureNode(query.Get("node")), Organization: query.Get("organization"), Cluster: query.Get("cluster"), Tab: query.Get("tab"), ComponentScope: query.Get("componentScope"), Component: query.Get("component"), Operation: query.Get("managementOperation")}
	switch query.Get("view") {
	case "management":
		selection.Node, selection.Tab = managementNode, "clusters"
	case "clusters":
		if selection.Cluster == "" {
			selection.Node, selection.Tab = managementNode, "clusters"
		} else {
			selection.Node = applicationNode
		}
	case "cluster":
		selection.Node = applicationNode
	case "settings":
		selection.Node, selection.Tab = managementNode, "settings"
		if selection.ComponentScope == "management" || (selection.ComponentScope == "" && selection.Component != "") {
			selection.ComponentScope = "management"
			selection.Tab = "components"
		}
		if selection.ComponentScope == "target" || (selection.ComponentScope == "postgres" && (query.Get("componentOrganization") != "" || query.Get("componentCluster") != "")) {
			selection.Node, selection.Tab = applicationNode, "components"
			selection.Organization, selection.Cluster = query.Get("componentOrganization"), query.Get("componentCluster")
			if selection.Organization == "" && selection.Cluster == "" {
				selection.Node, selection.Tab, selection.ComponentScope, selection.Component = managementNode, "clusters", "management", ""
			}
		}
	}
	if selection.Node == "" {
		selection.Node = managementNode
	}
	if selection.Tab == "" {
		selection.Tab = "components"
	}
	if selection.Node == managementNode {
		selection.Organization, selection.Cluster = "", ""
	}
	if selection.ComponentScope == "" {
		selection.ComponentScope = "management"
		if selection.Node == applicationNode {
			selection.ComponentScope = "target"
		}
		if selection.Node == managementNode && selection.Tab == "settings" {
			selection.ComponentScope = "postgres"
		}
	}
	return selection
}

func isInfrastructure(view string) bool {
	switch view {
	case "infrastructure", "management", "settings", "clusters", "cluster":
		return true
	}
	return false
}

func (selection infrastructureSelection) Values() url.Values {
	values := url.Values{"view": {"infrastructure"}, "node": {string(selection.Node)}, "tab": {selection.Tab}}
	for key, value := range map[string]string{"organization": selection.Organization, "cluster": selection.Cluster, "componentScope": selection.ComponentScope, "component": selection.Component, "managementOperation": selection.Operation} {
		if value != "" {
			values.Set(key, value)
		}
	}
	return values
}
func (selection infrastructureSelection) URL() string { return "/?" + selection.Values().Encode() }
func (selection infrastructureSelection) TabURL(tab string) string {
	selection.Tab = tab
	if selection.Node == managementNode {
		source := "management"
		if tab == "settings" {
			source = "postgres"
		}
		if source != selection.ComponentScope {
			selection.ComponentScope, selection.Component = source, ""
		}
	}
	return selection.URL()
}
func managementInfrastructureURL() string {
	return infrastructureSelection{Node: managementNode, Tab: "components", ComponentScope: "management"}.URL()
}
func (cluster clusterSummary) URL() string {
	return infrastructureSelection{Node: applicationNode, Organization: cluster.Organization, Cluster: cluster.Name, Tab: "components", ComponentScope: "target"}.URL()
}

func (selection infrastructureSelection) validate() error {
	valid := false
	switch selection.Node {
	case managementNode:
		valid = ((selection.Tab == "components" || selection.Tab == "clusters") && selection.ComponentScope == "management") || (selection.Tab == "settings" && selection.ComponentScope == "postgres")
	case applicationNode:
		if selection.Cluster == "" {
			return apiProblem{status: http.StatusBadRequest, message: "Choose a registered application cluster."}
		}
		valid = selection.Tab == "components" && (selection.ComponentScope == "target" || selection.ComponentScope == "postgres")
		if selection.Tab == "metrics" || selection.Tab == "spaces" || selection.Tab == "applications" || selection.Tab == "settings" {
			valid = selection.ComponentScope == "target" || selection.ComponentScope == "postgres"
		}
	}
	if !valid {
		return apiProblem{status: http.StatusNotFound, message: "This component source or section is not available for the selected cluster."}
	}
	return nil
}

func (web handler) loadInfrastructure(request *http.Request, selection infrastructureSelection, page pageData) (pageData, error) {
	if err := selection.validate(); err != nil {
		return page, err
	}
	page.Infrastructure = selection
	page.View, page.SectionView, page.SectionTitle = "infrastructure", "infrastructure", "Infrastructure"
	page.Organization, page.Cluster = selection.Organization, selection.Cluster
	var err error
	page, err = web.loadClusterSummaries(request, page)
	if err != nil {
		return page, err
	}
	if selection.Node == applicationNode && page.Cluster != "" {
		if page.SelectedCluster.Name == "" {
			return page, apiProblem{status: http.StatusNotFound, message: "The selected cluster was not found in this organization."}
		}
		selection.Organization = page.SelectedCluster.Organization
		selection.Cluster = page.SelectedCluster.Name
		page.Organization, page.Title = selection.Organization, selection.Cluster
	} else if selection.Node == managementNode {
		page.Title = "Management cluster"
	} else if selection.Node != applicationNode {
		return page, apiProblem{status: http.StatusNotFound, message: "This infrastructure node was not found."}
	}
	page.Infrastructure = selection
	page.ClusterPath = "/api/ui/cluster?" + url.Values{"organization": {page.Organization}, "cluster": {page.Cluster}}.Encode()
	if selection.Operation != "" {
		page.Settings.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/management/operations/"+url.PathEscape(selection.Operation), nil)
		if err != nil {
			return page, err
		}
	}
	if selection.Tab == "components" || (selection.Node == managementNode && selection.Tab == "settings") {
		page, err = web.loadManagementSettings(request, page)
		if err != nil {
			return page, err
		}
		selection.Component = page.Settings.Selected.Name
	}
	page.Infrastructure, page.Location = selection, selection.URL()
	return page, nil
}

func (web handler) managementOperation(writer http.ResponseWriter, request *http.Request) {
	selection := infrastructureFromQuery(request.URL.Query())
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/management/operations/"+url.PathEscape(selection.Operation), nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	web.respond(writer, request, "management-operation", operation, http.StatusOK)
}
