package webui

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) loadApplicationPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	var err error
	page.Applications, err = requestApi[appapi.ApplicationState](request.Context(), web.api, http.MethodGet, spacePath(page.Organization, page.Space.Name)+"/apps", nil)
	if err != nil || page.View != "application" {
		return page, err
	}
	page.ApplicationTab = selection.Get("tab")
	if page.ApplicationTab == "" {
		page.ApplicationTab = "general"
	}
	if page.ApplicationTab != "general" && page.ApplicationTab != "configuration" && page.ApplicationTab != "manifests" {
		return page, apiProblem{status: http.StatusNotFound, message: "This application page was not found."}
	}
	name := selection.Get("app")
	for _, application := range page.Applications.Configuration.Apps {
		if application.Name == name {
			page.Application, page.Draft = application, application
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
	if page.Application.Name == "" && page.ApplicationTab != "general" {
		return page, apiProblem{status: http.StatusNotFound, message: "This application page was not found."}
	}
	for _, manifest := range page.Applications.Manifests {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if page.Application.Name != "" && labels["app.kubernetes.io/name"] == page.Application.Name {
			page.ApplicationManifests = append(page.ApplicationManifests, manifest)
		}
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
	if err != nil || page.ApplicationTab != "general" {
		return page, err
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
