package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

type applicationEditorData struct {
	// Organization and Space scope navigation between configuration views.
	Organization, Space string
	// Format selects the guided form or a single application YAML document.
	Format string
	// Draft supplies the editable application fields.
	Draft appapi.App
	// Document preserves omitted settings during partial YAML edits.
	Document string
	// Yaml contains one application, without a space-wide apps wrapper.
	Yaml string
	// ExistingName fixes the identity when editing an accepted application.
	ExistingName string
	// SavedIngresses fixes deployed route identities until resource deletion is supported.
	SavedIngresses []string
	// Ingresses supplies route cards with removal available for unsaved rows.
	Ingresses []applicationIngressData
	// Checks supplies the two independently editable HTTP probes.
	Checks []applicationCheckData
	// GroupNames contains published choices without carrying environment values.
	GroupNames []string
	// Namespace identifies the actual Service namespace for the internal address.
	Namespace string
	// EnvironmentUrl opens group management in this editor's organization and space.
	EnvironmentUrl string
}

type applicationCheckData struct {
	// Name is the prefix used to submit one probe's fields.
	Name string
	// Title labels the probe card.
	Title string
	// Description explains when the probe affects the application.
	Description string
	// Check contains the probe's complete editable values.
	Check appapi.Check
	// PortSet distinguishes an explicit probe port from the container port default.
	PortSet bool
}

type applicationIngressData struct {
	// Route contains the editable host and path of one named HTTPS route.
	Route appapi.ExpandedIngress
	// Saved prevents a named upsert from appearing to delete a deployed route.
	Saved bool
	// Focus moves keyboard focus to a newly added route after the editor refreshes.
	Focus bool
}

func applicationDocument(form url.Values) (json.RawMessage, error) {
	if form.Get("format") == "yaml" {
		return decodeYaml(form.Get("configuration"))
	}
	patch, err := applicationFields(form)
	if err != nil {
		return nil, err
	}
	return json.Marshal(patch)
}

func applicationEditorView(document json.RawMessage, existingName string, savedIngresses []string) (applicationEditorData, error) {
	editor := applicationEditorData{Format: "fields", Document: string(document), ExistingName: existingName, SavedIngresses: savedIngresses,
		Draft: appapi.App{Port: 8080, Replicas: 1, Predeploy: appapi.Predeploy{TimeoutSeconds: 300},
			Checks: appapi.Checks{
				Readiness: appapi.Check{Enabled: true, Path: "/healthz", PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3},
				Startup:   appapi.Check{Enabled: true, Path: "/healthz", PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3},
			}}}
	if err := json.Unmarshal(document, &editor.Draft); err != nil {
		return editor, err
	}
	var patch appapi.AppPatch
	if err := json.Unmarshal(document, &patch); err != nil {
		return editor, err
	}
	if patch.Checks == nil || patch.Checks.Readiness == nil || patch.Checks.Readiness.Port == nil {
		editor.Draft.Checks.Readiness.Port = editor.Draft.Port
	}
	if patch.Checks == nil || patch.Checks.Startup == nil || patch.Checks.Startup.Port == nil {
		editor.Draft.Checks.Startup.Port = editor.Draft.Port
	}
	if patch.Checks == nil || patch.Checks.Readiness == nil || patch.Checks.Readiness.Enabled == nil {
		editor.Draft.Checks.Readiness.Enabled = editor.Draft.Port > 0
	}
	if patch.Checks == nil || patch.Checks.Startup == nil || patch.Checks.Startup.Enabled == nil {
		editor.Draft.Checks.Startup.Enabled = editor.Draft.Port > 0
	}
	for _, route := range editor.Draft.Ingresses {
		editor.Ingresses = append(editor.Ingresses, applicationIngressData{Route: route, Saved: slices.Contains(savedIngresses, route.Name)})
	}
	editor.Checks = []applicationCheckData{
		{Name: "readiness", Title: "Readiness", Description: "Decides when this app can receive traffic.", Check: editor.Draft.Checks.Readiness,
			PortSet: patch.Checks != nil && patch.Checks.Readiness != nil && patch.Checks.Readiness.Port != nil},
		{Name: "startup", Title: "Startup", Description: "Allows time for this app to start successfully.", Check: editor.Draft.Checks.Startup,
			PortSet: patch.Checks != nil && patch.Checks.Startup != nil && patch.Checks.Startup.Port != nil},
	}
	return editor, nil
}

func applicationIdentity(document json.RawMessage, existingName string) (string, error) {
	var application struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(document, &application); err != nil {
		return "", apiProblem{status: http.StatusBadRequest, message: "Enter one application as a YAML mapping, starting with name and image."}
	}
	if existingName != "" && application.Name != existingName {
		return "", apiProblem{status: http.StatusBadRequest, message: "Keep this application's name when editing. Use Add an application to create another app."}
	}
	return application.Name, nil
}

func (web handler) applicationEditor(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "The form could not be read or exceeds the 1 MiB limit."})
		return
	}
	form := request.PostForm
	if form.Has("removeIngress") {
		index, err := strconv.Atoi(form.Get("removeIngress"))
		if err != nil || index < 0 || index >= len(form["ingressName"]) || len(form["ingressName"]) != len(form["ingressHost"]) || len(form["ingressName"]) != len(form["ingressPath"]) {
			web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an unsaved route to remove."})
			return
		}
		if slices.Contains(form["savedIngress"], form["ingressName"][index]) {
			web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Deployed routes can be updated, but cannot be removed yet."})
			return
		}
		for _, key := range []string{"ingressName", "ingressHost", "ingressPath"} {
			form[key] = slices.Delete(form[key], index, index+1)
		}
	}
	document, err := applicationDocument(form)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if _, err := applicationIdentity(document, form.Get("application")); err != nil {
		web.problem(writer, request, err)
		return
	}
	// Conversion rejects unsupported values before they can be lost in typed controls.
	var patch appapi.AppPatch
	if source := form.Get("document"); source != "" {
		if err := json.Unmarshal([]byte(source), &patch); err != nil {
			web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Reload the application editor and try again."})
			return
		}
	}
	var shape any
	if err := json.Unmarshal(document, &shape); err != nil || applicationContainsNull(shape) {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Use application values instead of null; omit settings you want to keep."})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Application YAML must use the supported application fields and value types."})
		return
	}
	document, err = json.Marshal(patch)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	editor, err := applicationEditorView(document, form.Get("application"), form["savedIngress"])
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	editor.GroupNames, editor.Namespace = form["environmentChoice"], form.Get("serviceNamespace")
	editor.Organization, editor.Space = form.Get("organization"), form.Get("space")
	editor.EnvironmentUrl = pageUrl("environment", form.Get("organization"), form.Get("space"))
	if form.Get("editorAction") == "add-ingress" {
		editor.Ingresses = append(editor.Ingresses, applicationIngressData{Route: appapi.ExpandedIngress{Path: "/"}, Focus: true})
	}
	if form.Get("editorFormat") == "yaml" || (form.Get("editorFormat") == "" && form.Get("yamlMode") == "on") {
		editor.Format = "yaml"
		data, err := yaml.JSONToYAML(document)
		if err != nil {
			web.problem(writer, request, err)
			return
		}
		editor.Yaml = string(data)
	}
	if request.Header.Get("HX-Request") == "true" {
		web.render(writer, request, "application-editor", editor, http.StatusOK)
		return
	}
	page, err := web.loadPage(request, url.Values{"view": {"application"}, "organization": {form.Get("organization")}, "space": {form.Get("space")}, "app": {editor.ExistingName}, "tab": {"configuration"}, "subtab": {editor.Format}})
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	page.ApplicationEditor = editor
	web.render(writer, request, "page", page, http.StatusOK)
}

func applicationContainsNull(value any) bool {
	if value == nil {
		return true
	}
	switch value := value.(type) {
	case map[string]any:
		for _, field := range value {
			if applicationContainsNull(field) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if applicationContainsNull(item) {
				return true
			}
		}
	}
	return false
}
