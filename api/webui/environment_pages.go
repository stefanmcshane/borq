package webui

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

type environmentGroupSummary struct {
	// Name identifies the published group without its secret values.
	Name string `json:"name"`
	// Type distinguishes secret groups from registry credentials.
	Type string `json:"type"`
	// Version identifies the latest immutable publication.
	Version int `json:"version"`
	// ManagedBy identifies the producer of a read-only group.
	ManagedBy string `json:"managedBy,omitempty"`
	// VariableCount reports list metadata without exposing variable names or values.
	VariableCount int `json:"variableCount"`
}

func (web handler) loadEnvironmentPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	base := spacePath(page.Organization, page.Space.Name) + "/secrets"
	page.Secret.Type = appapi.SecretTypeSecretGroup
	if page.View == "environment" {
		groups, err := requestApi[[]appapi.Secret](request.Context(), web.api, http.MethodGet, base, nil)
		if err != nil {
			return page, err
		}
		for _, group := range groups {
			page.Groups = append(page.Groups, environmentGroupSummary{Name: group.Name, Version: group.Version, Type: string(group.Type), ManagedBy: secretManagedBy(group), VariableCount: len(group.Data)})
		}
		return page, nil
	}
	page.GroupTab = selection.Get("tab")
	if page.GroupTab == "" {
		page.GroupTab = "variables"
	}
	if page.GroupTab != "variables" && page.GroupTab != "versions" && page.GroupTab != "applications" && page.GroupTab != "settings" {
		return page, apiProblem{status: http.StatusNotFound, message: "This secret group page was not found."}
	}
	name := selection.Get("group")
	if name == "" {
		if page.GroupTab != "variables" || selection.Get("version") != "" {
			return page, apiProblem{status: http.StatusNotFound, message: "Create a secret before opening its settings."}
		}
		return page, nil
	}
	secret, err := requestApi[appapi.Secret](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name), nil)
	if err != nil {
		return page, err
	}
	page.GroupLatestVersion = secret.Version
	if version := selection.Get("version"); version != "" && version != "latest" && version != strconv.Itoa(secret.Version) {
		secret, err = requestApi[appapi.Secret](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name)+"/versions/"+url.PathEscape(version), nil)
		if err != nil {
			return page, err
		}
	}
	page.Secret = secret
	values := []appapi.Variable{}
	for key, value := range secret.Data {
		values = append(values, appapi.Variable{Name: key, Value: value})
	}
	slices.SortFunc(values, func(a, b appapi.Variable) int { return strings.Compare(a.Name, b.Name) })
	group := appapi.EnvironmentGroup{Name: secret.Name, Version: secret.Version, ManagedBy: secretManagedBy(secret), Values: values}
	if secret.Type == appapi.SecretTypeRegistry && page.GroupTab != "variables" && page.GroupTab != "settings" {
		return page, apiProblem{status: http.StatusNotFound, message: "This registry secret page was not found."}
	}
	if page.GroupTab == "variables" {
		page.EnvironmentEditor.Values = group.Values
	} else {
		group.Values = nil
		page.Secret.Data = nil
	}
	page.Group, page.Title = group, group.Name
	limit := 11
	if group.ManagedBy != "" {
		limit = 1
	}
	firstVersion := 1
	if secret.OldestVersion != nil {
		firstVersion = max(firstVersion, *secret.OldestVersion)
	}
	for version := page.GroupLatestVersion; version >= firstVersion && len(page.GroupVersions) < limit; version-- {
		page.GroupVersions = append(page.GroupVersions, version)
	}
	for _, application := range page.Space.Configuration.Apps {
		if application.Env.Name == name || secret.Type == appapi.SecretTypeRegistry {
			page.GroupApplications = append(page.GroupApplications, application)
		}
	}
	return page, nil
}

// Optional producer ownership is represented by a generated API pointer.
func secretManagedBy(secret appapi.Secret) string {
	if secret.ManagedBy == nil {
		return ""
	}
	return *secret.ManagedBy
}
