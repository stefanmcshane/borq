package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ListOrganizationRoles returns the reusable roles available for member assignment.
func (server handler) ListOrganizationRoles(writer http.ResponseWriter, request *http.Request, organization string) {
	roles, err := server.manager.ListOrganizationRoles(request.Context(), organization)
	respond[[]OrganizationRole](writer, http.StatusOK, roles, err)
}

// CreateOrganizationRole creates a named set of organization permissions.
func (server handler) CreateOrganizationRole(writer http.ResponseWriter, request *http.Request, organization string) {
	var input apps.OrganizationRoleInput
	if !server.decodeBody(writer, request, "OrganizationRoleInput", &input) {
		return
	}
	role, err := server.manager.CreateOrganizationRole(request.Context(), organization, input)
	respond[OrganizationRole](writer, http.StatusCreated, role, err)
}

// PutOrganizationRole updates permissions only at the caller's observed revision.
func (server handler) PutOrganizationRole(writer http.ResponseWriter, request *http.Request, organization, role string) {
	var input apps.OrganizationRoleInput
	if !server.decodeBody(writer, request, "OrganizationRoleInput", &input) {
		return
	}
	updated, err := server.manager.PutOrganizationRole(request.Context(), organization, role, input)
	respond[OrganizationRole](writer, http.StatusOK, updated, err)
}

// DeleteOrganizationRole removes an unassigned role at its observed revision.
func (server handler) DeleteOrganizationRole(writer http.ResponseWriter, request *http.Request, organization, role string, params DeleteOrganizationRoleParams) {
	if params.Revision < 1 {
		writeProblem(writer, http.StatusBadRequest, "revision must be positive")
		return
	}
	err := server.manager.DeleteOrganizationRole(request.Context(), organization, role, params.Revision)
	respond[DeletedOrganizationRole](writer, http.StatusOK, DeletedOrganizationRole{ID: role}, err)
}
