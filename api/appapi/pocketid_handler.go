package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ListPocketID lists managed identity providers without enrollment credentials.
func (server handler) ListPocketID(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath) {
	result, err := server.manager.ListPocketId(request.Context(), organization, space)
	respond[[]PocketID](writer, http.StatusOK, result, err)
}

// GetPocketID observes the managed provider and its latest operation.
func (server handler) GetPocketID(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, pocketid PocketIDPath) {
	result, err := server.manager.GetPocketId(request.Context(), organization, space, pocketid)
	respond[PocketID](writer, http.StatusOK, result, err)
}

// PutPocketID accepts creation or an explicit update while retaining SQLite data.
func (server handler) PutPocketID(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, pocketid PocketIDPath, parameters PutPocketIDParams) {
	var input apps.PocketIdInput
	if !server.decodeBody(writer, request, "PocketIdInput", &input) {
		return
	}
	result, err := server.manager.PutPocketId(request.Context(), organization, space, pocketid, input, parameters.Apply != nil && *parameters.Apply)
	respond[PocketID](writer, http.StatusAccepted, result, err)
}

// DeletePocketID removes the managed identity provider through its durable operation.
func (server handler) DeletePocketID(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, pocketid PocketIDPath) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.DeletePocketId(request.Context(), organization, space, pocketid)
	respond[PocketID](writer, http.StatusAccepted, result, err)
}

// GetPocketIDOperation observes accepted work without scheduling another update.
func (server handler) GetPocketIDOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	result, err := server.manager.GetPocketIdOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// CreatePocketIDEnrollment reveals a single-use administrator credential only on an explicit request.
func (server handler) CreatePocketIDEnrollment(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, pocketid PocketIDPath) {
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if !emptyBody(writer, request) {
		return
	}
	location, err := server.manager.NewPocketIdEnrollment(request.Context(), organization, space, pocketid)
	respond[PocketIDEnrollment](writer, http.StatusOK, PocketIDEnrollment{URL: location}, err)
}
