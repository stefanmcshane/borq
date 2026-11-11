package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
)

type managementState struct {
	Phase       string
	Ready       bool
	Attempt     int
	Message     string
	NextRetryAt time.Time
}

type managementStartup struct {
	manager        apps.Manager
	context        context.Context
	cancel         context.CancelFunc
	enabled        bool
	retryDelay     time.Duration
	attemptTimeout time.Duration
	mutex          sync.RWMutex
	state          managementState
	started        bool
	closed         bool
	workers        sync.WaitGroup
}

func (startup *managementStartup) snapshot() managementState {
	startup.mutex.RLock()
	defer startup.mutex.RUnlock()
	return startup.state
}

func (startup *managementStartup) start() {
	startup.mutex.Lock()
	defer startup.mutex.Unlock()
	if !startup.enabled || startup.started || startup.closed {
		return
	}
	if startup.manager.ManagementDestroyActive() {
		startup.state = managementState{Phase: "destroying", Message: "Management teardown is active; its operation API remains available."}
		return
	}
	startup.started = true
	startup.workers.Add(1)
	go startup.run()
}

func (startup *managementStartup) stop() {
	startup.mutex.Lock()
	startup.closed = true
	startup.cancel()
	startup.mutex.Unlock()
	startup.workers.Wait()
}

func (startup *managementStartup) run() {
	defer startup.workers.Done()
	for {
		if startup.manager.ManagementDestroyActive() {
			return
		}
		if startup.context.Err() != nil {
			return
		}
		startup.mutex.Lock()
		startup.state.Attempt++
		startup.state.Phase = "installing"
		startup.state.Message = "Installing management components."
		startup.state.NextRetryAt = time.Time{}
		startup.mutex.Unlock()
		ctx, cancel := context.WithTimeout(startup.context, startup.attemptTimeout)
		_, err := startup.manager.ReconcileManagementComponents(ctx, false)
		cancel()
		startup.mutex.Lock()
		if err == nil {
			startup.state.Ready = true
			startup.state.Phase = "ready"
			startup.state.Message = "Management components are ready."
			startup.state.NextRetryAt = time.Time{}
			startup.mutex.Unlock()
			return
		}
		if startup.context.Err() != nil {
			startup.state.Phase = "stopped"
			startup.state.Message = "Management component setup stopped."
			startup.mutex.Unlock()
			return
		}
		slog.ErrorContext(startup.context, "management_component_setup_failed", "attempt", startup.state.Attempt, "error", err)
		startup.state.Phase = "failed"
		startup.state.Message = fmt.Sprintf("Management component setup failed. Retrying in %s; inspect server logs for the component error.", startup.retryDelay)
		startup.state.NextRetryAt = time.Now().UTC().Add(startup.retryDelay)
		startup.mutex.Unlock()
		timer := time.NewTimer(startup.retryDelay)
		select {
		case <-startup.context.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (startup *managementStartup) protect(next http.Handler, settings platform.Settings) http.Handler {
	if settings.Profile == "" {
		settings.Profile = "dev"
	}
	if settings.Namespace == "" {
		settings.Namespace = platform.ManagementNamespace
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/management/destroy" || strings.HasPrefix(request.URL.Path, "/api/v1/management/destroy/operations/") {
			next.ServeHTTP(writer, request)
			return
		}
		state := startup.snapshot()
		if !startup.enabled || state.Ready {
			next.ServeHTTP(writer, request)
			return
		}
		if request.URL.Path == "/api/v1/management/components" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
			state.Message = startup.manager.ManagementSetupMessage(request.Context())
			status := "installing"
			if state.Phase == "failed" || state.Phase == "stopped" {
				status = "failed"
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(apps.ComponentInstallation{ClusterID: "management", Status: status, Message: state.Message, UpdatedAt: time.Now().UTC(), Result: platform.Result{Profile: settings.Profile, Namespace: settings.Namespace, Components: []platform.ComponentState{}}})
			return
		}
		if requiresManagement(request) {
			if request.URL.Path == "/api/v1/management/configuration" && request.Method == http.MethodPut {
				state.Message = startup.manager.ManagementSetupMessage(request.Context())
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(startup.retryDelay.Seconds()))))
			writer.Header().Set("X-Borq-Setup-Pending", "true")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "Management components are not ready. " + state.Message + " Check /api/healthz or GET /api/v1/management/components before retrying."})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func requiresManagement(request *http.Request) bool {
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/api"), "/")
	parts := strings.Split(path, "/")
	if strings.Contains(path, "/environment-groups") || strings.Contains(path, "/plans") {
		return true
	}
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return false
	}
	if strings.HasPrefix(path, "v1/management/") {
		return true
	}
	// Registration and target reconciliation requests only write durable local
	// state. Their worker waits for management readiness independently.
	if len(parts) >= 4 && parts[0] == "v1" && parts[1] == "organizations" && parts[3] == "spaces" {
		return true
	}
	return false
}
