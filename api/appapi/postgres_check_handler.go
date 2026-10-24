package appapi

import (
	"encoding/json"
	"github.com/stefanmcshane/borq/business/server/apps"
	"io"
	"net/http"
	"strings"
)

func (server handler) postgresRestoreCheck(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		writeProblem(writer, http.StatusUnauthorized, "a projected restore-check token is required")
		return
	}
	var input apps.PostgresCheckInput
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.JobUID == "" {
		writeProblem(writer, http.StatusBadRequest, "a restore-check jobUid is required")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeProblem(writer, http.StatusBadRequest, "one restore-check request is required")
		return
	}
	input.Token = token
	result, err := server.manager.RestoreCheckPostgres(request.Context(), request.PathValue("organization"), request.PathValue("space"), request.PathValue("postgres"), input)
	respond[Operation](writer, http.StatusAccepted, result, err)
}

func (server handler) postgresRestoreCheckOperation(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		writeProblem(writer, http.StatusUnauthorized, "a projected restore-check token is required")
		return
	}
	input := apps.PostgresCheckInput{Token: token, JobUID: request.URL.Query().Get("jobUid")}
	if input.JobUID == "" {
		writeProblem(writer, http.StatusBadRequest, "a restore-check jobUid is required")
		return
	}
	result, err := server.manager.GetPostgresCheckOperation(request.Context(), request.PathValue("organization"), request.PathValue("space"), request.PathValue("postgres"), request.PathValue("operation"), input)
	respond[Operation](writer, http.StatusOK, result, err)
}
