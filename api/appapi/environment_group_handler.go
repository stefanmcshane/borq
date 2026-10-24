package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ListEnvironmentGroups lists published group families independently of application deployment.
func (server handler) ListEnvironmentGroups(writer http.ResponseWriter, request *http.Request, organization, space string) {
	result, err := server.manager.ListEnvironmentGroups(request.Context(), apps.EnvironmentGroupTarget{
		Organization: organization, Space: space,
	})
	respond[[]EnvironmentGroup](writer, http.StatusOK, result, err)
}

// GetEnvironmentGroup reads the latest published version with its complete values.
func (server handler) GetEnvironmentGroup(writer http.ResponseWriter, request *http.Request, organization, space, group string) {
	server.GetEnvironmentGroupVersion(writer, request, organization, space, group, "latest")
}

// GetEnvironmentGroupVersion reads one existing immutable version without changing applications.
func (server handler) GetEnvironmentGroupVersion(writer http.ResponseWriter, request *http.Request, organization, space, group, version string) {
	result, err := server.manager.GetEnvironmentGroup(request.Context(), apps.EnvironmentGroupTarget{
		Organization: organization, Space: space,
	}, group, version)
	respond[EnvironmentGroup](writer, http.StatusOK, result, err)
}

// PublishEnvironmentGroup publishes a complete value map before an application plan selects it.
func (server handler) PublishEnvironmentGroup(writer http.ResponseWriter, request *http.Request, organization, space, group string) {
	var body PublishEnvironmentGroupInput
	if !server.decodeBody(writer, request, "PublishEnvironmentGroupInput", &body) {
		return
	}
	values, err := convert[[]apps.Variable](body.Values)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode environment group")
		return
	}
	result, err := server.manager.PublishEnvironmentGroup(request.Context(), apps.PublishEnvironmentGroupInput{
		Target: apps.EnvironmentGroupTarget{Organization: organization, Space: space},
		Name:   group, Values: values, BaseVersion: body.BaseVersion,
	})
	respond[EnvironmentGroup](writer, http.StatusCreated, result, err)
}

// CloneEnvironmentGroup publishes a distinct group family from an existing version.
func (server handler) CloneEnvironmentGroup(writer http.ResponseWriter, request *http.Request, organization, space, group string) {
	var body CloneEnvironmentGroupInput
	if !server.decodeBody(writer, request, "CloneEnvironmentGroupInput", &body) {
		return
	}
	version := "latest"
	if body.SourceVersion != nil {
		version = *body.SourceVersion
	}
	result, err := server.manager.CloneEnvironmentGroup(request.Context(), apps.CloneEnvironmentGroupInput{
		Target: apps.EnvironmentGroupTarget{Organization: organization, Space: space},
		Name:   group, SourceName: body.SourceName, SourceVersion: version,
	})
	respond[EnvironmentGroup](writer, http.StatusCreated, result, err)
}
