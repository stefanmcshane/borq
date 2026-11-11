package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) executeOrganizationOidc(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	revision, err := strconv.ParseInt(form.Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		return result, apiProblem{status: http.StatusBadRequest, message: "Reload the organization settings before saving."}
	}
	// Generated API models distinguish an omitted secret from a supplied replacement.
	secret := form.Get("clientSecret")
	input := appapi.OrganizationOidcInput{IssuerURL: strings.TrimSpace(form.Get("issuerUrl")), ClientID: strings.TrimSpace(form.Get("clientId")), BaseURL: strings.TrimSpace(form.Get("borqPublicUrl")), Revision: revision}
	if email := strings.TrimSpace(form.Get("adminEmail")); email != "" {
		input.AdminEmail = &email
	}
	if secret != "" {
		input.ClientSecret = &secret
	}
	if _, err := requestApi[appapi.OrganizationOidc](request.Context(), web.api, http.MethodPut, organizationPath(form.Get("organization"))+"/oidc", input); err != nil {
		return result, err
	}
	result.selection.Set("view", "organization-authentication")
	result.selection.Del("space")
	result.notice = "Provider settings saved and discovery checked. Test the saved settings to verify the callback, or open the sign-in page for normal access."
	return result, nil
}
