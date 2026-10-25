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
	// Catalog contains desired Helm input for the selected installation context.
	Catalog appapi.ComponentCatalog
	// Components links the compact catalog to one selected component.
	Components []managementComponentRow
	// Selected supplies effective input and separately saved customization.
	Selected appapi.ComponentConfiguration
	// Scope, Organization, and Cluster preserve the component preview context.
	Scope, Organization, Cluster string
	// Scopes and Targets provide canonical context navigation.
	Scopes, Targets []managementSettingsOption
	// Operation reports aggregate apply progress.
	Operation appapi.Operation
}

type managementSettingsOption struct {
	// Label names one navigation choice.
	Label string
	// URL retains only safe component selection parameters.
	URL string
	// Selected identifies the current choice.
	Selected bool
}

type managementComponentRow struct {
	appapi.ComponentConfiguration
	// URL opens this component in the current installation context.
	URL string
	// Selected identifies the component shown in the detail panel.
	Selected bool
}

func managementSettingsUrl(scope, organization, cluster, component, operation string) string {
	selection := url.Values{"view": {"settings"}, "componentScope": {scope}}
	for key, value := range map[string]string{"componentOrganization": organization, "componentCluster": cluster, "component": component, "managementOperation": operation} {
		if value != "" {
			selection.Set(key, value)
		}
	}
	return "/?" + selection.Encode()
}

func (web handler) loadManagementSettings(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	var err error
	page.Settings.Storage, err = requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodGet, "/api/v1/management/s3", nil)
	if problem, ok := err.(apiProblem); err != nil && (!ok || problem.status != http.StatusNotFound) {
		return page, err
	}
	settings := page.Settings
	operation := selection.Get("managementOperation")
	settings.Scope = selection.Get("componentScope")
	if settings.Scope == "" {
		settings.Scope = "management"
	}
	settings.Organization, settings.Cluster = selection.Get("componentOrganization"), selection.Get("componentCluster")
	for _, scope := range []struct{ name, label string }{{"management", "Management"}, {"target", "Deployment clusters"}, {"postgres", "PostgreSQL controllers"}} {
		settings.Scopes = append(settings.Scopes, managementSettingsOption{Label: scope.label, URL: managementSettingsUrl(scope.name, "", "", "", operation), Selected: settings.Scope == scope.name})
	}
	if settings.Scope == "target" {
		var targets []appapi.Cluster
		for _, organization := range page.Organizations {
			clusters, err := requestApi[[]appapi.Cluster](request.Context(), web.api, http.MethodGet, organizationPath(organization.Name)+"/clusters", nil)
			if err != nil {
				return page, err
			}
			for _, cluster := range clusters {
				cluster.Organization = organization.Name
				targets = append(targets, cluster)
			}
		}
		if settings.Organization == "" && settings.Cluster == "" && len(targets) > 0 {
			settings.Organization, settings.Cluster = targets[0].Organization, targets[0].Name
		}
		for _, cluster := range targets {
			settings.Targets = append(settings.Targets, managementSettingsOption{Label: cluster.Organization + " / " + cluster.Name, URL: managementSettingsUrl(settings.Scope, cluster.Organization, cluster.Name, selection.Get("component"), operation), Selected: settings.Organization == cluster.Organization && settings.Cluster == cluster.Name})
		}
	}
	if settings.Scope != "target" || settings.Organization != "" || settings.Cluster != "" {
		query := url.Values{"scope": {settings.Scope}, "organization": {settings.Organization}, "cluster": {settings.Cluster}}
		settings.Catalog, err = requestApi[appapi.ComponentCatalog](request.Context(), web.api, http.MethodGet, "/api/v1/management/component-catalog?"+query.Encode(), nil)
		if err != nil {
			return page, err
		}
		selected := selection.Get("component")
		if selected == "" && len(settings.Catalog.Components) > 0 {
			selected = settings.Catalog.Components[0].Name
		}
		for _, component := range settings.Catalog.Components {
			row := managementComponentRow{ComponentConfiguration: component, Selected: selected == component.Name, URL: managementSettingsUrl(settings.Scope, settings.Organization, settings.Cluster, component.Name, operation)}
			settings.Components = append(settings.Components, row)
			if row.Selected {
				settings.Selected = component
			}
		}
		if selected != "" && settings.Selected.Name == "" {
			return page, apiProblem{status: http.StatusNotFound, message: "This component is not available in the selected installation context."}
		}
	}
	if operation != "" {
		settings.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/management/operations/"+url.PathEscape(operation), nil)
		if err != nil {
			return page, err
		}
	}
	page.Settings = settings
	page.Location = managementSettingsUrl(settings.Scope, settings.Organization, settings.Cluster, settings.Selected.Name, operation)
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
		for _, key := range []string{"componentScope", "componentOrganization", "componentCluster", "component"} {
			if value := form.Get(key); value != "" {
				result.selection.Set(key, value)
			}
		}
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
