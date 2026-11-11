package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) attachApplicationSecrets(request *http.Request, application string, document json.RawMessage) (json.RawMessage, error) {
	form := request.PostForm
	values, err := applicationSecretValues(form)
	if err != nil || len(values) == 0 {
		return document, err
	}
	if application == "" || form.Get("secretDraft") == "" {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Enter an application name and reload the editor if needed before saving secrets."}
	}
	data := make(map[string]string, len(values))
	for _, variable := range values {
		if variable.Name == "" {
			return nil, apiProblem{status: http.StatusBadRequest, message: "Each secret needs a key."}
		}
		if _, exists := data[variable.Name]; exists {
			return nil, apiProblem{status: http.StatusBadRequest, message: "Use a unique key for each secret."}
		}
		data[variable.Name] = variable.Value
	}
	// Validate before replacing env so custom secrets cannot hide invalid selectors.
	var shape map[string]any
	if err := json.Unmarshal(document, &shape); err != nil {
		return nil, err
	}
	if environment, supplied := shape["env"]; supplied {
		specification, err := appapi.GetSwagger()
		if err != nil {
			return nil, err
		}
		if err := specification.Components.Schemas["EnvReferencePatch"].Value.VisitJSON(environment); err != nil {
			return nil, apiProblem{status: http.StatusBadRequest, message: "Use a valid secret group name and version."}
		}
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(document, &patch); err != nil {
		return nil, err
	}
	source, err := applicationSecretSource(form.Get("document"), document)
	if err != nil {
		return nil, err
	}
	name := application + "-" + form.Get("secretDraft")
	base := spacePath(form.Get("organization"), form.Get("space")) + "/environment-groups/"
	if source.Name != "" && source.Version != "0" {
		group, err := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, base+url.PathEscape(source.Name)+"/versions/"+url.PathEscape(source.Version), nil)
		if err != nil {
			return nil, err
		}
		if group.ManagedBy != "" {
			return nil, apiProblem{status: http.StatusConflict, message: "Custom secrets cannot be combined with a managed group because its credentials rotate. Use the managed group without custom secrets."}
		}
		for _, variable := range group.Values {
			if _, overridden := data[variable.Name]; !overridden {
				data[variable.Name] = variable.Value
			}
		}
		// A different selected group or a newer latest version needs its own
		// publication, while retrying the same source snapshot remains idempotent.
		snapshot := sha256.Sum256([]byte(group.Name + "/" + strconv.Itoa(group.Version)))
		name += "-" + hex.EncodeToString(snapshot[:8])
	}
	values = make([]appapi.Variable, 0, len(data))
	for _, key := range slices.Sorted(maps.Keys(data)) {
		values = append(values, appapi.Variable{Name: key, Value: data[key]})
	}
	path := base + url.PathEscape(name)
	// A create-only write prevents a draft from replacing a shared or managed group.
	baseVersion := 0
	secret, err := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodPost, path+"/versions", appapi.PublishEnvironmentGroupInput{Values: values, BaseVersion: &baseVersion})
	if problem, ok := err.(apiProblem); ok && problem.status == http.StatusConflict {
		// A failed plan or lost response can leave the publication saved. Reuse
		// only identical values; changes in the editor receive a new draft identity.
		existing, readErr := requestApi[appapi.EnvironmentGroup](request.Context(), web.api, http.MethodGet, path, nil)
		existingValues := make(map[string]string, len(existing.Values))
		for _, variable := range existing.Values {
			existingValues[variable.Name] = variable.Value
		}
		if readErr == nil && existing.ManagedBy == "" && maps.Equal(existingValues, data) {
			secret, err = existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	patch["env"], err = json.Marshal(appapi.EnvReference{Name: secret.Name, Version: strconv.Itoa(secret.Version)})
	if err != nil {
		return nil, err
	}
	return json.Marshal(patch)
}

func applicationSecretSource(saved string, document json.RawMessage) (appapi.EnvReference, error) {
	var current struct {
		Env appapi.EnvReference `json:"env"`
	}
	if saved != "" {
		if err := json.Unmarshal([]byte(saved), &current); err != nil {
			return current.Env, apiProblem{status: http.StatusBadRequest, message: "Reload the application editor and try again."}
		}
	}
	var patch struct {
		Env *appapi.EnvReferencePatch `json:"env"`
	}
	if err := json.Unmarshal(document, &patch); err != nil {
		return current.Env, apiProblem{status: http.StatusBadRequest, message: "Choose a secret group name and version."}
	}
	if patch.Env != nil {
		if patch.Env.Name != nil && *patch.Env.Name != "" {
			current.Env.Name = *patch.Env.Name
		}
		current.Env.Version = "latest"
		if patch.Env.Version != nil {
			current.Env.Version = *patch.Env.Version
		}
	}
	if current.Env.Version == "" {
		current.Env.Version = "latest"
	}
	return current.Env, nil
}

func applicationSecretValues(form url.Values) ([]appapi.Variable, error) {
	names := make([]string, len(form["variableName"]))
	for index, name := range form["variableName"] {
		names[index] = strings.TrimSpace(name)
	}
	return environmentValues(url.Values{"format": {"fields"}, "variableName": names, "variableValue": form["variableValue"]})
}
