package webui

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

func (web handler) loadApplicationPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	var err error
	page.Applications, err = requestApi[appapi.ApplicationState](request.Context(), web.api, http.MethodGet, spacePath(page.Organization, page.Space.Name)+"/apps", nil)
	if err != nil || page.View != "application" {
		return page, err
	}
	page.ApplicationTab = selection.Get("tab")
	page.ApplicationSubtab = selection.Get("subtab")
	// Keep existing bookmarks usable after consolidating the application sections.
	switch page.ApplicationTab {
	case "general":
		page.ApplicationTab = "configuration"
	case "logs":
		page.ApplicationTab, page.ApplicationSubtab = "observability", "logs"
	case "deployments":
		page.ApplicationTab = "overview"
	case "manifests":
		page.ApplicationTab, page.ApplicationSubtab = "configuration", "manifests"
	}
	if page.ApplicationTab == "" {
		page.ApplicationTab = "configuration"
		if selection.Get("app") != "" && page.Plan.ID == "" {
			page.ApplicationTab = "overview"
		}
	}
	if page.ApplicationTab != "overview" && page.ApplicationTab != "observability" && page.ApplicationTab != "configuration" {
		return page, apiProblem{status: http.StatusNotFound, message: "This application page was not found."}
	}
	name := selection.Get("app")
	for _, application := range page.Applications.Configuration.Apps {
		if application.Name == name {
			page.Application, page.Draft = application, application
		}
	}
	if id := selection.Get("draft"); id != "" && page.Plan.ID == "" && page.ApplicationTab == "configuration" {
		draft, err := requestApi[appapi.Plan](request.Context(), web.api, http.MethodGet, "/api/v1/plans/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
		if draft.Space.ID != page.Space.ID {
			return page, apiProblem{status: http.StatusNotFound, message: "This draft belongs to another space."}
		}
		found := false
		for _, application := range draft.Configuration.Apps {
			if application.Name == name {
				page.Draft = application
				found = true
			}
		}
		if !found {
			return page, apiProblem{status: http.StatusNotFound, message: "The selected application was not found in this plan."}
		}
	}
	// A reviewed new app is not accepted yet; its draft lives in the saved plan.
	for _, application := range page.Plan.Configuration.Apps {
		if application.Name == name {
			page.Draft = application
		}
	}
	if name != "" && page.Draft.Name != name {
		return page, apiProblem{status: http.StatusNotFound, message: "The selected application was not found."}
	}
	if name != "" {
		page.Title = name
	}
	if page.Application.Name == "" && page.ApplicationTab != "configuration" {
		return page, apiProblem{status: http.StatusNotFound, message: "This application page was not found."}
	}
	for _, manifest := range page.Applications.Manifests {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if page.Application.Name != "" && labels["app.kubernetes.io/name"] == page.Application.Name {
			page.ApplicationManifests = append(page.ApplicationManifests, manifest)
		}
	}
	page.AppPath = "/ui/application?" + url.Values{"organization": {page.Organization}, "space": {page.Space.Name}, "app": {page.Application.Name}}.Encode()
	page.Monitoring = monitoringView(page.Organization, page.Space.Name, page.Application.Name, "/ui/application")
	if page.ApplicationTab == "observability" && page.ApplicationSubtab == "" {
		page.ApplicationSubtab = "metrics"
	}
	if page.ApplicationTab == "configuration" && page.ApplicationSubtab == "" {
		page.ApplicationSubtab = "fields"
	}
	if (page.ApplicationTab == "observability" && page.ApplicationSubtab != "logs" && page.ApplicationSubtab != "metrics") || (page.ApplicationTab == "configuration" && page.ApplicationSubtab != "fields" && page.ApplicationSubtab != "yaml" && page.ApplicationSubtab != "manifests") || (page.ApplicationSubtab == "manifests" && page.Application.Name == "") {
		return page, apiProblem{status: http.StatusNotFound, message: "This application view was not found."}
	}
	page.ReviewReturnUrl = pageUrl("application", page.Organization, page.Space.Name, "app", page.Application.Name, "tab", page.ApplicationTab, "subtab", page.ApplicationSubtab)
	if page.Plan.ID != "" && page.ApplicationTab == "configuration" {
		page.ReviewReturnUrl = pageUrl("application", page.Organization, page.Space.Name, "app", page.Draft.Name, "tab", page.ApplicationTab, "subtab", page.ApplicationSubtab, "draft", page.Plan.ID)
	}
	// The editor keeps its draft through YAML switches without changing accepted state.
	document := json.RawMessage(`{}`)
	if page.Draft.Name != "" {
		document, err = json.Marshal(page.Draft)
		if err != nil {
			return page, err
		}
	}
	var savedIngresses []string
	for _, route := range page.Application.Ingresses {
		savedIngresses = append(savedIngresses, route.Name)
	}
	page.ApplicationEditor, err = applicationEditorView(document, page.Application.Name, savedIngresses)
	if err != nil || page.ApplicationTab != "configuration" {
		return page, err
	}
	page.ApplicationEditor.Organization, page.ApplicationEditor.Space = page.Organization, page.Space.Name
	if page.ApplicationSubtab == "manifests" {
		page.ApplicationEditor.Format = "manifests"
		return page, nil
	}
	if page.ApplicationSubtab == "yaml" {
		page.ApplicationEditor.Format = "yaml"
		data, err := yaml.JSONToYAML(document)
		if err != nil {
			return page, err
		}
		page.ApplicationEditor.Yaml = string(data)
	}
	groups, err := requestApi[[]appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, spacePath(page.Organization, page.Space.Name)+"/environment-groups", nil)
	if err != nil {
		return page, err
	}
	for _, group := range groups {
		page.ApplicationEditor.GroupNames = append(page.ApplicationEditor.GroupNames, group.Name)
	}
	page.ApplicationEditor.Namespace = page.Space.Namespace
	page.ApplicationEditor.EnvironmentUrl = pageUrl("environment", page.Organization, page.Space.Name)
	return page, nil
}
