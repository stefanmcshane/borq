package webui

import (
	"encoding/json"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type postgresPageData struct {
	// Tab and Subtab select the database section and observability view.
	Tab, Subtab string
	// Range, Search and End retain the selected observation window and log filter.
	Range, Search, End string
	// Databases lists accepted services in the selected space.
	Databases []appapi.Postgres
	// Database is selected accepted state without credentials.
	Database appapi.Postgres
	// Backups contains actual metadata and recovery eligibility.
	Backups []appapi.PostgresBackup
	// BackupProblem keeps an unavailable archive visible without hiding database health.
	BackupProblem string
	// Path scopes live monitoring to the selected database.
	Path string
	// Operation reports durable accepted changes.
	Operation appapi.Operation
}

func (web handler) loadPostgresPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	base := spacePath(page.Organization, page.Space.Name) + "/managed-addons/postgres"
	var err error
	page.Postgres.Databases, err = requestApi[[]appapi.Postgres](request.Context(), web.api, http.MethodGet, base, nil)
	if err != nil {
		return page, err
	}
	name := selection.Get("postgres")
	if name == "" {
		if id := selection.Get("postgresOperation"); id != "" {
			page.Postgres.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/postgres-operations/"+url.PathEscape(id), nil)
			if err != nil {
				return page, err
			}
			if page.Postgres.Operation.SpaceID != page.Space.ID {
				return page, apiProblem{status: http.StatusNotFound, message: "The operation belongs to another space."}
			}
		}
		return page, nil
	}
	page.Postgres.Tab = selection.Get("tab")
	if page.Postgres.Tab == "" {
		page.Postgres.Tab = "overview"
	}
	switch page.Postgres.Tab {
	case "overview", "backups", "configuration":
	case "observability":
		page.Postgres.Subtab = selection.Get("subtab")
		if page.Postgres.Subtab == "" {
			page.Postgres.Subtab = "metrics"
		}
		if page.Postgres.Subtab != "metrics" && page.Postgres.Subtab != "logs" {
			return page, apiProblem{status: http.StatusNotFound, message: "This database observability view was not found."}
		}
	default:
		return page, apiProblem{status: http.StatusNotFound, message: "This database section was not found."}
	}
	page.Postgres.Database, err = requestApi[appapi.Postgres](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name), nil)
	if err != nil {
		return page, err
	}
	page.Title = name
	page.Postgres.Range = selection.Get("range")
	if page.Postgres.Range == "" {
		page.Postgres.Range = "1h"
	}
	page.Postgres.Search, page.Postgres.End = selection.Get("search"), selection.Get("end")
	page.Postgres.Path = "/ui/postgres?" + url.Values{
		"organization": {page.Organization}, "space": {page.Space.Name}, "postgres": {name},
		"range": {page.Postgres.Range}, "search": {page.Postgres.Search}, "end": {page.Postgres.End},
	}.Encode()
	if page.Postgres.Tab == "backups" {
		page.Postgres.Backups, err = requestApi[[]appapi.PostgresBackup](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name)+"/backups", nil)
		if err != nil {
			page.Postgres.BackupProblem = "Backup catalogue is unavailable. Check shared storage and retry."
		}
	}
	if id := page.Postgres.Database.Operation; id != "" {
		page.Postgres.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/postgres-operations/"+url.PathEscape(id), nil)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

type postgresPanelData struct {
	// Location preserves the selected scope and bounded query while refreshing.
	Location string
	// Problem reports unavailability without substituting synthetic zero values.
	Problem string
	// Metrics contains SQL and volume observations.
	Metrics appapi.PostgresMetrics
	// Cpu and Memory reuse the accessible resource history charts.
	Cpu, Memory usageChart
	// Logs is bounded escaped database output.
	Logs appapi.AppLogs
}

func (web handler) postgresPanel(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	base := spacePath(query.Get("organization"), query.Get("space")) + "/managed-addons/postgres/" + url.PathEscape(query.Get("postgres"))
	history := query.Get("range")
	if history == "" {
		history = "1h"
	}
	values := url.Values{"range": {history}}
	if end := query.Get("end"); end != "" {
		values.Set("end", end)
	}
	data := postgresPanelData{Location: "/ui/postgres?" + query.Encode()}
	panel := query.Get("panel")
	var err error
	if panel == "logs" {
		values.Set("limit", "200")
		values.Set("search", query.Get("search"))
		data.Logs, err = requestApi[appapi.AppLogs](request.Context(), web.api, http.MethodGet, base+"/logs?"+values.Encode(), nil)
	} else if panel == "metrics" {
		data.Metrics, err = requestApi[appapi.PostgresMetrics](request.Context(), web.api, http.MethodGet, base+"/metrics?"+values.Encode(), nil)
		data.Cpu = makeUsageChart("CPU usage", "mCPU", data.Metrics.Usage.CPUCores, 1000, data.Metrics.Usage)
		data.Memory = makeUsageChart("Memory usage", "MiB", data.Metrics.Usage.MemoryBytes, 1.0/1048576, data.Metrics.Usage)
	} else {
		web.problem(writer, request, apiProblem{status: http.StatusNotFound, message: "This database panel was not found."})
		return
	}
	if err != nil {
		data.Problem = "This observation is unavailable. Check the database and telemetry connection, then retry."
	}
	web.render(writer, request, "postgres-"+panel, data, http.StatusOK)
}

func (web handler) executePostgresAction(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	name := form.Get("name")
	if form.Get("action") == "restore-postgres" || (form.Get("action") == "put-postgres" && form.Get("apply") != "true") {
		name = strings.Map(func(character rune) rune {
			if character >= 'A' && character <= 'Z' {
				return character + ('a' - 'A')
			}
			return character
		}, name)
	}
	base := spacePath(form.Get("organization"), form.Get("space")) + "/managed-addons/postgres/" + url.PathEscape(name)
	var database appapi.Postgres
	var err error
	switch form.Get("action") {
	case "put-postgres":
		input := appapi.PostgresInput{}
		if source := form.Get("configuration"); source != "" {
			data, err := decodeYaml(source)
			if err != nil {
				return result, err
			}
			var value any
			if err := json.Unmarshal(data, &value); err != nil {
				return result, apiProblem{status: http.StatusBadRequest, message: "Enter a valid PostgreSQL configuration object."}
			}
			specification, err := appapi.GetSwagger()
			if err != nil {
				return result, err
			}
			if err := specification.Components.Schemas["PostgresInput"].Value.VisitJSON(value); err != nil {
				message := appapi.SchemaValidationMessage(err)
				if message == "" {
					message = "Advanced database settings must match the PostgreSQL configuration schema."
				}
				return result, apiProblem{status: http.StatusBadRequest, message: message}
			}
			if err := json.Unmarshal(data, &input); err != nil {
				return result, apiProblem{status: http.StatusBadRequest, message: "Unable to read PostgreSQL configuration."}
			}
		}
		if storage := form.Get("storage"); storage != "" {
			input.Storage = &storage
		}
		if value := form.Get("instances"); value != "" {
			instances, err := strconv.Atoi(value)
			if err != nil {
				return result, apiProblem{status: http.StatusBadRequest, message: "Enter a valid instance count."}
			}
			input.Instances = &instances
		}
		if form.Get("apply") == "true" {
			base += "?apply=true"
		}
		database, err = requestApi[appapi.Postgres](request.Context(), web.api, http.MethodPut, base, input)
	case "delete-postgres":
		database, err = requestApi[appapi.Postgres](request.Context(), web.api, http.MethodDelete, base, nil)
	case "restore-postgres":
		body := appapi.PostgresRestoreInput{BackupID: form.Get("backup")}
		if value := form.Get("recoveryTarget"); value != "" {
			target, parseError := time.Parse(time.RFC3339, value)
			if parseError != nil {
				return result, apiProblem{status: http.StatusBadRequest, message: "Recovery time must use RFC3339, such as 2026-10-24T12:00:00Z."}
			}
			body.RecoveryTarget = &target
		}
		database, err = requestApi[appapi.Postgres](request.Context(), web.api, http.MethodPost, base+"/restore", body)
	}
	if err != nil {
		return result, err
	}
	result.selection.Set("view", "postgres-detail")
	result.selection.Set("postgres", database.Name)
	result.selection.Set("tab", "overview")
	result.selection.Del("subtab")
	result.notice = "Database operation accepted. Protection and recovery progress update below."
	if form.Get("action") == "put-postgres" && form.Get("apply") == "true" {
		result.selection.Set("tab", "configuration")
	}
	if form.Get("action") == "delete-postgres" {
		result.selection.Set("view", "postgres")
		result.selection.Del("postgres")
		result.selection.Del("tab")
		if database.Operation != "" {
			result.selection.Set("postgresOperation", database.Operation)
		}
		result.notice = "Database deletion accepted. Its backup archive will be preserved."
	}
	return result, nil
}

func (web handler) postgresOperation(writer http.ResponseWriter, request *http.Request) {
	operation, err := requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/postgres-operations/"+url.PathEscape(request.PathValue("operation")), nil)
	if err != nil {
		web.problem(writer, request, err)
		return
	}
	if operationComplete(operation.Status) {
		writer.Header().Set("HX-Trigger", "deployment-complete")
	}
	web.render(writer, request, "postgres-operation", operation, http.StatusOK)
}
