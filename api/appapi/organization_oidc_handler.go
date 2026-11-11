package appapi

import (
	"errors"
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// GetOrganizationOidc reads setup metadata without exposing provider credentials.
func (server handler) GetOrganizationOidc(writer http.ResponseWriter, request *http.Request, organization string) {
	settings, err := server.manager.GetOrganizationOidc(request.Context(), organization)
	respond[OrganizationOidc](writer, http.StatusOK, settings, err)
}

// PutOrganizationOidc saves provider settings and the selected organization administrator.
func (server handler) PutOrganizationOidc(writer http.ResponseWriter, request *http.Request, organization string) {
	var input apps.OrganizationOidcInput
	if !server.decodeBody(writer, request, "OrganizationOidcInput", &input) {
		return
	}
	settings, err := server.manager.PutOrganizationOidc(request.Context(), organization, input)
	if message := apps.OrganizationOidcRequestMessage(err); message != "" {
		status := http.StatusBadRequest
		if errors.Is(err, apps.ErrConflict) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, message)
		return
	}
	respond[OrganizationOidc](writer, http.StatusOK, settings, err)
}

// EnrollOrganizationOidcAdministrator pins an account using installation administrator authority.
func (server handler) EnrollOrganizationOidcAdministrator(writer http.ResponseWriter, request *http.Request, organization string) {
	var input apps.OrganizationOidcAdministratorInput
	if !server.decodeBody(writer, request, "OrganizationOidcAdministratorInput", &input) {
		return
	}
	permissions, err := server.manager.EnrollOrganizationOidcAdministrator(request.Context(), organization, input)
	if message := apps.OrganizationOidcRequestMessage(err); message != "" {
		status := http.StatusBadRequest
		if errors.Is(err, apps.ErrConflict) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, message)
		return
	}
	respond[OrganizationPermissions](writer, http.StatusOK, permissions, err)
}
