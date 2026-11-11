package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

type registrySettingsData struct {
	// Configured reports saved GHCR authentication without exposing credentials or references.
	Configured bool
}

// Generated optional registry fields use pointers to represent omitted settings.
func registryConfigured(registries *appapi.Registries) bool {
	if registries == nil {
		return false
	}
	auth := (*registries)["ghcr.io"]
	return auth.CredentialsRef != nil && *auth.CredentialsRef != ""
}

func (web handler) executeRegistrySettings(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	username, token := strings.TrimSpace(form.Get("githubUsername")), form.Get("githubToken")
	if username == "" || strings.TrimSpace(token) == "" {
		return result, apiProblem{status: http.StatusBadRequest, message: "Enter your GitHub username and personal access token together."}
	}
	// Generated API models use pointers to distinguish omitted fields from replacement credentials.
	registries := appapi.Registries{"ghcr.io": {Username: &username, Token: &token}}
	var operation appapi.Operation
	var err error
	if form.Get("action") == "put-management-registry" {
		result.selection = infrastructureSelection{Node: managementNode, Tab: "settings", ComponentScope: "postgres"}.Values()
		operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodPut, "/api/v1/management/configuration", appapi.ManagementConfiguration{Registries: &registries})
		result.notice = "Saving management GHCR credentials."
	} else {
		organization, space := form.Get("organization"), form.Get("space")
		if organization == "" || space == "" {
			return result, apiProblem{status: http.StatusBadRequest, message: "Choose an organization and space before adding registry credentials."}
		}
		result.selection = url.Values{"view": {"secrets"}, "organization": {organization}, "space": {space}}
		operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodPut, spacePath(organization, space)+"/configuration", appapi.SpaceConfiguration{Registries: &registries})
		result.notice = "Saving GHCR credentials for this space."
	}
	if err != nil {
		return result, err
	}
	result.selection.Set("managementOperation", operation.ID)
	return result, nil
}
