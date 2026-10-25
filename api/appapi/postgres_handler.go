package appapi

import (
	"github.com/stefanmcshane/borq/business/server/apps"
	"net/http"
)

// ListPostgres lists accepted managed databases without connection credentials.
func (server handler) ListPostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath) {
	result, err := server.manager.ListPostgres(request.Context(), organization, space)
	respond[[]Postgres](writer, http.StatusOK, result, err)
}

// GetPostgres observes database and protection status without connection credentials.
func (server handler) GetPostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	result, err := server.manager.GetPostgres(request.Context(), organization, space, postgres)
	respond[Postgres](writer, http.StatusOK, result, err)
}

// PutPostgres delegates creation or explicit update to the durable manager.
func (server handler) PutPostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath, parameters PutPostgresParams) {
	var body PostgresInput
	if !server.decodeBody(writer, request, "PostgresInput", &body) {
		return
	}
	input, err := convert[apps.PostgresInput](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid PostgreSQL configuration")
		return
	}
	result, err := server.manager.PutPostgres(request.Context(), organization, space, postgres, input, parameters.Apply != nil && *parameters.Apply)
	respond[Postgres](writer, http.StatusAccepted, result, err)
}

// DeletePostgres removes the database while preserving its archive catalogue.
func (server handler) DeletePostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.DeletePostgres(request.Context(), organization, space, postgres)
	respond[Postgres](writer, http.StatusAccepted, result, err)
}

// GetPostgresEnv returns credentials only through this explicit secret response.
func (server handler) GetPostgresEnv(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	result, err := server.manager.GetPostgresEnv(request.Context(), organization, space, postgres)
	respond[PostgresEnv](writer, http.StatusOK, result, err)
}

// GetPostgresOperation reads accepted work without scheduling another operation.
func (server handler) GetPostgresOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	result, err := server.manager.GetPostgresOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// ListPostgresBackups reads the real archive catalogue, including deleted sources.
func (server handler) ListPostgresBackups(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	result, err := server.manager.ListPostgresBackups(request.Context(), organization, space, postgres)
	respond[[]PostgresBackup](writer, http.StatusOK, result, err)
}

// RestorePostgres reserves a new destination and leaves eligibility policy to the manager.
func (server handler) RestorePostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	var body PostgresRestoreInput
	if !server.decodeBody(writer, request, "PostgresRestoreInput", &body) {
		return
	}
	input, err := convert[apps.PostgresRestoreInput](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid restore configuration")
		return
	}
	result, err := server.manager.RestorePostgres(request.Context(), organization, space, postgres, input)
	respond[Postgres](writer, http.StatusAccepted, result, err)
}

// BackupPostgres exposes production backup machinery to automation and acceptance checks.
func (server handler) BackupPostgres(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.BackupPostgres(request.Context(), organization, space, postgres)
	respond[Operation](writer, http.StatusAccepted, result, err)
}

// GetPostgresMetrics reports scoped resource history and current SQL activity.
func (server handler) GetPostgresMetrics(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath, parameters GetPostgresMetricsParams) {
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: postgres}
	if parameters.Range != nil {
		input.Range = string(*parameters.Range)
	}
	if parameters.End != nil {
		input.End = *parameters.End
	}
	result, err := server.manager.GetPostgresMetrics(request.Context(), input)
	respond[PostgresMetrics](writer, http.StatusOK, result, err)
}

// GetPostgresLogs reports bounded database output through the shared telemetry scope.
func (server handler) GetPostgresLogs(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, postgres PostgresPath, parameters GetPostgresLogsParams) {
	input := apps.TelemetryInput{Organization: organization, Space: space, Application: postgres}
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
		if input.Limit < 1 || input.Limit > 1000 {
			writeProblem(writer, http.StatusBadRequest, "log limit must be between 1 and 1000")
			return
		}
	}
	result, err := server.manager.GetPostgresLogs(request.Context(), input)
	respond[AppLogs](writer, http.StatusOK, result, err)
}
