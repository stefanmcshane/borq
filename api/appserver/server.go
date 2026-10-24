// Package appserver manages components and applications on existing Kubernetes clusters.
package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/api/appmcp"
	"github.com/stefanmcshane/borq/api/webui"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewInput describes the application server's local listener and persistent state.
type NewInput struct {
	// PrometheusQueryUrl and LokiQueryUrl override private management query endpoints.
	PrometheusQueryUrl, LokiQueryUrl string
	// ListenHost defaults to loopback; use an explicit interface when serving remote clients.
	ListenHost string
	// Port is the HTTP port on the server host.
	Port int
	// TelemetryPort serves only authenticated ingestion when infrastructure management is enabled.
	TelemetryPort int
	// DataDir stores the SQLite database and must survive server restarts.
	DataDir string
	// RolloutTimeout bounds server-side rollout work; zero uses ten minutes.
	RolloutTimeout time.Duration
	// InfrastructureEnabled installs and manages deployment target components.
	InfrastructureEnabled bool
	// ManagementEnabled installs supporting components after HTTP listeners bind.
	ManagementEnabled bool
	// ManagementKubeconfigPath selects the management cluster; empty uses the pod service account.
	ManagementKubeconfigPath string
	// PlatformSettings selects component profiles and target-reachable service endpoints.
	PlatformSettings platform.Settings
}

// Server owns the HTTP listener and the application manager's worker lifetime.
type Server struct {
	// net/http requires a shared pointer to coordinate serving and shutdown.
	http *http.Server
	// A separate listener exposes ingestion without exposing the administrative API.
	telemetry *http.Server
	manager   apps.Manager
	// Shared startup state coordinates readiness, request gating and cancellation.
	management *managementStartup
	// The SDK owns shared processor state and exposes shutdown through its pointer API.
	tracing *sdktrace.TracerProvider
}

// New opens durable state without requiring any registered Kubernetes cluster.
func New(ctx context.Context, input NewInput) (Server, error) {
	if input.Port < 1 || input.Port > 65535 {
		return Server{}, errors.New("server port must be between 1 and 65535")
	}
	if input.DataDir == "" {
		return Server{}, errors.New("server data directory is required")
	}
	if input.RolloutTimeout < 0 {
		return Server{}, errors.New("rollout timeout cannot be negative")
	}
	if input.ListenHost == "" {
		input.ListenHost = "127.0.0.1"
	}
	if input.ManagementEnabled && !input.InfrastructureEnabled {
		return Server{}, errors.New("management setup requires infrastructure management to be enabled")
	}
	if input.InfrastructureEnabled {
		if input.TelemetryPort == 0 {
			input.TelemetryPort = 7834
		}
		if input.TelemetryPort < 1 || input.TelemetryPort > 65535 || input.TelemetryPort == input.Port {
			return Server{}, errors.New("telemetry port must be between 1 and 65535 and differ from the API port")
		}
	}
	manager, err := apps.NewManager(ctx, apps.NewInput{
		PrometheusQueryUrl: input.PrometheusQueryUrl, LokiQueryUrl: input.LokiQueryUrl,
		DatabasePath: filepath.Join(input.DataDir, "borq.sqlite"), RolloutTimeout: input.RolloutTimeout,
		InfrastructureEnabled: input.InfrastructureEnabled, ManagementKubeconfigPath: input.ManagementKubeconfigPath, PlatformSettings: input.PlatformSettings,
		ManagementEnabled: input.ManagementEnabled,
	})
	if err != nil {
		return Server{}, fmt.Errorf("open application state: %w", err)
	}
	handler, err := appapi.NewHandler(manager)
	if err != nil {
		_ = manager.Close()
		return Server{}, err
	}
	tracing := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "borq-apps"))))
	managementContext, cancelManagement := context.WithCancel(ctx)
	// A pointer keeps worker state shared by value copies of Server and HTTP handlers.
	management := &managementStartup{manager: manager, context: managementContext, cancel: cancelManagement,
		enabled: input.ManagementEnabled, retryDelay: 15 * time.Second, attemptTimeout: 30 * time.Minute,
		state: managementState{Phase: "pending", Message: "Management component setup has not started."},
	}
	if input.PlatformSettings.Timeout > 0 {
		management.attemptTimeout = 6 * input.PlatformSettings.Timeout
	}
	if !input.ManagementEnabled {
		management.state = managementState{Phase: "disabled", Ready: true, Message: "Management component setup is disabled."}
	}
	mux := http.NewServeMux()
	api := management.protect(handler, input.PlatformSettings)
	web, err := webui.NewHandler(api)
	if err != nil {
		cancelManagement()
		_ = manager.Close()
		_ = tracing.Shutdown(ctx)
		return Server{}, err
	}
	health := func(includeManagement bool) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				writer.Header().Set("Allow", "GET, HEAD")
				http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			status, description := "success", "Persistent application state is open."
			checks := []map[string]any{}
			if err := manager.CheckHealth(request.Context()); err != nil {
				status, description = "fail", "Persistent application state is unavailable."
			}
			checks = append(checks, map[string]any{"name": "sqlite", "status": status, "description": description})
			if includeManagement && input.ManagementEnabled {
				state := management.snapshot()
				checkStatus := "success"
				if !state.Ready {
					checkStatus = "fail"
					status = "fail"
				}
				checks = append(checks, map[string]any{"name": "management", "status": checkStatus, "description": state.Message, "phase": state.Phase, "attempt": state.Attempt, "nextRetryAt": state.NextRetryAt})
			}
			if status != "success" {
				writer.WriteHeader(http.StatusServiceUnavailable)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"status": status, "checks": checks})
		}
	}
	mux.HandleFunc("/api/healthz", health(true))
	mux.HandleFunc("/api/readyz", health(true))
	mux.HandleFunc("/api/livez", health(false))
	mux.Handle("/{$}", web)
	mux.Handle("/assets/", web)
	mux.Handle("/ui/", web)
	mcp, err := appmcp.NewHandler(api)
	if err != nil {
		cancelManagement()
		_ = manager.Close()
		_ = tracing.Shutdown(ctx)
		return Server{}, err
	}
	mux.Handle("/api/mcp", mcp)
	mux.Handle("/api/", api)
	server := Server{manager: manager, management: management, tracing: tracing, http: &http.Server{
		Addr: net.JoinHostPort(input.ListenHost, strconv.Itoa(input.Port)), Handler: otelhttp.NewHandler(mux, "borq.apps", otelhttp.WithTracerProvider(tracing)),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}}
	if input.InfrastructureEnabled {
		gateway, err := newTelemetryGateway(manager, input.ManagementKubeconfigPath, input.PlatformSettings.Namespace)
		if err != nil {
			cancelManagement()
			_ = manager.Close()
			_ = tracing.Shutdown(ctx)
			return Server{}, err
		}
		mux.Handle("/api/v1/ingest/", gateway)
		server.telemetry = &http.Server{
			Addr: net.JoinHostPort(input.ListenHost, strconv.Itoa(input.TelemetryPort)), Handler: gateway,
			ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		}
	}
	return server, nil
}

// Serve runs until cancellation, then drains requests before stopping workers.
func (server Server) Serve(ctx context.Context) error {
	defer server.Close()
	listener, err := net.Listen("tcp", server.http.Addr)
	if err != nil {
		return err
	}
	var telemetryListener net.Listener
	if server.telemetry != nil {
		telemetryListener, err = net.Listen("tcp", server.telemetry.Addr)
		if err != nil {
			_ = listener.Close()
			return err
		}
	}
	failures := make(chan error, 2)
	go func() { failures <- server.http.Serve(listener) }()
	if server.telemetry != nil {
		go func() { failures <- server.telemetry.Serve(telemetryListener) }()
	}
	server.management.start()
	var serveError error
	select {
	case err := <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			serveError = err
		}
	case <-ctx.Done():
	}
	server.management.cancel()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, listener := range []*http.Server{server.http, server.telemetry} {
		if listener == nil {
			continue
		}
		if err := listener.Shutdown(shutdownContext); err != nil {
			_ = listener.Close()
			serveError = errors.Join(serveError, fmt.Errorf("drain Borq listener: %w", err))
		}
	}
	return serveError
}

// Close stops a server that will not be served, releasing its workers and database.
func (server Server) Close() error {
	server.management.cancel()
	httpError := server.http.Close()
	if server.telemetry != nil {
		httpError = errors.Join(httpError, server.telemetry.Close())
	}
	server.management.stop()
	managerError := server.manager.Close()
	return errors.Join(httpError, managerError, server.shutdownTracing())
}

func (server Server) shutdownTracing() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.tracing.Shutdown(ctx)
}
