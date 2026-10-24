package appserver

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// ServeMux uses shared pointer ownership to register these handlers with the API.
func attachTelemetryGateway(mux *http.ServeMux, manager apps.Manager, namespace string) {
	if namespace == "" {
		namespace = "borq"
	}
	for _, backend := range []struct{ route, path, host string }{
		{"loki/api/v1/push", "/loki/api/v1/push", "http://loki." + namespace + ".svc:3100"},
		{"api/v1/write", "/api/v1/write", "http://prometheus-server." + namespace + ".svc:80"},
	} {
		address, _ := url.Parse(backend.host)
		proxy := httputil.NewSingleHostReverseProxy(address)
		// Telemetry stays inside the management cluster and never follows an outbound HTTP proxy.
		proxy.Transport = &http.Transport{Proxy: nil}
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
			http.Error(writer, "telemetry backend unavailable", http.StatusBadGateway)
		}
		mux.HandleFunc("POST /api/v1/ingest/{cluster}/"+backend.route, func(writer http.ResponseWriter, request *http.Request) {
			id := request.PathValue("cluster")
			if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				http.Error(writer, "invalid cluster", http.StatusBadRequest)
				return
			}
			authorization := request.Header.Get("Authorization")
			if !strings.HasPrefix(authorization, "Bearer ") {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := manager.AuthorizeTelemetry(request.Context(), id, strings.TrimPrefix(authorization, "Bearer "))
			if err != nil {
				http.Error(writer, "telemetry credentials unavailable", http.StatusServiceUnavailable)
				return
			}
			if !allowed {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
			request.URL.Path, request.URL.RawPath = backend.path, ""
			request.Header.Del("Authorization")
			request.Header.Set("X-Scope-OrgID", id)
			request.Header.Set("X-Borq-Cluster", id)
			request.Host = address.Host
			proxy.ServeHTTP(writer, request)
		})
	}
}
