// Package webui serves the embedded Astro site and its UI-specific JSON API.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stefanmcshane/borq/api/appapi"
)

// files includes Astro's hashed _astro directory so the binary needs no runtime files.
//
//go:embed all:site/dist
var files embed.FS

type handler struct {
	api          http.Handler
	version      string
	inputSchemas openapi3.Schemas
}

type viewResponse struct {
	// Component selects the browser view for Data.
	Component string `json:"component"`
	// Data contains the selected view's typed model.
	Data any `json:"data"`
}

// NewHandler serves the static site and UI helpers through the protected application API.
// Browser mutations reuse application validation and reject cross-origin requests.
func NewHandler(api http.Handler) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("management UI requires an API handler")
	}
	site, err := fs.Sub(files, "site/dist")
	if err != nil {
		return nil, fmt.Errorf("open management site: %w", err)
	}
	if _, err := fs.Stat(site, "index.html"); err != nil {
		return nil, fmt.Errorf("open management index; run make build-ui: %w", err)
	}
	fingerprint := sha256.New()
	if err := fs.WalkDir(site, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(site, name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(fingerprint, "%s\x00%d\x00", name, len(content))
		_, _ = fingerprint.Write(content)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("fingerprint management site: %w", err)
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load input normalization schemas: %w", err)
	}
	mux := http.NewServeMux()
	web := handler{api: api, version: fmt.Sprintf("%x", fingerprint.Sum(nil))[:16], inputSchemas: specification.Components.Schemas}
	static := http.FileServerFS(site)
	mux.HandleFunc("GET /", func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(request.URL.Path, "/")
		if name == "" {
			writer.Header().Set("Cache-Control", "no-cache")
			static.ServeHTTP(writer, request)
			return
		}
		if !strings.HasPrefix(name, "assets/") && !strings.HasPrefix(name, "_astro/") {
			http.NotFound(writer, request)
			return
		}
		info, err := fs.Stat(site, name)
		if err != nil || info.IsDir() {
			http.NotFound(writer, request)
			return
		}
		if strings.HasPrefix(name, "_astro/") {
			writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			writer.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(writer, request)
	})
	mux.HandleFunc("GET /api/ui/page", web.dashboard)
	mux.HandleFunc("POST /api/ui/actions", web.action)
	mux.HandleFunc("POST /api/ui/environment-editor", web.environmentEditor)
	mux.HandleFunc("POST /api/ui/application-editor", web.applicationEditor)
	mux.HandleFunc("GET /api/ui/postgres", web.postgresPanel)
	mux.HandleFunc("GET /api/ui/postgres/operations/{operation}", web.postgresOperation)
	mux.HandleFunc("GET /api/ui/application", web.applicationPanel)
	mux.HandleFunc("GET /api/ui/space", web.applicationPanel)
	mux.HandleFunc("GET /api/ui/custom-charts", web.applicationPanel)
	mux.HandleFunc("GET /api/ui/custom-charts/repository", web.customChartRepository)
	mux.HandleFunc("GET /api/ui/custom-charts/versions", web.customChartRepository)
	mux.HandleFunc("POST /api/ui/custom-charts/defaults", web.customChartDefaults)
	mux.HandleFunc("GET /api/ui/custom-charts/operations/{operation}", web.customChartOperation)
	mux.HandleFunc("GET /api/ui/components", web.components)
	mux.HandleFunc("GET /api/ui/management/operation", web.managementOperation)
	mux.HandleFunc("GET /api/ui/cluster", web.clusterMetrics)
	mux.HandleFunc("GET /api/ui/cluster-setup", web.clusterSetup)
	mux.HandleFunc("GET /api/ui/operations/{operation}", web.operation)
	mux.HandleFunc("POST /api/ui/operations/{operation}/cancel", web.cancelOperation)
	mux.HandleFunc("GET /api/ui/kubeconfig", web.kubeconfig)
	return http.NewCrossOriginProtection().Handler(mux), nil
}

func pageUrl(view, organization, space string, extra ...string) string {
	if view == "environment" {
		view = "secrets"
	}
	if view == "environment-group" {
		view = "secret"
	}
	if view == "spaces" || view == "clusters" || view == "cluster" {
		space = ""
	}
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

func (web handler) respond(writer http.ResponseWriter, request *http.Request, component string, data any, status int) {
	var output bytes.Buffer
	if err := json.NewEncoder(&output).Encode(viewResponse{Component: component, Data: data}); err != nil {
		slog.ErrorContext(request.Context(), "management_ui_encode_failed", "error", err)
		web.problem(writer, request, errors.New("encode management response"))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Borq-Ui-Version", web.version)
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
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Borq-Ui-Version", web.version)
	writer.WriteHeader(problem.status)
	if request.Method != http.MethodHead {
		_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: problem.message})
	}
}
