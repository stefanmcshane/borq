package appapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// NewAuthorizationHandler is shared by external HTTP, MCP dispatch and the UI's
// in-process API calls. Unknown operations never inherit a read or write grant.
func NewAuthorizationHandler(manager apps.Manager, next http.Handler) (http.Handler, error) {
	specification, err := GetSwagger()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	for path, item := range specification.Paths.Map() {
		for method, operation := range item.Operations() {
			security, err := operationSecurity(specification, operation)
			if err != nil {
				return nil, err
			}
			mux.Handle(method+" "+path, authorizeApiOperation(manager, next, security, operation.OperationID))
		}
	}
	// Restore jobs authenticate through TokenReview and their immutable Job UID.
	// A projected Kubernetes credential cannot also be a user or admin token.
	for _, pattern := range []string{
		"POST /api/v1/organizations/{organization}/spaces/{space}/managed-addons/postgres/{postgres}/restore-check",
		"GET /api/v1/organizations/{organization}/spaces/{space}/managed-addons/postgres/{postgres}/restore-check/{operation}",
	} {
		mux.Handle(pattern, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			release, err := manager.BeginApplicationRequest(request.Context())
			if err != nil {
				writeProblem(writer, http.StatusConflict, err.Error())
				return
			}
			defer release()
			next.ServeHTTP(writer, request)
		}))
	}
	for _, path := range []string{"GET /api/openapi.yaml", "GET /api/docs/"} {
		mux.Handle(path, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if _, ok := apps.PrincipalFromContext(request.Context()); !ok {
				writeAuthorizationProblem(writer, apps.ErrUnauthenticated)
				return
			}
			next.ServeHTTP(writer, request)
		}))
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Match the transport's legacy aliases before selecting an authorization rule.
		parts := strings.Split(request.URL.Path, "/")
		if len(parts) >= 8 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "organizations" && parts[5] == "spaces" && parts[7] == "addons" {
			parts[7] = "custom-charts"
		}
		if len(parts) == 5 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "addon-operations" {
			parts[3] = "custom-chart-operations"
		}
		if path := strings.Join(parts, "/"); path != request.URL.Path {
			request = request.Clone(request.Context())
			request.URL.Path = path
			request.URL.RawPath = ""
		}
		mux.ServeHTTP(writer, request)
	}), nil
}

func authorizeApiOperation(manager apps.Manager, next http.Handler, security apiOperationSecurity, operation string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := apps.PrincipalFromContext(request.Context())
		if !ok {
			writeAuthorizationProblem(writer, apps.ErrUnauthenticated)
			return
		}
		organization := request.PathValue("organization")
		requiredPermission := security.permission
		if security.targetPermission != "" && request.URL.Query().Get("scope") == "target" {
			organization = request.URL.Query().Get("organization")
			requiredPermission = security.targetPermission
		}
		if !principal.Admin {

			if operation == "ListOrganizations" {
				organization = principal.Login.Organization
			}
			if organization == "" {
				organization = operationOrganization(manager, request, operation, principal.Login.Organization)
			}
		}
		if err := manager.Authorize(request.Context(), requiredPermission, organization); err != nil {
			writeAuthorizationProblem(writer, err)
			return
		}
		if operation != "PlanManagementDestroy" && operation != "DestroyManagement" && operation != "GetManagementDestroy" {
			release, err := manager.BeginApplicationRequest(request.Context())
			if err != nil {
				writeProblem(writer, http.StatusConflict, err.Error())
				return
			}
			defer release()
		}
		if !principal.Admin && operation == "ListOrganizations" {
			organizations, err := manager.ListOrganizations(request.Context())
			if err != nil {
				writeAuthorizationProblem(writer, apps.ErrForbidden)
				return
			}
			visible := []apps.Organization{}
			for _, entry := range organizations {
				if entry.ID == principal.Login.OrganizationID {
					visible = append(visible, entry)
				}
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(writer).Encode(visible)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func IsJobRestoreCheckRequest(request *http.Request) bool {
	parts := strings.Split(request.URL.Path, "/")
	if len(parts) != 11 && len(parts) != 12 {
		return false
	}
	if parts[1] != "api" || parts[2] != "v1" || parts[3] != "organizations" || parts[5] != "spaces" || parts[7] != "managed-addons" || parts[8] != "postgres" || parts[10] != "restore-check" {
		return false
	}
	for _, index := range []int{4, 6, 9} {
		if parts[index] == "" || parts[index] == "." || parts[index] == ".." {
			return false
		}
	}
	return len(parts) == 11 && request.Method == http.MethodPost || len(parts) == 12 && parts[11] != "" && request.Method == http.MethodGet
}

func operationOrganization(manager apps.Manager, request *http.Request, operation, organization string) string {
	switch operation {
	case "GetChartDefaults", "ExpandApps", "RenderApps", "GetClusterSetup", "CreateAPIToken":
		return organization
	case "GetPlan", "ApplyPlan":
		plan, err := manager.GetPlan(request.Context(), request.PathValue("plan"))
		if err == nil {
			return plan.Space.Organization
		}
	case "GetOperation", "CancelOperation", "GetPostgresOperation", "GetPocketIDOperation", "GetCustomChartOperation":
		var result apps.Operation
		var err error
		switch operation {
		case "GetPocketIDOperation":
			result, err = manager.GetPocketIdOperation(request.Context(), request.PathValue("operation"))
		case "GetPostgresOperation":
			result, err = manager.GetPostgresOperation(request.Context(), request.PathValue("operation"))
		case "GetCustomChartOperation":
			result, err = manager.GetAddonOperation(request.Context(), request.PathValue("operation"))
		default:
			result, err = manager.GetOperation(request.Context(), request.PathValue("operation"))
		}
		if err != nil || result.SpaceID == "" {
			return ""
		}
		spaces, err := manager.ListSpaces(request.Context(), organization)
		if err == nil {
			for _, space := range spaces {
				if space.ID == result.SpaceID {
					return organization
				}
			}
		}
	}
	return ""
}

func writeAuthorizationProblem(writer http.ResponseWriter, err error) {
	status := http.StatusForbidden
	message := apps.ErrForbidden.Error()
	if errors.Is(err, apps.ErrUnauthenticated) {
		status = http.StatusUnauthorized
		message = apps.ErrUnauthenticated.Error()
		writer.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeProblem(writer, status, message)
}

type apiOperationSecurity struct {
	permission       apps.Permission
	targetPermission apps.Permission
}

// OperationPermissionDescription exposes the same validated permission contract used
// by HTTP authorization so MCP tools cannot advertise a different grant.
func OperationPermissionDescription(specification *openapi3.T, operation *openapi3.Operation) (string, error) {
	security, err := operationSecurity(specification, operation)
	if err != nil {
		return "", err
	}
	description := "Required permission: " + string(security.permission) + "."
	if security.targetPermission != "" {
		description += " When scope=target, requires " + string(security.targetPermission) + " in the selected organization instead."
	}
	return description, nil
}

func operationSecurity(specification *openapi3.T, operation *openapi3.Operation) (apiOperationSecurity, error) {
	invalid := func() (apiOperationSecurity, error) {
		return apiOperationSecurity{}, fmt.Errorf("API operation %s must declare one supported permission in security", operation.OperationID)
	}
	// Each operation explicitly declares one grant. Inherited requirements, OR
	// alternatives and empty objects could accidentally make a new endpoint public.
	if operation.Security == nil || len(*operation.Security) != 1 || len((*operation.Security)[0]) != 1 {
		return invalid()
	}
	var permission apps.Permission
	for name, scopes := range (*operation.Security)[0] {
		if len(scopes) != 0 {
			return invalid()
		}
		var err error
		permission, err = securitySchemePermission(specification, name)
		if err != nil {
			return invalid()
		}
	}
	result := apiOperationSecurity{permission: permission}
	extension, exists := operation.Extensions["x-borq-target-security"]
	if !exists {
		return result, nil
	}
	// The only conditional scope is the component catalog's existing target mode.
	// Reject new or malformed conditions until their resource ownership is defined.
	raw, err := json.Marshal(extension)
	if err != nil {
		return invalid()
	}
	var target struct {
		Query             string `json:"query"`
		Value             string `json:"value"`
		OrganizationQuery string `json:"organizationQuery"`
		Scheme            string `json:"scheme"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&target) != nil || operation.OperationID != "GetComponentCatalog" || permission != apps.PermissionManagementRead || target.Query != "scope" || target.Value != "target" || target.OrganizationQuery != "organization" {
		return invalid()
	}
	result.targetPermission, err = securitySchemePermission(specification, target.Scheme)
	if err != nil || result.targetPermission != apps.PermissionClusterRead {
		return invalid()
	}
	return result, nil
}

func securitySchemePermission(specification *openapi3.T, name string) (apps.Permission, error) {
	if specification.Components == nil {
		return "", apps.ErrForbidden
	}
	reference := specification.Components.SecuritySchemes[name]
	if reference == nil || reference.Value == nil {
		return "", apps.ErrForbidden
	}
	scheme := reference.Value
	value, ok := scheme.Extensions["x-borq-permission"].(string)
	permission := apps.Permission(value)
	if scheme.Type != "http" || scheme.Scheme != "bearer" || !ok || !apps.ValidPermission(permission) {
		return "", apps.ErrForbidden
	}
	return permission, nil
}
