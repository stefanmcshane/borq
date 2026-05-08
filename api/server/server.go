package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/reconciler"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/vault"
)

const (
	healthPath              = "/healthz"
	rootPath                = "/"
	managedNamespace        = "borq-system"
	kubernetesAuthCheckName = "kubernetes-auth"
	healthStatusSuccess     = "success"
	healthStatusFail        = "fail"
	startupHealthTimeout    = 10 * time.Second
	startupHealthInterval   = 300 * time.Millisecond
)

// NewServerInput describes the borq server runtime inputs.
type NewServerInput struct {
	Port           int
	DataDir        string
	KubeconfigPath string
	VaultAddress   string
	Log            logger.Logger
}

// Server serves the borq management API.
type Server struct {
	address    string
	dataDir    string
	health     []HealthCheck
	httpServer *http.Server
	log        logger.Logger
}

// NewServer constructs a borq server.
func NewServer(ctx context.Context, inp NewServerInput) (Server, error) {
	if err := validateNewServerInput(inp); err != nil {
		return Server{}, err
	}
	if err := os.MkdirAll(inp.DataDir, 0o755); err != nil {
		return Server{}, fmt.Errorf("create server data dir: %w", err)
	}

	kubernetesConnection, err := kubernetes.NewConnection(
		kubernetes.Config{KubeconfigPath: inp.KubeconfigPath},
		inp.Log,
	)
	if err != nil {
		return Server{}, fmt.Errorf("create kubernetes connection: %w", err)
	}

	health := []HealthCheck{
		newKubernetesAuthHealthCheck(kubernetesConnection, managedNamespace),
	}

	vaultAddr := inp.VaultAddress
	if vaultAddr == "" {
		vaultAddr = "http://vault-internal.borq-system.svc:8200"
	}
	vc, err := vault.NewClient(vault.NewClientInput{
		Address:        vaultAddr,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		return Server{}, fmt.Errorf("create vault client: %w", err)
	}

	handlers := map[string]reconciler.ComponentHandler{
		"vault": reconciler.VaultHandler{Client: vc, Log: inp.Log},
	}

	return buildServer(ctx, inp, kubernetesConnection, health, handlers, startupHealthTimeout, startupHealthInterval)
}

func buildServer(
	ctx context.Context,
	inp NewServerInput,
	conn kubernetes.Connection,
	health []HealthCheck,
	handlers map[string]reconciler.ComponentHandler,
	healthTimeout time.Duration,
	healthInterval time.Duration,
) (Server, error) {
	address := fmt.Sprintf(":%d", inp.Port)
	if err := waitForStartupHealth(ctx, health, healthTimeout, healthInterval); err != nil {
		return Server{}, err
	}

	return Server{
		address: address,
		dataDir: inp.DataDir,
		health:  health,
		log:     inp.Log,
		httpServer: &http.Server{
			Addr:              address,
			Handler:           newHandler(health, conn, inp.Log, handlers),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
}

func validateNewServerInput(inp NewServerInput) error {
	if inp.Port <= 0 {
		return errors.New("server port must be greater than zero")
	}
	if strings.TrimSpace(inp.DataDir) == "" {
		return errors.New("server data dir must not be empty")
	}

	return nil
}

// Serve starts the borq server and stops it when the context is cancelled.
func (server Server) Serve(ctx context.Context) error {
	shutdownComplete := make(chan struct{})
	defer close(shutdownComplete)

	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.httpServer.Shutdown(shutdownContext)
		case <-shutdownComplete:
		}
	}()

	server.log.Info("starting_borq_server", logger.WithFields(
		logger.String("address", server.address),
		logger.String("data_dir", server.dataDir),
	))

	err := server.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve borq server: %w", err)
	}

	return nil
}

func newHandler(health []HealthCheck, conn kubernetes.Connection, log logger.Logger, handlers map[string]reconciler.ComponentHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		report := runHealthChecks(request.Context(), health)
		writer.Header().Set("Content-Type", "application/json")
		if report.Status == healthStatusSuccess {
			writer.WriteHeader(http.StatusOK)
		} else {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(writer).Encode(report)
	})
	mux.HandleFunc(rootPath, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Path != rootPath {
			http.NotFound(writer, request)
			return
		}

		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("borq\n"))
	})
	mux.HandleFunc("/reconcile", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		componentName := strings.TrimSpace(request.URL.Query().Get("component"))
		dryRun := request.URL.Query().Get("dry-run") == "true"
		r := reconciler.NewReconciler(conn, log)

		var result reconciler.ReconcileResult
		if componentName == "" {
			if len(bytes.TrimSpace(body)) > 0 {
				http.Error(writer, "bad request: request body must be empty when component is omitted", http.StatusBadRequest)
				return
			}

			for _, name := range sortedHandlerNames(handlers) {
				handlerResult, err := reconcileHandler(request.Context(), r, log, handlers[name], nil, dryRun)
				if err != nil {
					http.Error(writer, "reconcile failed: "+err.Error(), http.StatusInternalServerError)
					return
				}
				result.Diffs = append(result.Diffs, handlerResult.Diffs...)
				result.Applied = result.Applied || handlerResult.Applied
			}
		} else {
			handler, ok := handlers[componentName]
			if !ok {
				http.Error(writer, "unknown component: "+componentName, http.StatusBadRequest)
				return
			}

			cfg := handler.DefaultConfig()
			if err := decodeReconcileConfig(body, &cfg); err != nil {
				http.Error(writer, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}

			result, err = reconcileHandler(request.Context(), r, log, handler, cfg, dryRun)
			if err != nil {
				http.Error(writer, "reconcile failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(result)
	})
	return mux
}

func decodeReconcileConfig(body []byte, cfg *any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	return json.Unmarshal(body, cfg)
}

func reconcileHandler(
	ctx context.Context,
	r reconciler.Reconciler,
	log logger.Logger,
	handler reconciler.ComponentHandler,
	cfg any,
	dryRun bool,
) (reconciler.ReconcileResult, error) {
	if cfg == nil {
		cfg = handler.DefaultConfig()
	}

	result, err := r.Reconcile(ctx, handler.Plan(cfg), handler, dryRun)
	if err == nil {
		return result, nil
	}

	var recErr *reconciler.ReconcileError
	if errors.As(err, &recErr) {
		log.Error("reconcile_error", logger.WithFields(
			append([]logger.Field{logger.String("error", err.Error())}, recErr.Fields()...)...,
		))
	} else {
		log.Error("reconcile_error", logger.WithFields(logger.String("error", err.Error())))
	}

	return result, err
}

func sortedHandlerNames(handlers map[string]reconciler.ComponentHandler) []string {
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
