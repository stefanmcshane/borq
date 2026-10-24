package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// GetApp returns current health independently from accepted deployment progress.
func (server handler) GetApp(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.GetApp(request.Context(), organization, space, application)
	respond[ApplicationDetails](writer, http.StatusOK, result, err)
}

// ListAppDeployments returns application-specific accepted release history.
func (server handler) ListAppDeployments(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.ListAppDeployments(request.Context(), organization, space, application)
	respond[[]Deployment](writer, http.StatusOK, result, err)
}

// GetAppMetrics reads bounded aggregate resource usage from management Prometheus.
func (server handler) GetAppMetrics(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath, parameters GetAppMetricsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: application}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	result, err := server.manager.GetAppMetrics(request.Context(), input)
	respond[AppMetrics](writer, http.StatusOK, result, err)
}

// GetAppLogs reads label-scoped persisted output without accepting arbitrary LogQL.
func (server handler) GetAppLogs(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath, parameters GetAppLogsParams) {
	writer.Header().Set("Cache-Control", "no-store")
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: application}
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
	result, err := server.manager.GetAppLogs(request.Context(), input)
	respond[AppLogs](writer, http.StatusOK, result, err)
}

// RestartApp immediately starts a Kubernetes rollout restart without creating a deployment plan.
func (server handler) RestartApp(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.RestartApp(request.Context(), organization, space, application)
	respond[ResourceStatus](writer, http.StatusAccepted, result, err)
}

// PlanAppRollback saves a historical application configuration as a fresh plan.
func (server handler) PlanAppRollback(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, application ApplicationPath) {
	var body RollbackInput
	if !server.decodeBody(writer, request, "RollbackInput", &body) {
		return
	}
	result, err := server.manager.PlanAppRollback(request.Context(), organization, space, application, body.Deployment)
	respond[Plan](writer, http.StatusCreated, result, err)
}
