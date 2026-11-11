package appapi

import (
	"github.com/stefanmcshane/borq/business/server/apps"
	"net/http"
)

// ListSecrets exposes typed, space-scoped JSON documents through the API and MCP.
func (server handler) ListSecrets(writer http.ResponseWriter, request *http.Request, organization, space string) {
	result, err := server.manager.ListSecrets(request.Context(), apps.EnvironmentGroupTarget{Organization: organization, Space: space})
	respond[[]Secret](writer, http.StatusOK, result, err)
}

// GetSecret reads the latest publication without returning registry tokens.
func (server handler) GetSecret(writer http.ResponseWriter, request *http.Request, organization, space, secret string) {
	server.GetSecretVersion(writer, request, organization, space, secret, "latest")
}

// GetSecretVersion retains access to existing immutable group versions.
func (server handler) GetSecretVersion(writer http.ResponseWriter, request *http.Request, organization, space, secret, version string) {
	result, err := server.manager.GetSecret(request.Context(), apps.EnvironmentGroupTarget{Organization: organization, Space: space}, secret, version)
	respond[Secret](writer, http.StatusOK, result, err)
}

// PutSecret creates or updates a typed JSON secret without application rollouts.
func (server handler) PutSecret(writer http.ResponseWriter, request *http.Request, organization, space, secret string) {
	var body PutSecretInput
	if !server.decodeBody(writer, request, "PutSecretInput", &body) {
		return
	}
	result, err := server.manager.PutSecret(request.Context(), apps.EnvironmentGroupTarget{Organization: organization, Space: space}, secret, apps.PutSecretInput{Type: string(body.Type), Data: body.Data, BaseVersion: body.BaseVersion})
	respond[Secret](writer, http.StatusOK, result, err)
}

// DeleteSecret revokes only the secret in the requested space and honors version checks.
func (server handler) DeleteSecret(writer http.ResponseWriter, request *http.Request, organization, space, secret string, params DeleteSecretParams) {
	version := 0
	if params.Version != nil {
		if *params.Version < 1 {
			writeProblem(writer, http.StatusBadRequest, "version must be positive")
			return
		}
		version = *params.Version
	}
	err := server.manager.DeleteSecret(request.Context(), apps.EnvironmentGroupTarget{Organization: organization, Space: space}, secret, version)
	respond[DeletedSecret](writer, http.StatusOK, DeletedSecret{Name: secret}, err)
}
