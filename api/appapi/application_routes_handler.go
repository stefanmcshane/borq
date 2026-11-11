package appapi

import "net/http"

// GetApplicationRoutePolicy exposes bounded suffixes from the space's pinned issuer.
func (server handler) GetApplicationRoutePolicy(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath, params GetApplicationRoutePolicyParams) {
	application := ""
	if params.Application != nil {
		application = *params.Application
	}
	policy, err := server.manager.GetApplicationRoutePolicy(request.Context(), organization, space, application)
	respond[ApplicationRoutePolicy](writer, http.StatusOK, policy, err)
}
