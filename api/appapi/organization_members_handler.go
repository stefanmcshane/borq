package appapi

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ListOrganizationMembers returns provider-approved members and their effective role permissions.
func (server handler) ListOrganizationMembers(writer http.ResponseWriter, request *http.Request, organization string) {
	members, err := server.manager.ListOrganizationMembers(request.Context(), organization)
	respond[[]OrganizationMember](writer, http.StatusOK, members, err)
}

// PutOrganizationMember replaces role assignments using the caller's observed revision.
func (server handler) PutOrganizationMember(writer http.ResponseWriter, request *http.Request, organization, member string) {
	var input apps.OrganizationMemberInput
	if !server.decodeBody(writer, request, "OrganizationMemberInput", &input) {
		return
	}
	updated, err := server.manager.PutOrganizationMember(request.Context(), organization, member, input)
	respond[OrganizationMember](writer, http.StatusOK, updated, err)
}

// CreateAPIToken delegates only permissions held by the current organization user.
func (server handler) CreateAPIToken(writer http.ResponseWriter, request *http.Request) {
	var input apps.CreateApiTokenInput
	if !server.decodeBody(writer, request, "CreateApiTokenInput", &input) {
		return
	}
	principal, _ := apps.PrincipalFromContext(request.Context())
	token, err := server.manager.CreateApiToken(request.Context(), input)
	if err != nil && principal.Admin {
		writeProblem(writer, http.StatusForbidden, "Sign in with an organization account to create a user token.")
		return
	}
	respond[APIToken](writer, http.StatusOK, token, err)
}
