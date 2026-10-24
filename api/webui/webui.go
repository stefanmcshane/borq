// Package webui serves the Borq management UI through the validated application API.
package webui

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

// files is embedded at package scope so the management page and htmx work without runtime assets or a CDN.
//
//go:embed *.html assets/*
var files embed.FS

type handler struct {
	api http.Handler
	// html/template exposes its concurrency-safe parsed template set through a pointer.
	page *template.Template
}

// NewHandler connects server-rendered management pages to the protected API handler.
// All browser mutations reuse API validation and are protected against cross-origin requests.
func NewHandler(api http.Handler) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("management UI requires an API handler")
	}
	page, err := template.New("management").Funcs(template.FuncMap{
		"pageUrl":  pageUrl,
		"yaml":     func(value any) (string, error) { data, err := yaml.Marshal(value); return string(data), err },
		"terminal": operationComplete,
		"variant": func(value any) string {
			switch fmt.Sprint(value) {
			case "failed", "conflict":
				return "destructive"
			case "ready", "succeeded", "deployed":
				return "secondary"
			default:
				return "outline"
			}
		},
	}).ParseFS(files, "*.html")
	if err != nil {
		return nil, fmt.Errorf("parse management page: %w", err)
	}
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		return nil, fmt.Errorf("open management assets: %w", err)
	}
	mux := http.NewServeMux()
	web := handler{api: api, page: page}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", web.dashboard)
	mux.HandleFunc("POST /ui/actions", web.action)
	mux.HandleFunc("POST /ui/environment-editor", web.environmentEditor)
	mux.HandleFunc("POST /ui/application-editor", web.applicationEditor)
	mux.HandleFunc("GET /ui/components", web.components)
	mux.HandleFunc("GET /ui/management/components", web.components)
	mux.HandleFunc("POST /ui/management/actions", web.action)
	mux.HandleFunc("GET /ui/operations/{operation}", web.operation)
	mux.HandleFunc("GET /ui/kubeconfig", web.kubeconfig)
	return http.NewCrossOriginProtection().Handler(mux), nil
}

func pageUrl(view, organization, space string, extra ...string) string {
	values := url.Values{"view": {view}}
	if organization != "" {
		values.Set("organization", organization)
	}
	if space != "" {
		values.Set("space", space)
	}
	for index := 0; index+1 < len(extra); index += 2 {
		values.Set(extra[index], extra[index+1])
	}
	return "/?" + values.Encode()
}

func operationComplete(status appapi.OperationStatus) bool {
	return status == appapi.Succeeded || status == appapi.Failed
}

func (web handler) render(writer http.ResponseWriter, request *http.Request, name string, data any, status int) {
	var output bytes.Buffer
	if err := web.page.ExecuteTemplate(&output, name, data); err != nil {
		slog.ErrorContext(request.Context(), "management_ui_render_failed", "error", err)
		http.Error(writer, "Unable to display the management page.", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Vary", "HX-Request")
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(output.Bytes())
	}
}

func (web handler) problem(writer http.ResponseWriter, request *http.Request, err error) {
	problem := apiProblem{status: http.StatusInternalServerError, message: "Unable to complete the request. Please retry or check the server logs."}
	if !errors.As(err, &problem) {
		slog.ErrorContext(request.Context(), "management_ui_request_failed", "error", err)
	}
	if problem.retryAfter != "" {
		writer.Header().Set("Retry-After", problem.retryAfter)
	}
	name := "error-page"
	if request.Header.Get("HX-Request") == "true" {
		name = "notice-error"
		writer.Header().Set("HX-Retarget", "#notice")
		writer.Header().Set("HX-Reswap", "innerHTML")
	}
	web.render(writer, request, name, problem.message, problem.status)
}
