package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/managementreconcile"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
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

	handlers := map[string]managementreconcile.ComponentHandler{
		"vault": managementreconcile.VaultHandler{},
	}

	return buildServer(ctx, inp, kubernetesConnection, health, handlers, startupHealthTimeout, startupHealthInterval)
}

func buildServer(
	ctx context.Context,
	inp NewServerInput,
	conn kubernetes.Connection,
	health []HealthCheck,
	handlers map[string]managementreconcile.ComponentHandler,
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

func newHandler(health []HealthCheck, conn kubernetes.Connection, log logger.Logger, handlers map[string]managementreconcile.ComponentHandler) http.Handler {
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

		componentName := request.URL.Query().Get("component")
		handler, ok := handlers[componentName]
		if !ok {
			http.Error(writer, "unknown component: "+componentName, http.StatusBadRequest)
			return
		}

		cfg := handler.DefaultConfig()
		if err := json.NewDecoder(request.Body).Decode(&cfg); err != nil {
			http.Error(writer, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		dryRun := request.URL.Query().Get("dry-run") == "true"
		reconciler := managementreconcile.NewReconciler(conn, log)

		plan := handler.Plan(cfg)
		actual, err := reconciler.GetActual(request.Context(), plan)
		if err != nil {
			log.Error("reconcile_get_actual_error", logger.WithFields(logger.String("error", err.Error())))
			http.Error(writer, "reconcile audit failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		_, result, err := reconciler.Reconcile(request.Context(), actual, plan, handler, dryRun)
		if err != nil {
			log.Error("reconcile_error", logger.WithFields(logger.String("error", err.Error())))
			http.Error(writer, "reconcile failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(result)
	})
	return mux
}
