package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ListManagementSecrets reads organization-owned management secret publications.
func (server handler) ListManagementSecrets(writer http.ResponseWriter, request *http.Request, organization string) {
	result, err := server.manager.ListManagementSecrets(request.Context(), organization)
	respond[[]Secret](writer, http.StatusOK, result, err)
}

// GetManagementSecret selects the latest management publication.
func (server handler) GetManagementSecret(writer http.ResponseWriter, request *http.Request, organization, secret string) {
	server.GetManagementSecretVersion(writer, request, organization, secret, "latest")
}

// GetManagementSecretVersion reads one immutable management publication.
func (server handler) GetManagementSecretVersion(writer http.ResponseWriter, request *http.Request, organization, secret, version string) {
	result, err := server.manager.GetManagementSecret(request.Context(), organization, secret, version)
	respond[Secret](writer, http.StatusOK, result, err)
}

// PutManagementSecret validates and publishes a complete management secret group.
func (server handler) PutManagementSecret(writer http.ResponseWriter, request *http.Request, organization, secret string) {
	var body PutSecretInput
	if !server.decodeBody(writer, request, "PutSecretInput", &body) {
		return
	}
	result, err := server.manager.PutManagementSecret(request.Context(), organization, secret, apps.PutSecretInput{Type: string(body.Type), Data: body.Data, BaseVersion: body.BaseVersion})
	respond[Secret](writer, http.StatusOK, result, err)
}
