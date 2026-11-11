package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
)

type pageData struct {
	// ManagementAdmin permits installation-wide provider configuration; organization grants never imply this authority.
	ManagementAdmin bool
	// CanManageUsers controls member navigation; the API checks the grant again for every request.
	CanManageUsers bool
	// CanConfigureOrganization permits access to organization authentication metadata.
	CanConfigureOrganization bool
	// AwaitingRole gives authenticated accounts without grants a useful landing page.
	AwaitingRole bool
	// Members contains identities that have signed in through the organization's provider.
	Members []appapi.OrganizationMember
	// Roles contains reusable organization permission sets.
	Roles []appapi.OrganizationRole
	// MemberPermissions supplies organization-scoped choices from the API permission schema.
	MemberPermissions []string
	// Oidc contains safe provider setup metadata for the selected organization.
	Oidc     appapi.OrganizationOidc
	PocketId appapi.OrganizationPocketID
	// Registry reports authentication for the selected space or management scope.
	Registry       registrySettingsData
	Infrastructure infrastructureSelection
	// Postgres supplies managed database configuration, recovery and monitoring.
	Postgres postgresPageData
	// SpacePocketId contains the selected space’s hosted identity services.
	SpacePocketId spacePocketIdPageData
	// SpacePocketIdEnrollment exists only in the immediate explicit enrollment response.
	SpacePocketIdEnrollment string
	// Settings contains safe storage configuration and component pins.
	Settings managementSettingsData
	// CustomCharts and CustomChartDraft populate the per-space chart catalog and editor.
	CustomCharts     []appapi.CustomChart
	CustomChart      appapi.CustomChart
	CustomChartDraft customChartEditorData
	// CustomChartTab and CustomChartSubtab select addon settings or shared monitoring.
	CustomChartTab, CustomChartSubtab string
	// Monitoring supplies the same controls for application and addon telemetry.
	Monitoring monitoringViewData
	// SpaceTab selects the overview, metrics or logs for the whole space.
	SpaceTab string

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
	// ClusterPath retains the saved link when refreshing live cluster metrics.
	ClusterPath string
	// PromptCluster opens the next onboarding step after organization creation.
	PromptCluster bool
	// Spaces lists the organization's independent deployment targets.
	Spaces []spaceSummary
	// Space is the selected deployment binding and revision.
	Space spaceSummary
	// Applications supplies accepted application metadata for navigation and lists.
	Applications applicationListData
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
	Groups []environmentGroupSummary
	// Secret supplies the type and safe data for the selected secret.
	Secret appapi.Secret
	// Group contains sensitive values only when the user explicitly opens its editor.
	Group appapi.EnvironmentGroup
	// GroupTab selects variables, versions, applications, or settings for one group.
	GroupTab string
	// GroupLatestVersion guards publication even when editing an older version.
	GroupLatestVersion int
	// GroupVersions lists the retained immutable versions available for browsing.
	GroupVersions []int
	// GroupApplications contains accepted apps referencing the selected group.
	GroupApplications []applicationSummary
	// EnvironmentEditor contains only the variables displayed in the chosen entry mode.
	EnvironmentEditor environmentEditorData
	// Plan contains the exact immutable artifacts awaiting explicit apply.
	Plan planData
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
	Context         string
	ClientAPIServer *string
	InCluster       bool
	// Name identifies the registered cluster.
	Name string
	// Organization identifies the organization attached to this registration.
	Organization string
	// Spaces contains only spaces bound to this cluster.
	Spaces []spaceSummary
	// ApplicationCount totals the accepted applications in these spaces.
	ApplicationCount int
}

func (web handler) dashboard(writer http.ResponseWriter, request *http.Request) {
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
	web.respond(writer, request, "workspace", page, http.StatusOK)
}

func (web handler) loadPage(request *http.Request, selection url.Values) (pageData, error) {
	trimFormFields(selection)
	view := selection.Get("view")
	if isInfrastructure(view) {
		view = "infrastructure"
	}
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
	if view == "secrets" || view == "space-settings" {
		view = "environment"
	}
	if view == "secret" {
		view = "environment-group"
	}
	if view == "environment" && selection.Get("group") != "" {
		view = "environment-group"
	}
	titles := map[string]string{"infrastructure": "Infrastructure", "postgres": "Managed PostgreSQL", "postgres-detail": "PostgreSQL", "custom-charts": "Custom addons", "custom-chart": "Install a custom addon", "overview": "Overview", "applications": "Applications", "application": "Add an application", "spaces": "All spaces", "environment": "Secrets", "environment-group": "Create a secret"}
	titles["space-pocketid"] = "Pocket ID"
	titles["organization-authentication"] = "Organization authentication"
	titles["organization-members"] = "Organization members"
	page := pageData{View: view, Title: titles[view], Organization: selection.Get("organization"), Cluster: selection.Get("cluster"),
		EnvironmentEditor: environmentEditorData{Format: "fields"}}
	principal, authenticated := apps.PrincipalFromContext(request.Context())
	page.ManagementAdmin = authenticated && principal.Admin
	page.CanManageUsers = page.ManagementAdmin || organizationUiPermission(principal, apps.PermissionUserManage)
	page.CanConfigureOrganization = page.ManagementAdmin || organizationUiPermission(principal, apps.PermissionOrganizationConfigure)
	if page.Title == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "This page was not found."}
	}
	if authenticated && !principal.Admin && principal.Login.Organization != "" && len(principal.Permissions) == 0 {
		page.AwaitingRole = true
		page.Organization = principal.Login.Organization
		page.View, page.Title = "overview", "Awaiting access"
		page.SectionView, page.SectionTitle = page.View, page.Title
		page.Location = pageUrl(page.View, page.Organization, "")
		return page, nil
	}
	if view == "organization-authentication" && !page.CanConfigureOrganization {
		return page, apiProblem{status: http.StatusForbidden, message: "Organization authentication settings require organization:configure."}
	}
	page.SectionView = view
	switch view {
	case "application":
		page.SectionView = "applications"
	case "environment-group":
		page.SectionView = "environment"
	case "custom-chart":
		page.SectionView = "custom-charts"
	case "postgres-detail", "space-pocketid":
		page.SectionView = "postgres"
	}
	page.SectionTitle = titles[page.SectionView]
	if page.SectionView == "postgres" {
		page.SectionTitle = "Managed addons"
	}
	var err error
	page.Organizations, err = requestApi[[]appapi.Organization](request.Context(), web.api, http.MethodGet, "/api/v1/organizations", nil)
	if err != nil {
		return page, err
	}
	if view == "infrastructure" {
		return web.loadInfrastructure(request, infrastructureFromQuery(selection), page)
	}
	if page.Organization == "" && len(page.Organizations) > 0 {
		page.Organization = page.Organizations[0].Name
	}
	if page.Organization == "" {
		page.Location = pageUrl(view, "", "")
		return page, nil
	}
	base := organizationPath(page.Organization)
	if view == "organization-members" {
		page.Members, err = requestApi[[]appapi.OrganizationMember](request.Context(), web.api, http.MethodGet, base+"/members", nil)
		if err == nil {
			page.Roles, err = requestApi[[]appapi.OrganizationRole](request.Context(), web.api, http.MethodGet, base+"/roles", nil)
		}
		page.MemberPermissions = web.organizationMemberPermissions()
		page.Location = pageUrl(view, page.Organization, "")
		return page, err
	}
	spaces, err := requestApi[[]appapi.Space](request.Context(), web.api, http.MethodGet, base+"/spaces", nil)
	if err != nil {
		return page, err
	}
	for _, space := range spaces {
		page.Spaces = append(page.Spaces, summarizeSpace(space))
	}
	page.Clusters, err = requestApi[[]appapi.Cluster](request.Context(), web.api, http.MethodGet, base+"/clusters", nil)
	if err != nil {
		return page, err
	}
	if view == "organization-authentication" {
		page.Oidc, err = requestApi[appapi.OrganizationOidc](request.Context(), web.api, http.MethodGet, base+"/oidc", nil)
		if err == nil {
			page.PocketId, err = requestApi[appapi.OrganizationPocketID](request.Context(), web.api, http.MethodGet, base+"/pocketid", nil)
		}
		page.Location = pageUrl(view, page.Organization, "")
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
	if view == "overview" && page.Space.ID != "" {
		page.SpaceTab = selection.Get("tab")
		if page.SpaceTab == "" {
			page.SpaceTab = "overview"
		}
		if page.SpaceTab != "overview" && page.SpaceTab != "metrics" && page.SpaceTab != "logs" {
			return page, apiProblem{status: http.StatusNotFound, message: "This space section was not found."}
		}
		page.Title = page.Space.Name
		page.Monitoring = monitoringView(page.Organization, page.Space.Name, "", "/api/ui/space")
		page.Monitoring.LogsTitle = "Space logs"
		page.Monitoring.LogsDescription = "Collected pod output across applications and addons in this space. Times shown in UTC."
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
		return page, apiProblem{status: http.StatusNotFound, message: "The selected secret group was not found."}
	}
	if id := selection.Get("plan"); id != "" {
		plan, err := requestApi[appapi.Plan](request.Context(), web.api, http.MethodGet, "/api/v1/plans/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
		page.Plan = summarizePlan(plan)
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

	if (view == "postgres" || view == "postgres-detail") && page.Space.ID != "" {
		page, err = web.loadPostgresPage(request, selection, page)
		if err != nil {
			return page, err
		}
	}
	if view == "space-pocketid" && page.Space.ID != "" {
		page, err = web.loadSpacePocketIdPage(request, selection, page)
		if err != nil {
			return page, err
		}
	}

	extra := []string{}
	if page.PromptCluster {
		extra = append(extra, "setup", "cluster")
	}
	for _, key := range []string{"spacePocketId", "pocketIdOperation", "postgres", "postgresOperation", "customChart", "app", "tab", "subtab", "draft", "plan", "operation", "cluster", "group", "version", "managementOperation"} {
		if value := selection.Get(key); value != "" {
			extra = append(extra, key, value)
		}
	}
	if view == "postgres" || view == "postgres-detail" {
		for _, key := range []string{"range", "search", "end"} {
			if value := selection.Get(key); value != "" {
				extra = append(extra, key, value)
			}
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
	web.respondOperation(writer, request, operation)
}

func (web handler) cancelOperation(writer http.ResponseWriter, request *http.Request) {
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPost, "/api/v1/operations/"+url.PathEscape(request.PathValue("operation"))+"/cancel", nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	web.respondOperation(writer, request, operation)
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
}

func (web handler) components(writer http.ResponseWriter, request *http.Request) {
	organization, cluster := request.URL.Query().Get("organization"), request.URL.Query().Get("cluster")
	if organization == "" || cluster == "" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an organization and cluster to inspect components."})
		return
	}
	data := componentData{Title: "On target cluster", Location: "/api/ui/components?" + url.Values{"organization": {organization}, "cluster": {cluster}}.Encode()}
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
	if data.Installation.InspectionWarning != nil {
		data.Problem = *data.Installation.InspectionWarning
	}
	web.respond(writer, request, "ui-components", data, http.StatusOK)
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
