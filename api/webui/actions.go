package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type actionResult struct {
	selection url.Values
	notice    string
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
	if request.Header.Get("HX-Request") != "true" {
		http.Redirect(writer, request, "/?"+result.selection.Encode(), http.StatusSeeOther)
		return
	}
	web.show(writer, request, result.selection, result.notice)
}

func (web handler) execute(request *http.Request) (actionResult, error) {
	form := request.PostForm
	if request.URL.Path == "/ui/management/actions" {
		if form.Get("action") != "upgrade-management" {
			return actionResult{}, apiProblem{status: http.StatusBadRequest, message: "Choose a supported management action."}
		}
		_, err := requestApi[appapi.ComponentInstallation](request.Context(), web.api, http.MethodPost, "/api/v1/management/components/upgrade", nil)
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{selection: url.Values{"view": {"management"}}, notice: "Management components upgraded."}, nil
	}
	organization, space := form.Get("organization"), form.Get("space")
	result := actionResult{selection: url.Values{"view": {form.Get("view")}, "organization": {organization}, "space": {space}}}
	if result.selection.Get("view") == "" {
		result.selection.Set("view", "overview")
	}
	name := form.Get("name")
	base := organizationPath(organization)
	switch form.Get("action") {
	case "create-organization":
		created, err := requestApi[appapi.Organization](request.Context(), web.api, http.MethodPut, organizationPath(name), nil)
		if err != nil {
			return result, err
		}
		result.selection = url.Values{"organization": {created.Name}, "view": {"overview"}}
		result.notice = "Organization created. Link a cluster to create its first space."
	case "register-cluster":
		// Generated API models use pointers to preserve omitted options and explicit false values.
		reconcile := form.Get("reconcile") == "on"
		context, kubeconfig := form.Get("context"), form.Get("kubeconfig")
		body := appapi.ClusterInput{Context: &context, Reconcile: &reconcile, Kubeconfig: &kubeconfig}
		_, err := requestApi[appapi.Cluster](request.Context(), web.api, http.MethodPut, base+"/clusters/"+url.PathEscape(name), body)
		if err != nil {
			return result, err
		}
		result.selection.Set("cluster", name)
		result.selection.Set("space", name)
		result.notice = "Cluster linked and its initial space is ready. Component setup runs in the background when enabled."
	case "create-space":
		body := appapi.SpaceInput{Cluster: form.Get("cluster")}
		created, err := requestApi[appapi.Space](request.Context(), web.api, http.MethodPut, base+"/spaces/"+url.PathEscape(name), body)
		if err != nil {
			return result, err
		}
		result.selection.Set("space", created.Name)
		result.notice = "Space saved. Its cluster binding is fixed."
	case "plan-app":
		document, err := applicationDocument(form)
		if err != nil {
			return result, err
		}
		application, err := applicationIdentity(document, form.Get("application"))
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
		}
		result.selection.Set("operation", operation.ID)
		result.notice = "Deployment queued. Progress appears below."
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
		result.notice = "Component run queued. Progress appears below."
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
		result.notice = "Environment group published. Create and apply an application plan to deploy it."
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
		result.notice = "Environment group cloned."
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
