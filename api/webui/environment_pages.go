package webui

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) loadEnvironmentPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	base := spacePath(page.Organization, page.Space.Name) + "/environment-groups"
	if page.View == "environment" {
		var err error
		page.Groups, err = requestApi[[]appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, base, nil)
		return page, err
	}
	page.GroupTab = selection.Get("tab")
	if page.GroupTab == "" {
		page.GroupTab = "variables"
	}
	if page.GroupTab != "variables" && page.GroupTab != "versions" && page.GroupTab != "applications" && page.GroupTab != "settings" {
		return page, apiProblem{status: http.StatusNotFound, message: "This environment group page was not found."}
	}
	name := selection.Get("group")
	if name == "" {
		if page.GroupTab != "variables" || selection.Get("version") != "" {
			return page, apiProblem{status: http.StatusNotFound, message: "Create an environment group before opening its settings."}
		}
		return page, nil
	}
	group, err := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name), nil)
	if err != nil {
		return page, err
	}
	page.GroupLatestVersion = group.Version
	if version := selection.Get("version"); version != "" && version != "latest" && version != strconv.Itoa(group.Version) {
		group, err = requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name)+"/versions/"+url.PathEscape(version), nil)
		if err != nil {
			return page, err
		}
	}
	page.Group, page.Title = group, group.Name
	page.EnvironmentEditor.Values = group.Values
	// The API retains the latest and ten previous versions and validates reads itself.
	for version := page.GroupLatestVersion; version > 0 && len(page.GroupVersions) < 11; version-- {
		page.GroupVersions = append(page.GroupVersions, version)
	}
	for _, application := range page.Space.Configuration.Apps {
		if application.Env.Name == name {
			page.GroupApplications = append(page.GroupApplications, application)
		}
	}
	return page, nil
}
