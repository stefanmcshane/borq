package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/helm"
)

// GetChartDefaults downloads version-specific upstream values without installing resources.
func (server handler) GetChartDefaults(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	var body ChartInput
	if !server.decodeBody(writer, request, "ChartInput", &body) {
		return
	}
	result, err := server.manager.GetChartDefaults(request.Context(), helm.ChartInput{Repository: body.Repository, Name: body.Name, Version: body.Version})
	respond[ChartDefaults](writer, http.StatusOK, result, err)
}

// ListCustomCharts returns only custom charts accepted in the requested space.
func (server handler) ListCustomCharts(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.ListAddons(request.Context(), organization, space)
	respond[[]CustomChart](writer, http.StatusOK, result, err)
}

// GetCustomChart reads accepted desired state and the latest installation outcome.
func (server handler) GetCustomChart(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.GetAddon(request.Context(), organization, space, customChart)
	respond[CustomChart](writer, http.StatusOK, result, err)
}

// PutCustomChart rejects duplicate installs unless apply explicitly permits an update.
func (server handler) PutCustomChart(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath, parameters PutCustomChartParams) {
	writer.Header().Set("Cache-Control", "no-store")
	var body CustomChartInput
	if !server.decodeBody(writer, request, "CustomChartInput", &body) {
		return
	}
	input, err := convert[apps.AddonInput](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid custom chart configuration")
		return
	}
	apply := parameters.Apply != nil && *parameters.Apply
	result, err := server.manager.PutAddon(request.Context(), organization, space, customChart, input, apply)
	respond[CustomChart](writer, http.StatusAccepted, result, err)
}

// DeleteCustomChart schedules uninstall of only this space-owned Helm release.
func (server handler) DeleteCustomChart(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath) {
	writer.Header().Set("Cache-Control", "no-store")
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.DeleteAddon(request.Context(), organization, space, customChart)
	respond[CustomChart](writer, http.StatusAccepted, result, err)
}

// GetCustomChartOperation reads progress without creating another operation.
func (server handler) GetCustomChartOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.GetAddonOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// GetCustomChartMetrics returns bounded CPU and memory across selected custom chart pods.
func (server handler) GetCustomChartMetrics(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath, parameters GetCustomChartMetricsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: customChart}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	result, err := server.manager.GetAddonMetrics(request.Context(), input)
	respond[AppMetrics](writer, http.StatusOK, result, err)
}

// GetCustomChartEvents returns installation history even when pod log collection is disabled.
func (server handler) GetCustomChartEvents(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath, parameters GetCustomChartEventsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: customChart}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	result, err := server.manager.GetAddonEvents(request.Context(), input)
	respond[EventHistory](writer, http.StatusOK, result, err)
}

// GetCustomChartLogs returns bounded, label-scoped persisted custom chart output.
func (server handler) GetCustomChartLogs(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, customChart CustomChartPath, parameters GetCustomChartLogsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: customChart}
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
	result, err := server.manager.GetAddonLogs(request.Context(), input)
	respond[AppLogs](writer, http.StatusOK, result, err)
}
