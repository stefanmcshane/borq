package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
)

type monitoringViewData struct {
	// Organization, Space and Name retain the selected workload scope.
	Organization, Space, Name string
	// Endpoint and Path select the workload's read-only panel route.
	Endpoint, Path string
	// LogsTitle and LogsDescription describe the output without changing the controls.
	LogsTitle, LogsDescription string
}

func monitoringView(organization, space, name, endpoint string) monitoringViewData {
	return monitoringViewData{Organization: organization, Space: space, Name: name, Endpoint: endpoint,
		Path:      endpoint + "?" + url.Values{"organization": {organization}, "space": {space}, "app": {name}}.Encode(),
		LogsTitle: "Application logs", LogsDescription: "Versioned output from all replicas and predeploy tasks, with deployment and pod events. Times shown in UTC."}
}

type applicationPanelData struct {
	// Organization, Space and Application retain the scope for panel actions.
	Organization, Space, Application string
	// Location is a read-only refresh address retaining filters.
	Location string
	// Range and Search retain the chosen history and literal log filter.
	Range, Search string
	// Follow enables periodic log refresh; pausing retains the current snapshot.
	Follow bool
	// Problem explains missing monitoring or cluster access without hiding the rest of the app.
	Problem string
	// Details reports live health, separate from operation acceptance.
	Details appapi.ApplicationDetails
	// Deployments contains only releases explicitly targeting this application.
	Deployments []appapi.Deployment
	// Logs contains bounded, escaped application output.
	Logs appapi.AppLogs
	// Rows interleaves log output and lifecycle markers chronologically.
	Rows []applicationLogRow
	// EventsNotice identifies incomplete lifecycle history without hiding metrics.
	EventsNotice string
	// History contains lifecycle events for the overview independently of output collection.
	History appapi.EventHistory
	// Endpoint retains the route used by overview history controls.
	Endpoint string
	// Cpu and Memory present aggregate metrics with consistent units.
	Cpu, Memory usageChart
}

type usageChart struct {
	// Events provides shared lifecycle markers at their actual position on the time axis.
	Events []usageEvent
	// Step is the horizontal distance between expected samples in the SVG coordinate system.
	Step float64
	// Title and Unit label the chart and its accessible data table.
	Title, Unit string
	// Latest, Average and Peak summarize actual samples, never synthetic zeroes.
	Latest, Average, Peak string
	// Maximum and Start/End label the axes.
	Maximum, Start, End string
	// Segments prevents drawing lines across missing samples.
	Segments []string
	// Samples is the accessible, readable time series.
	Samples []usageSample
}

type usageSample struct {
	// X and Y locate the measurement for pointer and keyboard inspection.
	X, Y float64
	// Time and Value expose one measurement without relying on chart colors.
	Time, Value string
}

type usageEvent struct {
	// X is the marker's position on the time axis.
	X float64
	// Time, Message and Kind label the observed transition.
	Time, Message, Kind string
	// Clock keeps hover rows short beneath their shared date and time heading.
	Clock string
}

type applicationLogRow struct {
	// Timestamp orders output and lifecycle markers on the same timeline.
	Timestamp time.Time
	// Entry or Event supplies the row's escaped content.
	Entry *appapi.AppLogEntry
	Event *appapi.ApplicationEvent
	// OperationPath selects the correct operation transport for this workload.
	OperationPath string
}

func (web handler) applicationPanel(writer http.ResponseWriter, request *http.Request) {
	selection := request.URL.Query()
	data := applicationPanelData{Organization: selection.Get("organization"), Space: selection.Get("space"), Application: selection.Get("app"), Range: selection.Get("range"), Search: selection.Get("search"), Follow: selection.Get("follow") == "on"}
	spacePanel := request.URL.Path == "/api/ui/space"
	if data.Organization == "" || data.Space == "" || (!spacePanel && data.Application == "") {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose an application and space."})
		return
	}
	if data.Range == "" {
		data.Range = "1h"
	}
	if data.Range != "30m" && data.Range != "1h" && data.Range != "12h" && data.Range != "3d" {
		web.problem(writer, request, apiProblem{status: http.StatusBadRequest, message: "Choose 30 minutes, 1 hour, 12 hours, or 3 days."})
		return
	}
	panel := selection.Get("panel")
	data.Location = "/api/ui/application?" + selection.Encode()
	data.Endpoint = "/api/ui/application"
	operationPath := "/api/ui/operations/"
	base := spacePath(data.Organization, data.Space) + "/apps/" + url.PathEscape(data.Application)
	if spacePanel {
		if panel != "metrics" && panel != "logs" {
			web.problem(writer, request, apiProblem{status: http.StatusNotFound, message: "This space panel was not found."})
			return
		}
		data.Location = "/api/ui/space?" + selection.Encode()
		data.Endpoint = "/api/ui/space"
		base = spacePath(data.Organization, data.Space)
	}
	if request.URL.Path == "/api/ui/custom-charts" {
		if panel != "metrics" && panel != "logs" && panel != "events" {
			web.problem(writer, request, apiProblem{status: http.StatusNotFound, message: "This custom addon panel was not found."})
			return
		}
		data.Location = "/api/ui/custom-charts?" + selection.Encode()
		data.Endpoint, operationPath = "/api/ui/custom-charts", "/api/ui/custom-charts/operations/"
		base = spacePath(data.Organization, data.Space) + "/custom-charts/" + url.PathEscape(data.Application)
	}
	query := url.Values{"range": {data.Range}, "search": {data.Search}}
	var err error
	switch panel {
	case "runtime", "status":
		data.Details, err = requestApi[appapi.ApplicationDetails](request.Context(), web.api, http.MethodGet, base, nil)
	case "deployments":
		data.Deployments, err = requestApi[[]appapi.Deployment](request.Context(), web.api, http.MethodGet, base+"/deployments", nil)
		if err == nil {
			for index := range data.Deployments {
				data.Deployments[index].Operation, err = web.applicationOperation(request.Context(), data.Deployments[index].Operation, data.Organization, data.Space, data.Application)
				if err != nil {
					break
				}
			}
		}
	case "metrics":
		var metrics appapi.AppMetrics
		metrics, err = requestApi[appapi.AppMetrics](request.Context(), web.api, http.MethodGet, base+"/metrics?"+query.Encode(), nil)
		data.Cpu = makeUsageChart("CPU usage", "mCPU", metrics.CPUCores, 1000, metrics)
		data.Memory = makeUsageChart("Memory usage", "MiB", metrics.MemoryBytes, 1.0/(1024*1024), metrics)
		data.EventsNotice = metrics.EventsNotice
	case "logs":
		query.Set("limit", "200")
		data.Logs, err = requestApi[appapi.AppLogs](request.Context(), web.api, http.MethodGet, base+"/logs?"+query.Encode(), nil)
		for _, entry := range data.Logs.Entries {
			data.Rows = append(data.Rows, applicationLogRow{Timestamp: entry.Timestamp, Entry: &entry})
		}
		data.Rows = append(data.Rows, eventRows(data.Logs.Events, operationPath)...)
		slices.SortStableFunc(data.Rows, func(a, b applicationLogRow) int { return a.Timestamp.Compare(b.Timestamp) })
	case "events":
		data.History, err = requestApi[appapi.EventHistory](request.Context(), web.api, http.MethodGet, base+"/events?"+query.Encode(), nil)
		data.Rows = eventRows(data.History.Events, operationPath)
	default:
		web.problem(writer, request, apiProblem{status: http.StatusNotFound, message: "This application panel was not found."})
		return
	}
	if err != nil {
		if problem, ok := err.(apiProblem); ok && (problem.status == http.StatusNotFound || problem.status == http.StatusBadRequest) {
			web.problem(writer, request, err)
			return
		}
		data.Problem = "This information is temporarily unavailable. Refresh to try again."
		if panel == "metrics" || panel == "logs" {
			data.Problem = "Management monitoring is unavailable. Check the management services and this cluster’s collector, then refresh."
		}
	}
	web.respond(writer, request, "application-"+panel, data, http.StatusOK)
}

func eventRows(events []appapi.ApplicationEvent, operationPath string) []applicationLogRow {
	rows := make([]applicationLogRow, 0, len(events))
	for _, event := range events {
		row := applicationLogRow{Timestamp: event.Timestamp, Event: &event}
		if event.Deployment != "" {
			row.OperationPath = operationPath + url.PathEscape(event.Deployment)
		}
		rows = append(rows, row)
	}
	return rows
}

func makeUsageChart(title, unit string, samples []appapi.MetricSample, scale float64, metrics appapi.AppMetrics) usageChart {
	chart := usageChart{Title: title, Unit: unit, Latest: "—", Average: "—", Peak: "—", Start: timeLabel(metrics.Start), End: timeLabel(metrics.End)}
	window := metrics.End.Sub(metrics.Start).Seconds()
	if window <= 0 {
		return chart
	}
	chart.Step = float64(metrics.StepSeconds) / window * 600
	for _, event := range metrics.Events {
		if event.Timestamp.Before(metrics.Start) || event.Timestamp.After(metrics.End) {
			continue
		}
		message := usageEventMessage(event)
		if message == "" {
			continue
		}
		chart.Events = append(chart.Events, usageEvent{X: event.Timestamp.Sub(metrics.Start).Seconds() / window * 600, Time: event.Timestamp.UTC().Format("Jan 02 15:04:05 UTC"), Clock: event.Timestamp.UTC().Format("15:04:05"), Message: message, Kind: event.Kind})
	}
	if len(samples) == 0 {
		return chart
	}
	peak, total := 0.0, 0.0
	for _, sample := range samples {
		value := sample.Value * scale
		peak = max(peak, value)
		total += value
	}
	format := func(value float64) string {
		if value > 0 && value < 0.1 {
			return strconv.FormatFloat(value, 'g', 2, 64)
		}
		return strconv.FormatFloat(value, 'f', 1, 64)
	}
	chart.Latest, chart.Average, chart.Peak, chart.Maximum = format(samples[len(samples)-1].Value*scale), format(total/float64(len(samples))), format(peak), format(peak*1.15)
	ceiling := peak * 1.15
	if ceiling == 0 {
		ceiling = 1
		chart.Maximum = "1.0"
	}
	var points []string
	for index, sample := range samples {
		if index > 0 && sample.Timestamp.Sub(samples[index-1].Timestamp) > time.Duration(metrics.StepSeconds)*time.Second*3/2 {
			chart.Segments = append(chart.Segments, chartSegment(points))
			points = nil
		}
		x := sample.Timestamp.Sub(metrics.Start).Seconds() / metrics.End.Sub(metrics.Start).Seconds() * 600
		y := 160 - sample.Value*scale/ceiling*150
		points = append(points, fmt.Sprintf("%.2f,%.2f", x, y))
		chart.Samples = append(chart.Samples, usageSample{X: x, Y: y, Time: sample.Timestamp.UTC().Format("Jan 02 15:04:05 UTC"), Value: format(sample.Value * scale)})
	}
	chart.Segments = append(chart.Segments, chartSegment(points))
	return chart
}

// Chart labels describe changes to the application. Raw event messages remain
// available in Logs; scheduler bookkeeping does not become a chart marker.
func usageEventMessage(event appapi.ApplicationEvent) string {
	switch event.Kind {
	case "deployment":
		return "Deployment started"
	case "succeeded":
		return "Deployment completed"
	case "failed":
		return "Deployment failed"
	}
	subject := "Replica"
	parts := strings.Split(event.Pod, "-")
	if len(parts) >= 4 && parts[len(parts)-2] == "pre" && len(parts[len(parts)-3]) == 12 {
		subject = "Predeploy"
	}
	// Collected Kubernetes messages start with their reason, followed by detail.
	reason, detail, _ := strings.Cut(event.Message, " · ")
	lower := strings.ToLower(detail)
	switch reason {
	case "BackoffLimitExceeded":
		return "Predeploy failed"
	case "DeadlineExceeded":
		return "Predeploy timed out"
	case "Completed", "Complete":
		return "Predeploy completed"
	case "Failed", "BackOff", "ErrImagePull", "ImagePullBackOff":
		if strings.Contains(lower, "pull image") || strings.Contains(lower, "pulling image") || strings.Contains(lower, "imagepull") || reason == "ErrImagePull" || reason == "ImagePullBackOff" {
			return "Image pull failed"
		}
		if reason == "BackOff" {
			return subject + " keeps restarting"
		}
		return subject + " failed to start"
	case "Started":
		if event.Kind == "restart" {
			return subject + " restarted"
		}
		return subject + " started"
	case "Killing":
		return subject + " stopped"
	case "OOMKilling", "OOMKilled":
		return subject + " ran out of memory"
	case "Unhealthy":
		if strings.Contains(lower, "readiness") {
			return subject + " not ready"
		}
		return subject + " failed health check"
	case "FailedScheduling":
		return subject + " waiting to start"
	case "FailedMount", "FailedAttachVolume":
		return subject + " storage unavailable"
	case "FailedToRetrieveImagePullSecret":
		return "Image pull secret unavailable"
	case "Evicted", "Preempted":
		return subject + " evicted"
	case "ScalingReplicaSet":
		if strings.Contains(lower, "scaled up") {
			return "Scaling up"
		}
		if strings.Contains(lower, "scaled down") {
			return "Scaling down"
		}
		return "Replica count changed"
	case "ProgressDeadlineExceeded":
		return "Deployment stalled"
	case "FailedCreate", "FailedPostStartHook", "FailedCreatePodSandBox":
		return subject + " failed to start"
	}
	if event.Kind == "restart" {
		return subject + " restarted"
	}
	return ""
}

func timeLabel(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("Jan 02, 15:04 UTC")
}

func chartSegment(points []string) string {
	if len(points) == 1 {
		return points[0] + " " + points[0]
	}
	return strings.Join(points, " ")
}
