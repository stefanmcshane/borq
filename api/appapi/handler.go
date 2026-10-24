package appapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stefanmcshane/borq/business/server/apps"
	strictjson "sigs.k8s.io/json"
)

// handler translates the generated HTTP contract into application workflows.
// The schema pointer is required because kin-openapi exposes resolved schemas as pointers.
type handler struct {
	manager apps.Manager
	schemas openapi3.Schemas
}

// NewHandler validates the embedded contract and serves its generated API routes.
// The caller must restrict access to trusted administrators in this initial version.
func NewHandler(manager apps.Manager) (http.Handler, error) {
	specification, err := GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load application API specification: %w", err)
	}
	if err := specification.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validate application API specification: %w", err)
	}
	implementation := handler{manager: manager, schemas: specification.Components.Schemas}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/organizations/{organization}/spaces/{space}/managed-addons/postgres/{postgres}/restore-check", implementation.postgresRestoreCheck)
	mux.HandleFunc("GET /api/v1/organizations/{organization}/spaces/{space}/managed-addons/postgres/{postgres}/restore-check/{operation}", implementation.postgresRestoreCheckOperation)
	mux.HandleFunc("GET /api/openapi.yaml", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/yaml")
		_, _ = writer.Write(openapiDocument)
	})
	documentation, err := fs.Sub(documentationFiles, "api-docs")
	if err != nil {
		return nil, fmt.Errorf("open API documentation: %w", err)
	}
	mux.Handle("GET /api/docs/", http.StripPrefix("/api/docs/", http.FileServerFS(documentation)))
	routes := HandlerWithOptions(implementation, StdHTTPServerOptions{
		BaseRouter:  mux,
		Middlewares: []MiddlewareFunc{implementation.validatePath},
		ErrorHandlerFunc: func(writer http.ResponseWriter, request *http.Request, err error) {
			writeProblem(writer, http.StatusBadRequest, "invalid request path")
		},
	})
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Keep existing HTTP clients on the same saved installations after the public rename.
		parts := strings.Split(request.URL.Path, "/")
		if len(parts) >= 8 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "organizations" && parts[5] == "spaces" && parts[7] == "addons" {
			parts[7] = "custom-charts"
		} else if len(parts) == 5 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "addon-operations" {
			parts[3] = "custom-chart-operations"
		}
		if path := strings.Join(parts, "/"); path != request.URL.Path {
			request = request.Clone(request.Context())
			request.URL.Path, request.URL.RawPath = path, ""
		}
		// Set this before generated parameter binding and path validation so
		// credential and metric errors are never cached either.
		if strings.HasPrefix(request.URL.Path, "/api/v1/organizations/") && (strings.HasSuffix(request.URL.Path, "/kubeconfig") || strings.HasSuffix(request.URL.Path, "/metrics")) {
			writer.Header().Set("Cache-Control", "no-store")
		}
		writer.Header().Set("Cache-Control", "no-store")
		routes.ServeHTTP(writer, request)
	}), nil
}

func (server handler) validatePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identifier := server.schemas["Organization"].Value.Properties["name"].Value
		for _, parameter := range []string{"organization", "cluster", "space", "plan", "operation"} {
			value := request.PathValue(parameter)
			if value == "" {
				continue
			}
			if err := identifier.VisitJSON(value); err != nil {
				writeProblem(writer, http.StatusBadRequest, "invalid request path")
				return
			}
		}
		for parameter, schema := range map[string]*openapi3.Schema{
			"postgres":    server.schemas["Postgres"].Value.Properties["name"].Value,
			"customChart": server.schemas["CustomChart"].Value.Properties["name"].Value,
			"application": server.schemas["App"].Value.Properties["name"].Value,
			"group":       server.schemas["CloneEnvironmentGroupInput"].Value.Properties["sourceName"].Value,
			"version":     server.schemas["CloneEnvironmentGroupInput"].Value.Properties["sourceVersion"].Value,
		} {
			if value := request.PathValue(parameter); value != "" {
				if err := schema.VisitJSON(value); err != nil {
					message := "invalid request path"
					if guidance := SchemaValidationMessage(err); guidance != "" {
						message = guidance
					}
					writeProblem(writer, http.StatusBadRequest, message)
					return
				}
			}
		}
		next.ServeHTTP(writer, request)
	})
}

// PutOrganization creates an organization without allocating spaces.
func (server handler) PutOrganization(writer http.ResponseWriter, request *http.Request, organization string) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.CreateOrganization(request.Context(), organization)
	respond[Organization](writer, http.StatusOK, result, err)
}

// ListOrganizations returns all organization groupings in stable name order.
func (server handler) ListOrganizations(writer http.ResponseWriter, request *http.Request) {
	result, err := server.manager.ListOrganizations(request.Context())
	respond[[]Organization](writer, http.StatusOK, result, err)
}

// ListClusters returns the clusters owned by the requested organization.
func (server handler) ListClusters(writer http.ResponseWriter, request *http.Request, organization string) {
	result, err := server.manager.ListClusters(request.Context(), organization)
	respond[[]Cluster](writer, http.StatusOK, result, err)
}

// ListSpaces returns the independent deployment spaces in one organization.
func (server handler) ListSpaces(writer http.ResponseWriter, request *http.Request, organization string) {
	result, err := server.manager.ListSpaces(request.Context(), organization)
	respond[[]Space](writer, http.StatusOK, result, err)
}

// LinkCluster allocates a cluster name and its initial space together.
func (server handler) LinkCluster(writer http.ResponseWriter, request *http.Request, organization string) {
	var body LinkClusterInput
	if !server.decodeBody(writer, request, "LinkClusterInput", &body) {
		return
	}
	input, err := convert[apps.ClusterInput](body.Connection)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode cluster configuration")
		return
	}
	input.Organization, input.Name = organization, body.Name
	result, err := server.manager.LinkCluster(request.Context(), input)
	respond[Cluster](writer, http.StatusOK, result, err)
}

// PutCluster registers a connection using credentials available to this server.
func (server handler) PutCluster(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	var body ClusterInput
	if !server.decodeBody(writer, request, "ClusterInput", &body) {
		return
	}
	input, err := convert[apps.ClusterInput](body)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode cluster configuration")
		return
	}
	input.Organization, input.Name = organization, cluster
	result, err := server.manager.RegisterCluster(request.Context(), input)
	respond[Cluster](writer, http.StatusOK, result, err)
}

// PutSpace creates a space bound to a registered cluster.
func (server handler) PutSpace(writer http.ResponseWriter, request *http.Request, organization, space string) {
	var body SpaceInput
	if !server.decodeBody(writer, request, "SpaceInput", &body) {
		return
	}
	input, err := convert[apps.SpaceInput](body)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode space configuration")
		return
	}
	input.Organization, input.Name = organization, space
	result, err := server.manager.CreateSpace(request.Context(), input)
	respond[Space](writer, http.StatusOK, result, err)
}

// GetSpace reads the desired state accepted for one deployment target.
func (server handler) GetSpace(writer http.ResponseWriter, request *http.Request, organization, space string) {
	result, err := server.manager.GetSpace(request.Context(), organization, space)
	respond[Space](writer, http.StatusOK, result, err)
}

// ListApps reads accepted app configuration and its stored manifests from the same space snapshot.
func (server handler) ListApps(writer http.ResponseWriter, request *http.Request, organization, space string) {
	result, err := server.manager.ListApps(request.Context(), organization, space)
	respond[ApplicationState](writer, http.StatusOK, result, err)
}

// PostPlan validates a partial update and freezes its expanded artifacts.
func (server handler) PostPlan(writer http.ResponseWriter, request *http.Request, organization, space string) {
	var body Patch
	if !server.decodeBody(writer, request, "Patch", &body) {
		return
	}
	patch, err := convert[apps.Patch](body)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode application configuration")
		return
	}
	result, err := server.manager.Plan(request.Context(), apps.PlanInput{
		Organization: organization,
		Space:        space,
		Patch:        patch,
	})
	respond[Plan](writer, http.StatusCreated, result, err)
}

// ExpandApps expands a target-independent application update into its complete
// configuration without requiring cluster access or stored state.
func (server handler) ExpandApps(writer http.ResponseWriter, request *http.Request) {
	var body Patch
	if !server.decodeBody(writer, request, "Patch", &body) {
		return
	}
	patch, err := convert[apps.Patch](body)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode application configuration")
		return
	}
	configuration, err := apps.Merge(apps.Configuration{}, patch)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "application configuration is invalid")
		return
	}
	respond[Configuration](writer, http.StatusOK, configuration, nil)
}

// RenderApps exposes the same chart renderer used to freeze deployment plans.
func (server handler) RenderApps(writer http.ResponseWriter, request *http.Request) {
	var body RenderInput
	if !server.decodeBody(writer, request, "RenderInput", &body) {
		return
	}
	configuration, err := convert[apps.Configuration](body.Configuration)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode application configuration")
		return
	}
	planID := "preview"
	if body.PlanID != nil {
		planID = *body.PlanID
	}
	issuer := ""
	if body.CertificateIssuer != nil {
		issuer = *body.CertificateIssuer
	}
	scope := apps.ResourceScope{}
	if body.Scope != nil {
		scope, err = convert[apps.ResourceScope](body.Scope)
		if err != nil {
			writeProblem(writer, http.StatusBadRequest, "invalid resource scope")
			return
		}
	}
	rendered, err := server.manager.RenderApps(request.Context(), configuration, body.Namespace, body.TargetID, planID, issuer, scope)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "application configuration could not be rendered")
		return
	}
	respond[Rendered](writer, http.StatusOK, rendered, nil)
}

// InspectApps reconstructs supported application fields from actual resources,
// allowing live resources and saved renderings to share one application format.
func (server handler) InspectApps(writer http.ResponseWriter, request *http.Request) {
	var body InspectInput
	if !server.decodeBody(writer, request, "InspectInput", &body) {
		return
	}
	manifests, err := convert[[]map[string]any](body.Manifests)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to decode Kubernetes manifests")
		return
	}
	configuration, err := server.manager.InspectApps(request.Context(), manifests)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "Kubernetes manifests cannot be represented as an application configuration")
		return
	}
	respond[Configuration](writer, http.StatusOK, configuration, nil)
}

// GetPlan returns the saved revision without rerendering or observing the cluster.
func (server handler) GetPlan(writer http.ResponseWriter, request *http.Request, plan string) {
	result, err := server.manager.GetPlan(request.Context(), plan)
	respond[Plan](writer, http.StatusOK, result, err)
}

// ApplyPlan accepts the exact saved artifacts for asynchronous application.
func (server handler) ApplyPlan(writer http.ResponseWriter, request *http.Request, plan string) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.Apply(request.Context(), plan)
	respond[Operation](writer, http.StatusAccepted, result, err)
}

// GetOperation allows clients to follow progress independently of their lifetime.
func (server handler) GetOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	result, err := server.manager.GetOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// CancelOperation interrupts unfinished work without rolling back accepted settings.
func (server handler) CancelOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.CancelOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// decodeBody validates before decoding so null and omitted fields cannot collapse
// to the same Go representation. Its output pointer is necessary for decoding.
func (server handler) decodeBody(writer http.ResponseWriter, request *http.Request, schemaName string, output any) bool {
	mediaType := "application/json"
	if value := request.Header.Get("Content-Type"); value != "" {
		var err error
		mediaType, _, err = mime.ParseMediaType(value)
		if err != nil {
			writeProblem(writer, http.StatusUnsupportedMediaType, "request Content-Type is invalid")
			return false
		}
	}
	if mediaType != "application/json" {
		writeProblem(writer, http.StatusUnsupportedMediaType, "request Content-Type is unsupported")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "request body exceeds the 1 MiB limit or could not be read")
		return false
	}
	var value any
	strictErrors, err := strictjson.UnmarshalStrict(data, &value, strictjson.DisallowDuplicateFields)
	if err != nil || len(strictErrors) != 0 {
		writeProblem(writer, http.StatusBadRequest, "request body must contain one JSON document with unique keys")
		return false
	}
	if err = server.schemas[schemaName].Value.VisitJSON(value); err != nil {
		message := SchemaValidationMessage(err)
		if message == "" {
			message = "request body does not match the OpenAPI schema"
		}
		writeProblem(writer, http.StatusBadRequest, message)
		return false
	}
	if err = json.Unmarshal(data, output); err != nil {
		writeProblem(writer, http.StatusBadRequest, "request body could not be decoded")
		return false
	}
	return true
}

func emptyBody(writer http.ResponseWriter, request *http.Request) bool {
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err != nil || len(bytes.TrimSpace(data)) != 0 {
		writeProblem(writer, http.StatusBadRequest, "this operation does not accept a request body")
		return false
	}
	return true
}

func respond[Model any](writer http.ResponseWriter, status int, result any, err error) {
	if err != nil {
		switch {
		case errors.Is(err, apps.ErrTelemetryUnavailable):
			writer.Header().Set("Retry-After", "15")
			writeProblem(writer, http.StatusServiceUnavailable, "Management telemetry is unavailable. Check Prometheus, Loki, and the target collector, then retry.")
		case errors.Is(err, apps.ErrNotFound):
			writeProblem(writer, http.StatusNotFound, "requested resource was not found")
		case errors.Is(err, apps.ErrComponentsNotReady):
			writeProblem(writer, http.StatusConflict, "target components are not ready; inspect cluster components or run cluster reconcile before planning applications")
		case errors.Is(err, apps.ErrAddonExists):
			writeProblem(writer, http.StatusConflict, "A custom addon with this name already exists in this space. Open it to update its settings, or use apply.")
		case errors.Is(err, apps.ErrEnvironmentGroupManaged):
			writeProblem(writer, http.StatusConflict, apps.ErrEnvironmentGroupManaged.Error())
		case errors.Is(err, apps.ErrConflict):
			message := apps.PostgresRequestMessage(err)
			if message == "" {
				message = "request conflicts with current state; create a new plan if needed"
			}
			writeProblem(writer, http.StatusConflict, message)
		case errors.Is(err, apps.ErrInvalid):
			message := apps.PostgresRequestMessage(err)
			if message == "" {
				message = "request configuration is invalid"
			}
			writeProblem(writer, http.StatusBadRequest, message)
		default:
			writeProblem(writer, http.StatusInternalServerError, "application manager request failed")
		}
		return
	}
	response, err := convert[Model](result)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "unable to encode application manager response")
		return
	}
	writeJSON(writer, status, response)
}

func convert[Output any](input any) (Output, error) {
	var output Output
	data, err := json.Marshal(input)
	if err != nil {
		return output, err
	}
	err = json.Unmarshal(data, &output)
	return output, err
}

func writeProblem(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, Problem{Message: message})
}

func writeJSON(writer http.ResponseWriter, status int, response any) {
	data, err := json.Marshal(response)
	if err != nil {
		http.Error(writer, "unable to encode response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(append(data, '\n'))
}
