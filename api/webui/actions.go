package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type actionResult struct {
	selection          url.Values
	notice             string
	pocketIdEnrollment string
}

func (web handler) action(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "The form could not be read or exceeds the 1 MiB limit."})
		return
	}
	result, err := web.execute(request)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if result.pocketIdEnrollment != "" {
		page, err := web.loadPage(request, result.selection)
		if err != nil {
			web.problem(writer, request, err)
			return
		}
		page.Notice = result.notice
		page.SpacePocketIdEnrollment = result.pocketIdEnrollment
		web.respond(writer, request, "workspace", page, http.StatusOK)
		return
	}
	web.show(writer, request, result.selection, result.notice)
}

func (web handler) execute(request *http.Request) (actionResult, error) {
	form := request.PostForm
	trimFormFields(form)
	organization, space := form.Get("organization"), form.Get("space")
	result := actionResult{selection: url.Values{"view": {form.Get("view")}, "organization": {organization}, "space": {space}}}
	if result.selection.Get("view") == "" {
		result.selection.Set("view", "overview")
	}
	switch form.Get("action") {
	case "put-organization-member", "create-organization-role", "put-organization-role", "delete-organization-role":
		return web.executeOrganizationMember(request, result)
	case "put-management-s3", "delete-management-s3", "put-management-component", "put-management-offsite", "disable-management-offsite":
		if !isInfrastructure(result.selection.Get("view")) {
			result.selection.Set("view", "settings")
		}
	}
	if isInfrastructure(result.selection.Get("view")) {
		for _, key := range []string{"node", "cluster", "tab", "componentScope", "componentOrganization", "componentCluster", "component", "managementOperation"} {
			if value := form.Get(key); value != "" {
				result.selection.Set(key, value)
			}
		}
		selection := infrastructureFromQuery(result.selection)
		if err := selection.validate(); err != nil {
			return result, err
		}
		if selection.Node == applicationNode && selection.Organization == "" {
			return result, apiProblem{status: http.StatusBadRequest, message: "Choose an organization for this cluster link."}
		}
		result.selection = selection.Values()
	}
	name := form.Get("name")
	if form.Get("application") != "" || form.Get("action") == "plan-app" {
		result.selection.Set("tab", form.Get("tab"))
		result.selection.Set("subtab", form.Get("subtab"))
	}
	base := organizationPath(organization)
	switch form.Get("action") {
	case "put-organization-oidc":
		return web.executeOrganizationOidc(request, result)
	case "put-space-registry", "put-management-registry":
		return web.executeRegistrySettings(request, result)
	case "put-space-pocketid", "delete-space-pocketid", "enroll-space-pocketid":
		return web.executeSpacePocketIdAction(request, result)
	case "put-postgres", "delete-postgres", "restore-postgres":
		return web.executePostgresAction(request, result)
	case "put-management-s3", "delete-management-s3", "put-management-component", "put-management-offsite", "disable-management-offsite":
		return web.executeManagementSettings(request, result)
	case "cancel-deployment":
		id := form.Get("deployment")
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id), nil)
		if err != nil {
			return result, err
		}
		target, err := requestApi[appapi.Space](request.Context(), web.api, http.MethodGet, spacePath(organization, space), nil)
		if err != nil {
			return result, err
		}
		if operation.SpaceID != target.ID {
			return result, apiProblem{status: http.StatusConflict, message: "Select the deployment’s organization and space before canceling it."}
		}
		if _, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPost, "/api/v1/operations/"+url.PathEscape(id)+"/cancel", nil); err != nil {
			return result, err
		}
		result.selection.Set("view", "application")
		result.selection.Set("app", form.Get("application"))
		result.selection.Set("tab", "deployments")
		result.selection.Set("operation", id)
		result.notice = "Deployment cancellation requested. Accepted settings are retained."
	case "put-custom-chart", "delete-custom-chart":
		path := spacePath(organization, space) + "/custom-charts/" + url.PathEscape(name)
		method := http.MethodDelete
		var body any
		if form.Get("action") == "put-custom-chart" {
			input, err := customChartFormInput(form)
			if err != nil {
				return result, err
			}
			body, method = input, http.MethodPut
			if form.Get("apply") == "true" {
				path += "?apply=true"
			}
		}
		customChart, err := requestApi[appapi.CustomChart](request.Context(), web.api, method, path, body)
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "custom-chart")
		result.selection.Set("customChart", customChart.Name)
		result.notice = customChart.Message
		if method == http.MethodDelete {
			result.selection.Set("view", "custom-charts")
			result.selection.Del("customChart")
		}
	case "create-organization":
		body := organizationOnboardingForm(form)
		created, err := requestApi[appapi.Organization](request.Context(), web.api, http.MethodPut, organizationPath(name), body)
		if err != nil {
			return result, err
		}
		if created.AuthenticationState != nil && *created.AuthenticationState == "queued" {
			result.selection = url.Values{"organization": {created.Name}, "view": {"organization-authentication"}}
			result.notice = "Organization created. Its login provider setup is queued; sign-in becomes available when setup finishes."
		} else {
			result.selection = url.Values{"organization": {created.Name}, "view": {"overview"}, "setup": {"cluster"}}
			result.notice = "Organization created. Link a cluster to create its first deployment space."
		}
	case "register-cluster":
		// Generated API models use pointers to preserve omitted options and explicit false values.
		reconcile := true
		kubeconfig := form.Get("kubeconfig")
		body := appapi.ClusterInput{Reconcile: &reconcile, Kubeconfig: &kubeconfig}
		created, err := requestApi[appapi.Cluster](request.Context(), web.api, http.MethodPost, base+"/clusters", appapi.LinkClusterInput{Name: name, Connection: body})
		if err != nil {
			return result, err
		}
		result.selection.Set("cluster", created.Name)
		result.selection.Del("space")
		if isInfrastructure(result.selection.Get("view")) {
			result.selection.Set("view", "infrastructure")
			result.selection.Set("node", "application")
			result.selection.Set("organization", organization)
			result.selection.Set("componentScope", "target")
			result.selection.Set("tab", "metrics")
		}
		if form.Get("setup") == "cluster" {
			result.selection = url.Values{"organization": {organization}, "view": {"overview"}}
		}
		result.notice = "Cluster linked and its initial space created. Required component setup was requested."
	case "create-space":
		body := appapi.SpaceInput{Cluster: form.Get("cluster")}
		created, err := requestApi[appapi.Space](request.Context(), web.api, http.MethodPut, base+"/spaces/"+url.PathEscape(name), body)
		if err != nil {
			return result, err
		}
		result.selection.Set("space", created.Name)
		if result.selection.Get("view") == "spaces" {
			result.selection.Del("space")
		}
		result.notice = "Space saved. Its cluster binding is fixed."
	case "plan-app":
		document, err := web.applicationDocument(request, form)
		if err != nil {
			return result, err
		}
		application, err := applicationIdentity(document, form.Get("application"))
		if err != nil {
			return result, err
		}
		document, err = web.attachApplicationSecrets(request, application, document)
		if err != nil {
			return result, err
		}
		body := struct {
			Apps []json.RawMessage `json:"apps"`
		}{Apps: []json.RawMessage{document}}
		plan, err := requestApi[appapi.Plan](request.Context(), web.api, http.MethodPost, spacePath(organization, space)+"/plans", body)
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "application")
		result.selection.Set("app", application)
		result.selection.Set("plan", plan.ID)
		result.notice = "Plan saved. Review the changes before applying them."
	case "delete-app":
		application := form.Get("application")
		path := spacePath(organization, space) + "/apps/" + url.PathEscape(application)
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodDelete, path, nil)
		if err != nil {
			return result, err
		}
		result.selection = url.Values{"view": {"applications"}, "organization": {organization}, "space": {space}, "operation": {operation.ID}}
		result.notice = "Application deletion started. SQLite volumes, backups and secrets will be retained."
	case "restart-app":
		application := form.Get("application")
		path := spacePath(organization, space) + "/apps/" + url.PathEscape(application) + "/restart"
		if _, err := requestApi[appapi.ResourceStatus](request.Context(), web.api, http.MethodPost, path, nil); err != nil {
			return result, err
		}
		result.selection.Set("view", "application")
		result.selection.Set("app", application)
		result.selection.Set("tab", "overview")
		result.notice = "Rolling restart started. Live health updates below."
	case "scale-app":
		application := form.Get("application")
		replicas, parseError := strconv.Atoi(form.Get("replicas"))
		if parseError != nil || replicas < 0 || int64(replicas) > 2147483647 {
			return result, apiProblem{status: http.StatusBadRequest, message: "Enter a valid replica count."}
		}
		plan, err := requestApi[appapi.Plan](request.Context(), web.api, http.MethodPost, spacePath(organization, space)+"/plans", appapi.Patch{Apps: &[]appapi.AppPatch{{Name: application, Replicas: &replicas}}})
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "application")
		result.selection.Set("app", application)
		result.selection.Set("plan", plan.ID)
		result.notice = "Review the proposed changes, then deploy when ready."
	case "apply-plan":
		plan, err := requestApi[appapi.Plan](request.Context(), web.api, http.MethodGet, "/api/v1/plans/"+url.PathEscape(form.Get("plan")), nil)
		if err != nil {
			return result, err
		}
		if plan.Space.Organization != organization || plan.Space.Name != space {
			return result, apiProblem{status: http.StatusConflict, message: "Select the plan’s organization and space before applying it."}
		}
		operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodPost, "/api/v1/plans/"+url.PathEscape(plan.ID)+"/apply", nil)
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "applications")
		if application := form.Get("application"); application != "" {
			result.selection.Set("view", "application")
			result.selection.Set("app", application)
			result.selection.Set("tab", "overview")
			result.selection.Del("subtab")
		}
		result.selection.Set("operation", operation.ID)
		result.notice = "Deployment accepted. It replaces unfinished deployments in this space."
	case "reconcile-cluster", "upgrade-cluster":
		operation := "reconcile"
		if form.Get("action") == "upgrade-cluster" {
			operation = "upgrade"
		}
		_, err := requestApi[appapi.ComponentInstallation](request.Context(), web.api, http.MethodPost, base+"/clusters/"+url.PathEscape(form.Get("cluster"))+"/components/"+operation, nil)
		if err != nil {
			return result, err
		}
		result.selection.Set("cluster", form.Get("cluster"))
		result.selection.Set("view", "infrastructure")
		result.selection.Set("node", "application")
		if result.selection.Get("componentScope") == "" {
			result.selection.Set("componentScope", "target")
		}
		result.selection.Set("tab", "components")
		result.selection.Del("space")
		result.notice = "Component run queued. Progress appears below."
	case "delete-secret":
		version, err := strconv.Atoi(form.Get("baseVersion"))
		if err != nil || version < 1 {
			return result, apiProblem{status: http.StatusBadRequest, message: "Reload the secret before deleting it."}
		}
		_, err = requestApi[appapi.DeletedSecret](request.Context(), web.api, http.MethodDelete, spacePath(organization, space)+"/secrets/"+url.PathEscape(name)+"?version="+strconv.Itoa(version), nil)
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "secrets")
		result.notice = "Secret deleted. Applications were not redeployed."
	case "put-secret":
		values, err := environmentValues(form)
		if err != nil {
			return result, err
		}
		data := map[string]string{}
		for _, value := range values {
			if _, exists := data[value.Name]; exists {
				return result, apiProblem{status: http.StatusBadRequest, message: "Use a unique key for each value."}
			}
			data[value.Name] = value.Value
		}
		version, err := strconv.Atoi(form.Get("baseVersion"))
		if err != nil || version < 0 {
			return result, apiProblem{status: http.StatusBadRequest, message: "Reload the secret before saving it."}
		}
		body := appapi.PutSecretInput{Type: appapi.PutSecretInputType(form.Get("secretType")), Data: data, BaseVersion: &version}
		secret, err := requestApi[appapi.Secret](request.Context(), web.api, http.MethodPut, spacePath(organization, space)+"/secrets/"+url.PathEscape(name), body)
		if err != nil {
			return result, err
		}
		result.selection.Set("view", "secret")
		result.selection.Set("group", secret.Name)
		result.selection.Set("tab", "variables")
		result.notice = "Secret saved. Applications adopt changes when you review and apply a deployment."
	case "publish-group":
		body, err := environmentPublication(form)
		if err != nil {
			return result, err
		}
		group, err := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodPost, spacePath(organization, space)+"/environment-groups/"+url.PathEscape(name)+"/versions", body)
		if err != nil {
			return result, err
		}
		result.selection.Set("group", group.Name)
		result.selection.Set("view", "environment-group")
		result.selection.Set("tab", "variables")
		result.notice = "Secret group published. Create and apply an application plan to deploy it."
	case "clone-group":
		version := form.Get("sourceVersion")
		body := appapi.CloneEnvironmentGroupInput{SourceName: form.Get("sourceName")}
		if version != "" {
			body.SourceVersion = &version
		}
		group, err := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodPost, spacePath(organization, space)+"/environment-groups/"+url.PathEscape(name)+"/clone", body)
		if err != nil {
			return result, err
		}
		result.selection.Set("group", group.Name)
		result.selection.Set("view", "environment-group")
		result.selection.Set("tab", "variables")
		result.notice = "Secret group cloned."
	default:
		return result, apiProblem{status: http.StatusBadRequest, message: "Choose a supported workspace action."}
	}
	return result, nil
}

// decodeYaml is a form I/O boundary; the API remains the owner of schema validation.
func decodeYaml(source string) (json.RawMessage, error) {
	decoder := yamlreader.NewDecoder(strings.NewReader(source))
	var document yamlreader.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Enter one valid YAML document."}
	}
	var extra yamlreader.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Only one YAML document is accepted."}
	}
	data, err := yaml.YAMLToJSONStrict([]byte(source))
	if err != nil {
		return nil, apiProblem{status: http.StatusBadRequest, message: "YAML must be valid and use unique keys."}
	}
	return json.RawMessage(data), nil
}

func organizationOnboardingForm(form url.Values) appapi.OrganizationOnboardingInput {
	input := appapi.OrganizationOnboardingInput{}
	if email := strings.TrimSpace(form.Get("adminEmail")); email != "" {
		input.AdminEmail = &email
	}
	switch form.Get("provider") {
	case "external":
		secret := form.Get("clientSecret")
		input.Oidc = &appapi.OrganizationOidcInput{IssuerURL: form.Get("issuerUrl"), BaseURL: form.Get("borqPublicUrl"), ClientID: form.Get("clientId"), ClientSecret: &secret}
	case "new-pocketid":
		input.PocketID = &appapi.OrganizationPocketIDInput{IssuerURL: form.Get("issuerUrl"), BaseURL: form.Get("borqPublicUrl"), AdminEmail: form.Get("pocketIdAdminEmail"), CertificateIssuer: form.Get("certificateIssuer")}
	case "reuse":
		if source := strings.TrimSpace(form.Get("pocketIdOrganization")); source != "" {
			input.PocketIDOrganization = &source
		}
	}
	return input
}
