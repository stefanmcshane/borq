package webui

import (
	"encoding/json"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/url"
)

type managementSettingsData struct {
	// Storage contains only safe connection coordinates and a credential reference.
	Storage appapi.ManagementS3
	// Components lists the editable chart overrides by known component name.
	Components []managementComponentRow
	// Operation reports aggregate apply progress.
	Operation appapi.Operation
}

type managementComponentRow struct {
	// Name identifies one component in the shared management configuration.
	Name string
	// Configuration retains accepted repo, chart, pin and inline values.
	Configuration appapi.ComponentOverride
}

func (web handler) loadManagementSettings(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	configuration, err := requestApi[appapi.ManagementConfiguration](request.Context(), web.api, http.MethodGet, "/api/v1/management/configuration", nil)
	if err != nil {
		return page, err
	}
	page.Settings.Storage, err = requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodGet, "/api/v1/management/s3", nil)
	if problem, ok := err.(apiProblem); err != nil && (!ok || problem.status != http.StatusNotFound) {
		return page, err
	}
	for _, name := range []string{"openbao", "external-secrets", "prometheus", "loki", "rustfs", "cert-manager", "traefik", "alloy", "cloudnative-pg", "plugin-barman-cloud"} {
		row := managementComponentRow{Name: name}
		if configuration.Components != nil {
			row.Configuration = (*configuration.Components)[name]
		}
		page.Settings.Components = append(page.Settings.Components, row)
	}
	if id := selection.Get("managementOperation"); id != "" {
		page.Settings.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/management/operations/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
	}
	page.Location = pageUrl("settings", page.Organization, "")
	if id := selection.Get("managementOperation"); id != "" {
		page.Location = pageUrl("settings", page.Organization, "", "managementOperation", id)
	}
	return page, nil
}

func (web handler) executeManagementSettings(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	result.selection.Set("view", "settings")
	switch form.Get("action") {
	case "put-management-s3":
		body := appapi.ManagementS3Input{Region: form.Get("region"), Bucket: form.Get("bucket"), PathStyle: form.Get("pathStyle") == "on"}
		endpoint, prefix, reference := form.Get("endpoint"), form.Get("prefix"), form.Get("credentialsRef")
		body.Endpoint = &endpoint
		body.Prefix = &prefix
		access, secret, session := form.Get("accessKey"), form.Get("secretKey"), form.Get("sessionToken")
		if access != "" || secret != "" {
			body.AccessKey = &access
			body.SecretKey = &secret
			if session != "" {
				body.SessionToken = &session
			}
		} else if reference != "" {
			body.CredentialsRef = &reference
		}
		if _, err := requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodPut, "/api/v1/management/s3", body); err != nil {
			return result, err
		}
		result.notice = "Shared storage validated and saved. Inline credentials are stored privately."
	case "delete-management-s3":
		if _, err := requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodDelete, "/api/v1/management/s3", nil); err != nil {
			return result, err
		}
		result.notice = "Unused shared storage connection removed."
	case "grant-managed-addons":
		body := appapi.OrganizationManagedAddonsInput{Enabled: form.Get("enabled") == "on"}
		if _, err := requestApi[appapi.Organization](request.Context(), web.api, http.MethodPut, organizationPath(form.Get("organization"))+"/managed-addons", body); err != nil {
			return result, err
		}
		result.notice = "Organization managed addon grant saved."
	case "put-management-component":
		body := appapi.ComponentOverride{}
		repository, chart, version := form.Get("repo"), form.Get("chart"), form.Get("version")
		if repository != "" {
			body.Repo = &repository
		}
		if chart != "" {
			body.Chart = &chart
		}
		if version != "" {
			body.Version = &version
		}
		if source := form.Get("values"); source != "" {
			var values map[string]any
			data, err := decodeYaml(source)
			if err != nil {
				return result, err
			}
			if err := json.Unmarshal(data, &values); err != nil || values == nil {
				return result, apiProblem{status: http.StatusBadRequest, message: "Component values must be a YAML object with unique keys."}
			}
			body.Values = &values
		}
		overrides := map[string]appapi.ComponentOverride{form.Get("component"): body}
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPut, "/api/v1/management/configuration", appapi.ManagementConfiguration{Components: &overrides})
		if err != nil {
			return result, err
		}
		result.selection.Set("managementOperation", operation.ID)
		result.notice = "Component configuration accepted. Apply handles installations and upgrades."
	}
	return result, nil
}
