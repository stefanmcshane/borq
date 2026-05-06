package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func TestNewServer(t *testing.T) {
	is := is.New(t)

	dataDir := filepath.Join(t.TempDir(), "data")
	server, err := NewServer(Config{Port: 8080, DataDir: dataDir}, logger.Logger{})
	is.NoErr(err)
	is.Equal(server.address, ":8080")
}

func TestNewServerRejectsInvalidConfig(t *testing.T) {
	testCases := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{
			name:    "empty data dir",
			config:  Config{Port: 8080},
			wantErr: "server data dir must not be empty",
		},
		{
			name:    "non positive port",
			config:  Config{Port: 0, DataDir: t.TempDir()},
			wantErr: "server port must be greater than zero",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			is := is.New(t)
			_, err := NewServer(testCase.config, logger.Logger{})
			is.True(err != nil)
			is.Equal(err.Error(), testCase.wantErr)
		})
	}
}

func TestHandlerServesHealthAndRoot(t *testing.T) {
	is := is.New(t)

	handler := newHandler()

	rootRequest := httptest.NewRequest("GET", "/", nil)
	rootRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rootRecorder, rootRequest)
	is.Equal(rootRecorder.Code, http.StatusOK)

	healthRequest := httptest.NewRequest("GET", "/healthz", nil)
	healthRecorder := httptest.NewRecorder()
	handler.ServeHTTP(healthRecorder, healthRequest)
	is.Equal(healthRecorder.Code, http.StatusOK)
}
