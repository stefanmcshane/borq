package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

type environmentEditorData struct {
	// Format selects key/value fields or a bare YAML list.
	Format string
	// Values supplies the editable literal variables.
	Values []appapi.Variable
	// Yaml holds the list without the API publication wrapper.
	Yaml string
}

func environmentPublication(form url.Values) (appapi.PublishEnvironmentGroupInput, error) {
	trimFormFields(form)
	version, err := strconv.Atoi(form.Get("baseVersion"))
	if err != nil || version < 0 {
		return appapi.PublishEnvironmentGroupInput{}, apiProblem{status: http.StatusBadRequest, message: "Reload this group before publishing to get its current version."}
	}
	values, err := environmentValues(form)
	return appapi.PublishEnvironmentGroupInput{Values: values, BaseVersion: &version}, err
}

func environmentValues(form url.Values) ([]appapi.Variable, error) {
	values := []appapi.Variable{}
	switch form.Get("format") {
	case "fields":
		names, literals := form["variableName"], form["variableValue"]
		if len(names) != len(literals) {
			return nil, apiProblem{status: http.StatusBadRequest, message: "Each variable needs a key and a value field."}
		}
		for index, name := range names {
			if name == "" && literals[index] == "" {
				continue
			}
			// Browser form encoding uses CRLF for textarea line endings.
			values = append(values, appapi.Variable{Name: name, Value: strings.ReplaceAll(literals[index], "\r\n", "\n")})
		}
	case "yaml":
		data, err := decodeYaml(form.Get("values"))
		if err != nil {
			return nil, err
		}
		// Pointers distinguish omitted or null entries from literal empty strings.
		var entries []struct {
			Name  *string `json:"name"`
			Value *string `json:"value"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		invalidList := apiProblem{status: http.StatusBadRequest, message: "Enter a YAML list with a name and a string value for each variable. Quote numbers and true/false to keep them as text."}
		if err := decoder.Decode(&entries); err != nil || entries == nil {
			return nil, invalidList
		}
		for _, entry := range entries {
			if entry.Name == nil || entry.Value == nil {
				return nil, invalidList
			}
			values = append(values, appapi.Variable{Name: strings.TrimSpace(*entry.Name), Value: *entry.Value})
		}
	default:
		return nil, apiProblem{status: http.StatusBadRequest, message: "Choose key/value fields or YAML to enter variables."}
	}
	return values, nil
}

func (web handler) environmentEditor(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "The form could not be read or exceeds the 1 MiB limit."})
		return
	}
	body, err := environmentPublication(request.PostForm)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	editor := environmentEditorData{Format: request.PostForm.Get("editor"), Values: body.Values}
	if editor.Format != "fields" && editor.Format != "yaml" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose key/value fields or YAML to enter variables."})
		return
	}
	if editor.Format == "yaml" {
		data, err := yaml.Marshal(body.Values)
		if err != nil {
			web.problem(writer, request, err)
			return
		}
		editor.Yaml = string(data)
	}
	web.respond(writer, request, "environment-editor", editor, http.StatusOK)
}
