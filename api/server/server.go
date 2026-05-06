package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	healthPath = "/healthz"
	rootPath   = "/"
)

// Config describes the borq server runtime configuration.
type Config struct {
	Port    int
	DataDir string
}

// Server serves the borq management API.
type Server struct {
	address    string
	dataDir    string
	httpServer *http.Server
	log        logger.Logger
}

// NewServer constructs a borq server.
func NewServer(config Config, log logger.Logger) (Server, error) {
	if config.Port <= 0 {
		return Server{}, errors.New("server port must be greater than zero")
	}
	if strings.TrimSpace(config.DataDir) == "" {
		return Server{}, errors.New("server data dir must not be empty")
	}
	if err := os.MkdirAll(config.DataDir, 0o755); err != nil {
		return Server{}, fmt.Errorf("create server data dir: %w", err)
	}

	address := fmt.Sprintf(":%d", config.Port)

	return Server{
		address: address,
		dataDir: config.DataDir,
		log:     log,
		httpServer: &http.Server{
			Addr:              address,
			Handler:           newHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
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

	server.log.Info(
		"starting borq server",
		logger.String("address", server.address),
		logger.String("data_dir", server.dataDir),
	)

	err := server.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve borq server: %w", err)
	}

	return nil
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
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

	return mux
}
