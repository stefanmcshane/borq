package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

type managementSettingsData struct {
	// Storage contains only safe connection coordinates and a credential reference.
	Storage appapi.ManagementS3
	// Offsite contains only safe coordinates and a reference for the external copy.
	Offsite appapi.ManagementOffsiteInput
	// BackupCoverage is nil until the user requests the potentially slow archive inspection.
	BackupCoverage *appapi.ManagementBackupCoverage `json:",omitempty"`
	// BackupCoverageError reports inspection failure without exposing service output.
	BackupCoverageError string
	// Catalog contains desired Helm input for the selected installation context.
	Catalog appapi.ComponentCatalog
	// Components links the compact catalog to one selected component.
	Components []managementComponentRow
	// Selected supplies effective input and separately saved customization.
	Selected appapi.ComponentConfiguration
	// Operation reports aggregate apply progress.
	Operation appapi.Operation
}

type managementComponentRow struct {
	Name, Version string
	// URL opens this component in the current installation context.
	URL string
	// Selected identifies the component shown in the detail panel.
	Selected bool
}

func (web handler) loadManagementSettings(request *http.Request, page pageData) (pageData, error) {
	selection := page.Infrastructure
	settings := managementSettingsData{Operation: page.Settings.Operation}
	var err error
	if selection.Node == managementNode && selection.Tab == "settings" {
		settings.Storage, err = requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodGet, "/api/v1/management/s3", nil)
		if problem, ok := err.(apiProblem); err != nil && (!ok || problem.status != http.StatusNotFound) {
			return page, err
		}
		configuration, err := requestApi[appapi.ManagementConfiguration](request.Context(), web.api, http.MethodGet, "/api/v1/management/configuration", nil)
		if err != nil {
			return page, err
		}
		page.Registry.Configured = registryConfigured(configuration.Registries)
		if policy := configuration.Offsite; policy != nil {
			settings.Offsite = appapi.ManagementOffsiteInput{Enabled: policy.Enabled, Repository: policy.Repository, Region: policy.Region, Interval: policy.Interval, KeepLast: policy.KeepLast, CredentialsRef: policy.CredentialsRef, Status: policy.Status}
		}
		if request.URL.Query().Get("checkBackups") == "true" {
			coverage, err := requestApi[appapi.ManagementBackupCoverage](request.Context(), web.api, http.MethodGet, "/api/v1/management/backups/status", nil)
			if err != nil {
				settings.BackupCoverageError = "Backup coverage could not be checked. Retry when Borq and backup storage are reachable."
			} else {
				settings.BackupCoverage = &coverage
			}
		}
		page.Settings = settings
		return page, nil
	}
	scopes := []string{selection.ComponentScope}
	if selection.Node == applicationNode {
		scopes = []string{"target", "postgres"}
	}
	selected := selection.Component
	for _, scope := range scopes {
		query := url.Values{"scope": {scope}, "organization": {selection.Organization}, "cluster": {selection.Cluster}}
		catalog, err := requestApi[appapi.ComponentCatalog](request.Context(), web.api, http.MethodGet, "/api/v1/management/component-catalog?"+query.Encode(), nil)
		if err != nil {
			return page, err
		}
		if scope == selection.ComponentScope {
			settings.Catalog = catalog
			if selected == "" && len(catalog.Components) > 0 {
				selected = catalog.Components[0].Name
			}
		}
		for _, component := range catalog.Components {
			next := selection
			next.ComponentScope, next.Component = scope, component.Name
			row := managementComponentRow{Name: component.Name, Version: component.Version, Selected: scope == selection.ComponentScope && selected == component.Name, URL: next.URL()}
			if row.Selected {
				settings.Selected = component
			}
			settings.Components = append(settings.Components, row)
		}
	}
	settings.Catalog.Components = nil
	if selected != "" && settings.Selected.Name == "" {
		return page, apiProblem{status: http.StatusNotFound, message: "This component is not available in the selected installation context."}
	}
	page.Settings = settings
	return page, nil
}

func (web handler) executeManagementSettings(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	switch form.Get("action") {
	case "put-management-offsite", "disable-management-offsite":
		body := appapi.ManagementOffsiteInput{}
		if form.Get("action") == "put-management-offsite" {
			body.Enabled = form.Get("enabled") == "on"
			repository, region, interval := form.Get("repository"), form.Get("region"), form.Get("interval")
			if strings.HasPrefix(repository, "https://") || strings.HasPrefix(repository, "http://") {
				repository = "s3:" + repository
			}
			body.Repository, body.Region, body.Interval = &repository, &region, &interval
			if value := form.Get("keepLast"); value != "" {
				keepLast, err := strconv.Atoi(value)
				if err != nil || keepLast < 1 || keepLast > 10000 {
					return result, apiProblem{status: http.StatusBadRequest, message: "Offsite snapshots to retain must be between 1 and 10000."}
				}
				body.KeepLast = &keepLast
			}
			password, access, secret, session := form.Get("password"), form.Get("accessKey"), form.Get("secretKey"), form.Get("sessionToken")
			if password != "" || access != "" || secret != "" || session != "" {
				if password == "" || access == "" || secret == "" {
					return result, apiProblem{status: http.StatusBadRequest, message: "Supply the offsite repository password, access key, and secret key together."}
				}
				body.Password, body.AccessKey, body.SecretKey = &password, &access, &secret
				if session != "" {
					body.SessionToken = &session
				}
			} else {
				configuration, err := requestApi[appapi.ManagementConfiguration](request.Context(), web.api, http.MethodGet, "/api/v1/management/configuration", nil)
				if err != nil {
					return result, err
				}
				if configuration.Offsite == nil || configuration.Offsite.CredentialsRef == nil || *configuration.Offsite.CredentialsRef == "" {
					return result, apiProblem{status: http.StatusBadRequest, message: "Enter the backup encryption password, access key, and secret key before saving offsite backups."}
				}
				body.CredentialsRef = configuration.Offsite.CredentialsRef
			}
		}
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPut, "/api/v1/management/configuration", appapi.ManagementConfiguration{Offsite: &body})
		if err != nil {
			return result, err
		}
		result.selection.Set("managementOperation", operation.ID)
		result.notice = "Offsite S3 connection test queued. Settings will be saved after the test succeeds."
		if form.Get("action") == "disable-management-offsite" {
			result.notice = "Offsite backup disable accepted. Existing snapshots and stored credentials are retained."
		}
	case "put-management-s3":
		body := appapi.ManagementS3Input{Region: form.Get("region"), Bucket: form.Get("bucket"), PathStyle: form.Get("pathStyle") == "on"}
		endpoint, prefix := form.Get("endpoint"), form.Get("prefix")
		body.Endpoint = &endpoint
		body.Prefix = &prefix
		access, secret, session := form.Get("accessKey"), form.Get("secretKey"), form.Get("sessionToken")
		if access != "" || secret != "" {
			body.AccessKey = &access
			body.SecretKey = &secret
			if session != "" {
				body.SessionToken = &session
			}
		} else {
			storage, err := requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodGet, "/api/v1/management/s3", nil)
			if err != nil {
				return result, err
			}
			body.CredentialsRef = &storage.CredentialsRef
		}
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPut, "/api/v1/management/s3", body)
		if err != nil {
			return result, err
		}
		result.selection.Set("managementOperation", operation.ID)
		result.notice = "Primary S3 connection test queued. Settings will be saved after the test succeeds."
	case "delete-management-s3":
		if _, err := requestApi[appapi.ManagementS3](request.Context(), web.api, http.MethodDelete, "/api/v1/management/s3", nil); err != nil {
			return result, err
		}
		result.notice = "Unused backup archive connection removed."
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
