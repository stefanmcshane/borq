package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/foundations/helm"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type customChartEditorData struct {
	// Name, Repository, Chart and Version identify the release and upstream artifact.
	Name, Repository, Chart, Version string
	// Values and Selector retain the YAML documents displayed by the editor.
	Values, Selector string
	// Logs controls collection and Existing makes the accepted release name immutable.
	Logs, Existing bool
}

func (web handler) loadCustomChartPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	var err error
	page.CustomCharts, err = requestApi[[]appapi.CustomChart](request.Context(), web.api, http.MethodGet, spacePath(page.Organization, page.Space.Name)+"/custom-charts", nil)
	if err != nil {
		return page, err
	}
	for index := range page.CustomCharts {
		page.CustomCharts[index].Specification.Values = nil
		page.CustomCharts[index].Specification.PodSelector = nil
	}
	page.CustomChartDraft = customChartEditorData{Values: "{}\n", Selector: "{}\n", Logs: true}
	page.CustomChartTab, page.CustomChartSubtab = selection.Get("tab"), selection.Get("subtab")
	if page.CustomChartTab == "" {
		page.CustomChartTab = "configuration"
		if selection.Get("customChart") != "" {
			page.CustomChartTab = "overview"
		}
	}
	if page.CustomChartTab != "overview" && page.CustomChartTab != "configuration" && page.CustomChartTab != "observability" {
		return page, apiProblem{status: http.StatusNotFound, message: "This custom addon page was not found."}
	}
	if page.CustomChartTab == "observability" {
		if page.CustomChartSubtab == "" {
			page.CustomChartSubtab = "metrics"
		}
		if page.CustomChartSubtab != "metrics" && page.CustomChartSubtab != "logs" {
			return page, apiProblem{status: http.StatusNotFound, message: "This custom addon view was not found."}
		}
	}
	if name := selection.Get("customChart"); name != "" {
		page.CustomChart, err = requestApi[appapi.CustomChart](request.Context(), web.api, http.MethodGet, spacePath(page.Organization, page.Space.Name)+"/custom-charts/"+url.PathEscape(name), nil)
		if err != nil {
			return page, err
		}
		specification := page.CustomChart.Specification
		page.CustomChart.Specification.Values = nil
		page.CustomChart.Specification.PodSelector = nil
		page.Title = name
		page.CustomChartDraft = customChartEditorData{Name: name, Repository: specification.Repository, Chart: specification.Chart, Version: specification.Version, Logs: specification.Logs == nil || *specification.Logs, Existing: true, Selector: "{}\n", Values: "{}\n"}
		if page.CustomChartTab == "configuration" && specification.Values != nil {
			encoded, err := yaml.Marshal(*specification.Values)
			if err != nil {
				return page, err
			}
			page.CustomChartDraft.Values = string(encoded)
		}
		if page.CustomChartTab == "configuration" && specification.PodSelector != nil {
			encoded, err := yaml.Marshal(*specification.PodSelector)
			if err != nil {
				return page, err
			}
			page.CustomChartDraft.Selector = string(encoded)
		}
	}
	if page.View == "custom-chart" && page.CustomChart.Name == "" && page.CustomChartTab != "configuration" {
		return page, apiProblem{status: http.StatusNotFound, message: "Install a custom addon before viewing its activity."}
	}
	page.Monitoring = monitoringView(page.Organization, page.Space.Name, page.CustomChart.Name, "/api/ui/custom-charts")
	page.Monitoring.LogsTitle = "Pod logs"
	page.Monitoring.LogsDescription = "Output from this addon's selected pods and containers, with installation and pod events. Times shown in UTC."
	return page, nil
}

func customChartFormInput(form url.Values) (appapi.CustomChartInput, error) {
	values, err := customChartYamlObject(form.Get("values"))
	if err != nil {
		return appapi.CustomChartInput{}, err
	}
	selectorObject, err := customChartYamlObject(form.Get("selector"))
	if err != nil {
		return appapi.CustomChartInput{}, err
	}
	selector := map[string]string{}
	for key, value := range selectorObject {
		label, ok := value.(string)
		if !ok {
			return appapi.CustomChartInput{}, apiProblem{status: http.StatusBadRequest, message: "Pod selector values must be quoted strings."}
		}
		selector[key] = label
	}
	logs := form.Get("logs") == "on"
	return appapi.CustomChartInput{Repository: strings.TrimSpace(form.Get("repository")), Chart: strings.TrimSpace(form.Get("chart")), Version: strings.TrimSpace(form.Get("version")), Values: &values, Logs: &logs, PodSelector: &selector}, nil
}

func customChartYamlObject(source string) (map[string]any, error) {
	if strings.TrimSpace(source) == "" {
		return map[string]any{}, nil
	}
	decoder := yamlreader.NewDecoder(strings.NewReader(source))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Enter a valid YAML object for overrides and selectors."}
	}
	if err := decoder.Decode(&document); err != io.EOF {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Enter exactly one YAML document in each editor."}
	}
	var result map[string]any
	if err := yaml.UnmarshalStrict([]byte(source), &result); err != nil || result == nil {
		return nil, apiProblem{status: http.StatusBadRequest, message: "Enter a YAML object with unique keys."}
	}
	return result, nil
}

func (web handler) customChartDefaults(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "The chart reference could not be read."})
		return
	}
	trimFormFields(request.PostForm)
	if strings.TrimSpace(request.PostForm.Get("chart")) == "" || strings.TrimSpace(request.PostForm.Get("version")) == "" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose a chart and version before fetching its default values."})
		return
	}
	result, err := requestApi[appapi.ChartDefaults](request.Context(), web.api, http.MethodPost, "/api/v1/charts/defaults", appapi.ChartInput{Repository: request.PostForm.Get("repository"), Name: request.PostForm.Get("chart"), Version: request.PostForm.Get("version")})
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	web.respond(writer, request, "custom-chart-defaults", result, http.StatusOK)
}

// customChartRepository is a browser helper; installation still uses the explicit API chart fields.
func (web handler) customChartRepository(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	selection := request.URL.Query()
	trimFormFields(selection)
	charts, err := helm.ListCharts(request.Context(), selection.Get("repository"))
	if err != nil {
		writer.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: err.Error()})
		return
	}
	if request.URL.Path == "/api/ui/custom-charts/versions" {
		for _, chart := range charts {
			if chart.Name == selection.Get("chart") {
				_ = json.NewEncoder(writer).Encode(chart.Versions)
				return
			}
		}
		writer.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "The selected chart was not found in this repository."})
		return
	}
	_ = json.NewEncoder(writer).Encode(charts)
}

func (web handler) customChartOperation(writer http.ResponseWriter, request *http.Request) {
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/custom-chart-operations/"+url.PathEscape(request.PathValue("operation")), nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	web.respond(writer, request, "custom-chart-operation", operation, http.StatusOK)
}
