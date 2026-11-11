// Package appmcp derives MCP tools from the embedded application OpenAPI contract.
package appmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanmcshane/borq/api/appapi"
)

// NewHandler serves Streamable HTTP tools through the supplied application API,
// including its request validation and permission checks. The caller must
// authenticate requests and supply an authorized application API handler.
func NewHandler(api http.Handler) (http.Handler, error) {
	specification, err := appapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load MCP API contract: %w", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "borq", Version: specification.Info.Version}, &mcp.ServerOptions{
		Instructions: specification.Info.Description,
	})
	for _, path := range specification.Paths.InMatchingOrder() {
		item := specification.Paths.Value(path)
		for method, operation := range item.Operations() {
			permissionDescription, err := appapi.OperationPermissionDescription(specification, operation)
			if err != nil {
				return nil, err
			}
			input, err := operationInput(item.Parameters, operation)
			if err != nil {
				return nil, fmt.Errorf("build MCP tool %s: %w", operation.OperationID, err)
			}
			inputSchema, err := toolSchema(input, specification.Components.Schemas)
			if err != nil {
				return nil, fmt.Errorf("build MCP input schema %s: %w", operation.OperationID, err)
			}
			output, err := operationOutput(operation)
			if err != nil {
				return nil, fmt.Errorf("build MCP output %s: %w", operation.OperationID, err)
			}
			outputSchema, err := toolSchema(output, specification.Components.Schemas)
			if err != nil {
				return nil, fmt.Errorf("build MCP output schema %s: %w", operation.OperationID, err)
			}
			// The SDK uses a pointer to distinguish an explicit non-destructive
			// hint from the protocol's destructive default.
			destructive := method != http.MethodGet
			if hint, ok := operation.Extensions["x-mcp-destructive"].(bool); ok {
				destructive = hint
			}
			idempotent, _ := operation.Extensions["x-mcp-idempotent"].(bool)
			readOnly := method == http.MethodGet
			if hint, ok := operation.Extensions["x-mcp-read-only"].(bool); ok {
				readOnly = hint
			}
			server.AddTool(&mcp.Tool{
				Name: operation.OperationID, Description: strings.TrimSpace(operation.Summary + "\n\n" + operation.Description + "\n\n" + permissionDescription),
				InputSchema: inputSchema, OutputSchema: outputSchema,
				Annotations: &mcp.ToolAnnotations{Title: operation.Summary, ReadOnlyHint: readOnly, DestructiveHint: &destructive, IdempotentHint: method == http.MethodGet || idempotent},
			}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				arguments := request.Params.Arguments
				if len(arguments) == 0 {
					arguments = json.RawMessage(`{}`)
				}
				var values any
				if err := json.Unmarshal(arguments, &values); err != nil {
					return toolError("Invalid tool arguments."), nil
				}
				if err := input.VisitJSON(values); err != nil {
					message := appapi.SchemaValidationMessage(err)
					if message == "" {
						message = "Tool arguments do not match the input schema."
					}
					return toolError(message), nil
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(arguments, &raw); err != nil {
					return toolError("Invalid tool arguments."), nil
				}
				return callApi(ctx, api, method, path, values.(map[string]any), raw["body"])
			})
		}
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
		// Allow a full 1 MiB API body plus the JSON-RPC envelope and parameters.
		MaxRequestBodyBytes: 2 << 20,
	})
	return http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		transport.ServeHTTP(privateResponse{writer}, request)
	})), nil
}

func callApi(ctx context.Context, api http.Handler, method, path string, arguments map[string]any, body json.RawMessage) (*mcp.CallToolResult, error) {
	if parameters, ok := arguments["path"].(map[string]any); ok {
		for name, value := range parameters {
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(fmt.Sprint(value)))
		}
	}
	query := url.Values{}
	if parameters, ok := arguments["query"].(map[string]any); ok {
		for name, value := range parameters {
			query.Set(name, fmt.Sprint(value))
		}
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return toolError("Unable to construct API request."), nil
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response := apiResponse{header: http.Header{}}
	api.ServeHTTP(&response, request)
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	var result any
	if err := json.Unmarshal(response.body.Bytes(), &result); err != nil {
		result = response.body.String()
	}
	return &mcp.CallToolResult{
		IsError:           status >= http.StatusBadRequest,
		Content:           []mcp.Content{&mcp.TextContent{Text: response.body.String()}},
		StructuredContent: map[string]any{"status": status, "body": result},
	}, nil
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}

// apiResponse buffers the non-streaming API response. Pointer receivers are
// required because http.ResponseWriter writes mutate the status and body.
type apiResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header retains API response headers for the duration of the buffered call.
func (response *apiResponse) Header() http.Header { return response.header }

// WriteHeader preserves the first status chosen by the API.
func (response *apiResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}

// Write buffers the API payload and supplies the default HTTP success status.
func (response *apiResponse) Write(data []byte) (int, error) {
	response.WriteHeader(http.StatusOK)
	return response.body.Write(data)
}

// privateResponse preserves no-store after the SDK sets its transport headers.
type privateResponse struct{ http.ResponseWriter }

// WriteHeader prevents caches from storing credentials returned by tools.
func (response privateResponse) WriteHeader(status int) {
	response.Header().Set("Cache-Control", "no-store")
	response.ResponseWriter.WriteHeader(status)
}

// Write also protects responses that rely on net/http's implicit status code.
func (response privateResponse) Write(data []byte) (int, error) {
	response.Header().Set("Cache-Control", "no-store")
	return response.ResponseWriter.Write(data)
}

// Unwrap preserves the SDK's access to net/http response controls.
func (response privateResponse) Unwrap() http.ResponseWriter { return response.ResponseWriter }
