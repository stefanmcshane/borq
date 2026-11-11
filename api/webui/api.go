package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/stefanmcshane/borq/api/appapi"
)

type apiProblem struct {
	status     int
	message    string
	retryAfter string
}

func (problem apiProblem) Error() string { return problem.message }

// requestApi uses the same handler as HTTP and MCP, including schema validation
// and startup guards, without a second network connection back to this server.
func requestApi[Output any](ctx context.Context, api http.Handler, method, path string, body any) (Output, error) {
	var output Output
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return output, fmt.Errorf("encode management request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(encoded))
	if err != nil {
		return output, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := apiResponse{header: http.Header{}}
	api.ServeHTTP(&response, request)
	if response.status >= http.StatusBadRequest {
		problem := appapi.Problem{Message: "The management API could not complete this request."}
		_ = json.Unmarshal(response.body.Bytes(), &problem)
		return output, apiProblem{status: response.status, message: problem.Message, retryAfter: response.header.Get("Retry-After")}
	}
	if err := json.Unmarshal(response.body.Bytes(), &output); err != nil {
		return output, fmt.Errorf("decode management response: %w", err)
	}
	return output, nil
}

// ResponseWriter mutates the buffered status and body, requiring pointer receivers.
type apiResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header exposes the headers written by the application API.
func (response *apiResponse) Header() http.Header { return response.header }

// WriteHeader retains the first status, matching net/http's non-streaming behavior.
func (response *apiResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}

// Write buffers the API body and supplies the implicit success status when needed.
func (response *apiResponse) Write(data []byte) (int, error) {
	response.WriteHeader(http.StatusOK)
	return response.body.Write(data)
}
