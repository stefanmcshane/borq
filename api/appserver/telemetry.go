package appserver

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"k8s.io/client-go/rest"
)

// newTelemetryGateway uses the selected management connection for both in-cluster
// and local development, without requiring local access to service DNS or pod IPs.
func newTelemetryGateway(manager apps.Manager, kubeconfigPath, namespace string) (http.Handler, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, err
	}
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires a configuration pointer to build its authenticated transport.
	transport, err := rest.TransportFor(&configuration)
	if err != nil {
		return nil, fmt.Errorf("configure telemetry store transport: %w", err)
	}
	mux := http.NewServeMux()
	if namespace == "" {
		namespace = "borq"
	}
	for _, backend := range []struct{ route, path, host string }{
		{"loki/api/v1/push", "/loki/api/v1/push", strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:loki:3100/proxy"},
		{"api/v1/write", "/api/v1/write", strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:prometheus-server:80/proxy"},
	} {
		address, _ := url.Parse(backend.host)
		proxy := httputil.NewSingleHostReverseProxy(address)
		// The upstream connection authenticates to Kubernetes, separately from the target bearer key.
		proxy.Transport = transport
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
			slog.WarnContext(request.Context(), "telemetry_store_unavailable", "store", backend.route, "error", err)
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
	return mux, nil
}
