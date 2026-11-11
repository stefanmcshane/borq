package appapi

import "net/http"

// GetOrganizationPermissions exposes the enrolled administrator's current grants to authorized editors.
func (server handler) GetOrganizationPermissions(writer http.ResponseWriter, request *http.Request, organization string) {
	settings, err := server.manager.GetOrganizationPermissions(request.Context(), organization)
	respond[OrganizationPermissions](writer, http.StatusOK, settings, err)
}
