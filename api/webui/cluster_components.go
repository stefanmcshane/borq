package webui

import (
	"net/http"
	"net/url"
	"unicode"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) clusterManagementComponents(writer http.ResponseWriter, request *http.Request) {
	organization, cluster := request.URL.Query().Get("organization"), request.URL.Query().Get("cluster")
	if organization == "" || cluster == "" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an organization and cluster link."})
		return
	}
	data := componentData{Title: "On management cluster", Location: "/ui/cluster-management-components?" + url.Values{"organization": {organization}, "cluster": {cluster}}.Encode()}
	result, err := requestApi[appapi.PlatformResult](request.Context(), web.api, http.MethodGet, organizationPath(organization)+"/clusters/"+url.PathEscape(cluster)+"/management-components", nil)
	if problem, ok := err.(apiProblem); ok && (problem.status == http.StatusBadRequest || problem.status == http.StatusNotFound) {
		web.problem(writer, request, err)
		return
	}
	data.Installation = appapi.ComponentInstallation{Status: "ready", Message: "Services that run Borq and support deployments.", Result: result}
	for _, component := range result.Components {
		if component.Status != "deployed" {
			data.Installation.Status = "not ready"
			break
		}
	}
	if len(result.Components) == 0 {
		data.Installation.Status, data.Installation.Message = "not installed", "No management services were found on this cluster."
	}
	if err != nil {
		data.Installation.Status, data.Installation.Message = "unavailable", "Management components could not be read."
		data.Problem = "Check cluster connectivity and permission to inspect Helm releases."
	}
	data.Converged = err == nil && len(result.Components) > 0
	web.render(writer, request, "ui-components", data, http.StatusOK)
}

func componentLabel(value string) string {
	characters := []rune(value)
	if len(characters) > 0 {
		characters[0] = unicode.ToUpper(characters[0])
	}
	return string(characters)
}
