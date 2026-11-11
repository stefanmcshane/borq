package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// GetSpaceMetrics reads aggregate usage within the selected space's namespace.
func (server handler) GetSpaceMetrics(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, parameters GetSpaceMetricsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	result, err := server.manager.GetSpaceMetrics(request.Context(), input)
	respond[AppMetrics](writer, http.StatusOK, result, err)
}

// GetSpaceLogs reads bounded pod output without accepting arbitrary selectors.
func (server handler) GetSpaceLogs(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, parameters GetSpaceLogsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	if parameters.Search != nil {
		input.Search = *parameters.Search
	}
	if parameters.Limit != nil {
		input.Limit = *parameters.Limit
		if input.Limit < 1 {
			writeProblem(writer, http.StatusBadRequest, "log limit must be between 1 and 1000")
			return
		}
	}
	result, err := server.manager.GetSpaceLogs(request.Context(), input)
	respond[AppLogs](writer, http.StatusOK, result, err)
}
