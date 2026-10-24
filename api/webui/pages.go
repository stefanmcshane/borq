package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
)

type pageData struct {
	// CustomCharts and CustomChartDraft populate the per-space chart catalog and editor.
	CustomCharts     []appapi.CustomChart
	CustomChart      appapi.CustomChart
	CustomChartDraft customChartEditorData
	// CustomChartTab and CustomChartSubtab select addon settings or shared monitoring.
	CustomChartTab, CustomChartSubtab string
	// Monitoring supplies the same controls for application and addon telemetry.
	Monitoring monitoringViewData

	// AppPath scopes asynchronous health, usage and log panels to the selected application.
	AppPath string
	// Title labels the current page and document.
	Title string
	// View selects an existing management screen.
	View string
	// SectionView identifies the parent screen for breadcrumb links and scope switches.
	SectionView string
	// SectionTitle labels the current section in the breadcrumb.
	SectionTitle string
	// Location carries only canonical navigation state, never submitted credentials or values.
	Location string
	// Organization selects a workspace or an organization-to-cluster link.
	Organization string
	// Organizations supplies the organization breadcrumb menu.
	Organizations []appapi.Organization
	// Clusters contains organization-to-cluster links without credentials.
	Clusters []appapi.Cluster
	// ClusterSummaries counts spaces and applications for each organization-to-cluster link.
	ClusterSummaries []clusterSummary
	// SelectedCluster supplies the selected cluster's spaces and application total.
	SelectedCluster clusterSummary
	// ClusterTab selects metrics, installed components, spaces or applications.
	ClusterTab string
	// ClusterPath retains the saved link when refreshing live cluster metrics.
	ClusterPath string
	// PromptCluster opens the next onboarding step after organization creation.
	PromptCluster bool
	// Spaces lists the organization's independent deployment targets.
	Spaces []appapi.Space
	// Space is the selected deployment binding and revision.
	Space appapi.Space
	// Applications is one accepted API snapshot, including its exact saved manifests.
	Applications appapi.ApplicationState
	// Draft supplies the application editor's current values.
	Draft appapi.App
	// Application is the selected application's accepted configuration.
	Application appapi.App
	// ApplicationTab selects a page within the application's workspace.
	ApplicationTab string
	// ApplicationSubtab selects the configuration editor or observability view.
	ApplicationSubtab string
	// ReviewReturnUrl returns to the section that opened the plan.
	ReviewReturnUrl string
	// ApplicationManifests contains only resources owned by the selected app.
	ApplicationManifests []appapi.Manifest
	// ApplicationEditor carries one unsaved application in either entry mode.
	ApplicationEditor applicationEditorData
	// Groups supplies names and version metadata for the environment list.
	Groups []appapi.EnvironmentGroup
	// Group contains sensitive values only when the user explicitly opens its editor.
	Group appapi.EnvironmentGroup
	// GroupTab selects variables, versions, applications, or settings for one group.
	GroupTab string
	// GroupLatestVersion guards publication even when editing an older version.
	GroupLatestVersion int
	// GroupVersions lists the retained immutable versions available for browsing.
	GroupVersions []int
	// GroupApplications contains accepted apps referencing the selected group.
	GroupApplications []appapi.App
	// EnvironmentEditor contains only the variables displayed in the chosen entry mode.
	EnvironmentEditor environmentEditorData
	// Plan contains the exact immutable artifacts awaiting explicit apply.
	Plan appapi.Plan
	// Operation reports an already accepted deployment without enqueueing more work.
	Operation appapi.Operation
	// Cluster selects the cluster overview.
	Cluster string
	// Notice confirms the completed browser action.
	Notice string
	// ApplicationCount summarizes accepted apps across the organization.
	ApplicationCount int
}

type clusterSummary struct {
	// Name identifies the registered cluster.
	Name string
	// Organization identifies the organization attached to this registration.
	Organization string
	// Spaces contains only spaces bound to this cluster.
	Spaces []appapi.Space
	// ApplicationCount totals the accepted applications in these spaces.
	ApplicationCount int
}

func (web handler) dashboard(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("view") == "management" {
		writer.Header().Set("Cache-Control", "no-store")
		if request.Header.Get("HX-Request") == "true" {
			writer.Header().Set("HX-Redirect", pageUrl("clusters", "", ""))
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, pageUrl("clusters", "", ""), http.StatusSeeOther)
		return
	}
	if version := request.Header.Get("X-Borq-Ui-Version"); request.Header.Get("HX-Request") == "true" && version != "" && version != web.version {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("HX-Redirect", request.URL.RequestURI())
		writer.WriteHeader(http.StatusOK)
		return
	}
	web.show(writer, request, request.URL.Query(), "")
}

func (web handler) show(writer http.ResponseWriter, request *http.Request, selection url.Values, notice string) {
	page, err := web.loadPage(request, selection)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if page.Space.ID != "" {
		http.SetCookie(writer, &http.Cookie{
			Name:     "borq-space-" + page.Organization,
			Value:    page.Space.Name,
			Path:     "/",
			HttpOnly: true,
			Secure:   request.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
	}
	page.Notice = notice
	name := "page"
	if request.Header.Get("HX-Request") == "true" {
		name = "workspace-update"
		writer.Header().Set("HX-Push-Url", page.Location)
	}
	web.render(writer, request, name, page, http.StatusOK)
}

func (web handler) loadPage(request *http.Request, selection url.Values) (pageData, error) {
	view := selection.Get("view")
	// Existing bookmarks resolve to the renamed chart screens and canonical URLs.
	if view == "addons" {
		view = "custom-charts"
	}
	if view == "addon" {
		view = "custom-chart"
	}
	if selection.Get("customChart") == "" && selection.Get("addon") != "" {
		selection.Set("customChart", selection.Get("addon"))
	}
	if view == "" {
		view = "overview"
	}
	if view == "applications" && selection.Get("app") != "" {
		view = "application"
	}
	if view == "environment" && selection.Get("group") != "" {
		view = "environment-group"
	}
	if view == "clusters" && selection.Get("cluster") != "" {
		view = "cluster"
	}
	titles := map[string]string{"custom-charts": "Custom addons", "custom-chart": "Install a custom addon", "overview": "Overview", "applications": "Applications", "application": "Add an application", "spaces": "Spaces", "clusters": "Clusters", "environment": "Environment groups", "environment-group": "Create an environment group"}
	page := pageData{View: view, Title: titles[view], Organization: selection.Get("organization"), Cluster: selection.Get("cluster"),
		EnvironmentEditor: environmentEditorData{Format: "fields"}}
	if view == "cluster" {
		page.Title = "Cluster"
		page.ClusterTab = selection.Get("tab")
	}
	if page.Title == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "This page was not found."}
	}
	page.SectionView = view
	switch view {
	case "application":
		page.SectionView = "applications"
	case "environment-group":
		page.SectionView = "environment"
	case "custom-chart":
		page.SectionView = "custom-charts"
	case "cluster":
		page.SectionView = "clusters"
	}
	page.SectionTitle = titles[page.SectionView]
	var err error
	page.Organizations, err = requestApi[[]appapi.Organization](request.Context(), web.api, http.MethodGet, "/api/v1/organizations", nil)
	if err != nil {
		return page, err
	}
	if view == "clusters" || view == "cluster" {
		return web.loadClustersPage(request, page)
	}
	if page.Organization == "" && len(page.Organizations) > 0 {
		page.Organization = page.Organizations[0].Name
	}
	if page.Organization == "" {
		page.Location = pageUrl(view, "", "")
		return page, nil
	}
	base := organizationPath(page.Organization)
	page.Spaces, err = requestApi[[]appapi.Space](request.Context(), web.api, http.MethodGet, base+"/spaces", nil)
	if err != nil {
		return page, err
	}
	page.Clusters, err = requestApi[[]appapi.Cluster](request.Context(), web.api, http.MethodGet, base+"/clusters", nil)
	if err != nil {
		return page, err
	}
	spaceName := selection.Get("space")
	if view == "spaces" {
		spaceName = ""
	}
	if spaceName == "" && len(page.Spaces) > 0 && view != "overview" && view != "spaces" {
		spaceName = page.Spaces[0].Name
		if cookie, err := request.Cookie("borq-space-" + page.Organization); err == nil {
			for _, space := range page.Spaces {
				if space.Name == cookie.Value {
					spaceName = space.Name
					break
				}
			}
		}
	}
	for _, space := range page.Spaces {
		page.ApplicationCount += len(space.Configuration.Apps)
		if space.Name == spaceName {
			page.Space = space
		}
	}
	if spaceName != "" && page.Space.ID == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "The selected space was not found in this organization."}
	}
	page.PromptCluster = view == "overview" && page.Space.ID == "" && len(page.Clusters) == 0 && selection.Get("setup") == "cluster"
	if selection.Get("app") != "" && page.Space.ID == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "The selected application was not found."}
	}
	if (view == "environment" || view == "environment-group") && page.Space.Cluster != "" {
		page, err = web.loadEnvironmentPage(request, selection, page)
		if err != nil {
			return page, err
		}
	}
	if selection.Get("group") != "" && page.Space.ID == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "The selected environment group was not found."}
	}
	if id := selection.Get("plan"); id != "" {
		page.Plan, err = requestApi[appapi.Plan](request.Context(), web.api, http.MethodGet, "/api/v1/plans/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
		if page.Plan.Space.ID != page.Space.ID {
			return page, apiProblem{status: http.StatusNotFound, message: "The plan belongs to another space."}
		}
	}
	if id := selection.Get("operation"); id != "" {
		page.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
		if page.Operation.SpaceID != page.Space.ID {
			return page, apiProblem{status: http.StatusNotFound, message: "The operation belongs to another space."}
		}
	}
	if (view == "applications" || view == "application") && page.Space.ID != "" {
		page, err = web.loadApplicationPage(request, selection, page)
		if err != nil {
			return page, err
		}
	}

	if (view == "custom-charts" || view == "custom-chart") && page.Space.ID != "" {
		page, err = web.loadCustomChartPage(request, selection, page)
		if err != nil {
			return page, err
		}
	}

	extra := []string{}
	if page.PromptCluster {
		extra = append(extra, "setup", "cluster")
	}
	for _, key := range []string{"customChart", "app", "tab", "subtab", "draft", "plan", "operation", "cluster", "group", "version"} {
		if value := selection.Get(key); value != "" {
			extra = append(extra, key, value)
		}
	}
	page.Location = pageUrl(view, page.Organization, page.Space.Name, extra...)
	return page, nil
}

func organizationPath(organization string) string {
	return "/api/v1/organizations/" + url.PathEscape(organization)
}
func spacePath(organization, space string) string {
	return organizationPath(organization) + "/spaces/" + url.PathEscape(space)
}

func (web handler) operation(writer http.ResponseWriter, request *http.Request) {
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/operations/"+url.PathEscape(request.PathValue("operation")), nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if operationComplete(operation.Status) {
		writer.Header().Set("HX-Trigger", "deployment-complete")
	}
	web.render(writer, request, "ui-operation", operation, http.StatusOK)
}

func (web handler) cancelOperation(writer http.ResponseWriter, request *http.Request) {
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPost, "/api/v1/operations/"+url.PathEscape(request.PathValue("operation"))+"/cancel", nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if operationComplete(operation.Status) {
		writer.Header().Set("HX-Trigger", "deployment-complete")
	}
	web.render(writer, request, "ui-operation", operation, http.StatusOK)
}

type componentData struct {
	// Title names the selected management or target component scope.
	Title string
	// Location retains the same scope while polling.
	Location string
	// Installation contains component progress without credentials.
	Installation appapi.ComponentInstallation
	// Problem reports an unavailable component observation within its own panel.
	Problem string
	// Converged identifies a linked target that also hosts management components.
	Converged bool
}

func (web handler) components(writer http.ResponseWriter, request *http.Request) {
	organization, cluster := request.URL.Query().Get("organization"), request.URL.Query().Get("cluster")
	if organization == "" || cluster == "" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an organization and cluster to inspect components."})
		return
	}
	data := componentData{Title: "On target cluster", Location: "/ui/components?" + url.Values{"organization": {organization}, "cluster": {cluster}}.Encode()}
	path := organizationPath(organization) + "/clusters/" + url.PathEscape(cluster) + "/components"
	var err error
	data.Installation, err = requestApi[appapi.ComponentInstallation](request.Context(), web.api, http.MethodGet, path, nil)
	if problem, ok := err.(apiProblem); ok && problem.status == http.StatusNotFound {
		data.Installation = appapi.ComponentInstallation{Status: "not configured", Message: "No component reconciliation has been recorded."}
		err = nil
	}
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	web.render(writer, request, "ui-components", data, http.StatusOK)
}

func (web handler) kubeconfig(writer http.ResponseWriter, request *http.Request) {
	organization, cluster := request.URL.Query().Get("organization"), request.URL.Query().Get("cluster")
	result, err := requestApi[appapi.ClusterKubeconfig](request.Context(), web.api, http.MethodGet, organizationPath(organization)+"/clusters/"+url.PathEscape(cluster)+"/kubeconfig", nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/yaml")
	writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", strconv.Quote(cluster+".yaml")))
	if request.Method != http.MethodHead {
		_, _ = writer.Write([]byte(result.Kubeconfig))
	}
}
