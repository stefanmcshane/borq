package appapi

import (
	"errors"
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// GetOrganizationPocketID reads credential-free managed provider setup progress.
func (server handler) GetOrganizationPocketID(writer http.ResponseWriter, request *http.Request, organization string) {
	settings, err := server.manager.GetOrganizationPocketId(request.Context(), organization)
	respond[OrganizationPocketID](writer, http.StatusOK, settings, err)
}

// SetupOrganizationPocketID queues private installation before publishing and connecting OIDC.
func (server handler) SetupOrganizationPocketID(writer http.ResponseWriter, request *http.Request, organization string) {
	var input apps.OrganizationPocketIdInput
	if !server.decodeBody(writer, request, "OrganizationPocketIdInput", &input) {
		return
	}
	settings, err := server.manager.SetupOrganizationPocketId(request.Context(), organization, input)
	if message := apps.OrganizationOidcRequestMessage(err); message != "" {
		status := http.StatusBadRequest
		if errors.Is(err, apps.ErrConflict) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, message)
		return
	}
	respond[OrganizationPocketID](writer, http.StatusAccepted, settings, err)
}

// GetOrganizationPocketIDConfiguration serves editable application secrets only to the trusted administration boundary.
func (server handler) GetOrganizationPocketIDConfiguration(writer http.ResponseWriter, request *http.Request, organization string) {
	configuration, err := server.manager.GetOrganizationPocketIdConfiguration(request.Context(), organization)
	respond[OrganizationPocketIDConfiguration](writer, http.StatusOK, configuration, err)
}

// CreateOrganizationPocketIDAdminEnrollment creates an initial enrollment credential only on explicit management administrator action.
func (server handler) CreateOrganizationPocketIDAdminEnrollment(writer http.ResponseWriter, request *http.Request, organization string) {
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	location, err := server.manager.PocketIdAdminEnrollment(request.Context(), organization)
	if errors.Is(err, apps.ErrPocketIdAlreadyEnrolled) {
		respond[OrganizationPocketIDAdminEnrollment](writer, http.StatusOK, OrganizationPocketIDAdminEnrollment{Enrolled: true}, nil)
		return
	}
	if message := apps.OrganizationOidcRequestMessage(err); message != "" {
		status := http.StatusBadRequest
		if errors.Is(err, apps.ErrConflict) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, message)
		return
	}
	respond[OrganizationPocketIDAdminEnrollment](writer, http.StatusOK, OrganizationPocketIDAdminEnrollment{URL: &location}, err)
}
