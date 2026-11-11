package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/oidc"
)

const oauthDefaultScope = "organization:read cluster:read space:read app:read"

// Like browser sessions, OAuth grants are process-local and revoked on restart.
type oauthState struct {
	mutex  sync.Mutex
	client http.Client
	// Templates are immutable after construction and shared by concurrent handlers.
	page    *template.Template
	scopes  []string
	fetches chan struct{}
	pending map[[32]byte]oauthAuthorization
	codes   map[[32]byte]oauthCode
	tokens  map[[32]byte]oauthAccess
}

type oauthAuthorization struct {
	client                               oauthClientMetadata
	redirect, resource, state, challenge string
	scopes                               []apps.Permission
	expires                              time.Time
	sessionHash                          [32]byte
}

type oauthCode struct {
	authorization oauthAuthorization
	principal     apps.Principal
	expires       time.Time
}

type oauthAccess struct {
	credential string
	resource   string
	expires    time.Time
}

func newOAuthState() (*oauthState, error) {
	page, err := template.New("consent").Parse(oauthConsentPage)
	if err != nil {
		return nil, err
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	for _, value := range specification.Components.Schemas["Permission"].Value.Enum {
		permission := apps.Permission(value.(string))
		if oauthPermission(permission) {
			scopes = append(scopes, string(permission))
		}
	}
	return &oauthState{client: newOAuthHttpClient(), page: page, scopes: scopes, fetches: make(chan struct{}, 8),
		pending: map[[32]byte]oauthAuthorization{}, codes: map[[32]byte]oauthCode{}, tokens: map[[32]byte]oauthAccess{}}, nil
}

func oauthPermission(permission apps.Permission) bool {
	return apps.ValidPermission(permission) && permission != apps.PermissionManagementRead && permission != apps.PermissionManagementConfigure &&
		permission != apps.PermissionOrganizationCreate && permission != apps.PermissionTokenCreate
}

func oauthPublicPath(path string) bool {
	switch path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/api/mcp", "/.well-known/oauth-authorization-server",
		"/api/auth/oauth/authorize", "/api/auth/oauth/consent", "/api/auth/oauth/token":
		return true
	}
	return false
}

func (auth *authentication) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", auth.oauthResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/api/mcp", auth.oauthResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", auth.oauthServerMetadata)
	mux.Handle("GET /api/auth/oauth/authorize", managementApplicationAccess(auth.manager, http.HandlerFunc(auth.oauthAuthorize)))
	mux.Handle("GET /api/auth/oauth/consent", managementApplicationAccess(auth.manager, http.HandlerFunc(auth.oauthConsent)))
	mux.Handle("POST /api/auth/oauth/consent", http.NewCrossOriginProtection().Handler(managementApplicationAccess(auth.manager, http.HandlerFunc(auth.oauthApprove))))
	mux.Handle("POST /api/auth/oauth/token", managementApplicationAccess(auth.manager, http.HandlerFunc(auth.oauthToken)))
}

func (auth *authentication) oauthOrigin(ctx context.Context, request *http.Request) (string, error) {
	settings, err := auth.manager.GetPlatformSettings(ctx)
	if err != nil {
		return "", err
	}
	origins := []string{settings.BorqPublicURL}
	if settings.Network.Tailscale != nil {
		origins = append(origins, settings.Network.ManagementURL)
	}
	organizations, err := auth.manager.ListOrganizationSignInNames(ctx)
	if err != nil {
		return "", err
	}
	for _, organization := range organizations {
		provider, err := auth.manager.GetOrganizationOidc(ctx, organization)
		if err != nil {
			return "", err
		}
		origins = append(origins, provider.BaseURL)
	}
	for _, origin := range origins {
		address, err := url.Parse(origin)
		if err == nil && oidc.ValidateURL(origin) == nil && address.Path == "" && strings.EqualFold(address.Host, request.Host) {
			return strings.ToLower(address.Scheme + "://" + address.Host), nil
		}
	}
	local := "http://" + request.Host
	if request.TLS != nil {
		local = "https://" + request.Host
	}
	address, err := url.Parse(local)
	if err == nil && oidc.ValidateURL(local) == nil && (address.Hostname() == "localhost" || net.ParseIP(address.Hostname()).IsLoopback()) {
		return strings.ToLower(local), nil
	}
	return "", errors.New("OAuth requires a configured Borq origin")
}

func (auth *authentication) oauthResourceMetadata(writer http.ResponseWriter, request *http.Request) {
	origin, err := auth.oauthOrigin(request.Context(), request)
	if err != nil {
		oauthProblem(writer, request, http.StatusBadRequest, "discovery", "invalid_request", "Open a configured Borq address to discover MCP sign-in.")
		return
	}
	oauthJSON(writer, struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		Scopes               []string `json:"scopes_supported"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}{origin + "/api/mcp", []string{origin}, strings.Fields(oauthDefaultScope), []string{"header"}})
}

func (auth *authentication) oauthServerMetadata(writer http.ResponseWriter, request *http.Request) {
	origin, err := auth.oauthOrigin(request.Context(), request)
	if err != nil {
		oauthProblem(writer, request, http.StatusBadRequest, "discovery", "invalid_request", "Open a configured Borq address to discover MCP sign-in.")
		return
	}
	oauthJSON(writer, struct {
		Issuer         string   `json:"issuer"`
		Authorize      string   `json:"authorization_endpoint"`
		Token          string   `json:"token_endpoint"`
		Responses      []string `json:"response_types_supported"`
		Grants         []string `json:"grant_types_supported"`
		AuthMethods    []string `json:"token_endpoint_auth_methods_supported"`
		Challenges     []string `json:"code_challenge_methods_supported"`
		Scopes         []string `json:"scopes_supported"`
		Cimd           bool     `json:"client_id_metadata_document_supported"`
		ResponseIssuer bool     `json:"authorization_response_iss_parameter_supported"`
	}{origin, origin + "/api/auth/oauth/authorize", origin + "/api/auth/oauth/token", []string{"code"}, []string{"authorization_code"},
		[]string{"none"}, []string{"S256"}, auth.oauth.scopes, true, true})
}

func (state *oauthState) expire() {
	now := time.Now()
	for key, pending := range state.pending {
		if !now.Before(pending.expires) {
			delete(state.pending, key)
		}
	}
	for key, code := range state.codes {
		if !now.Before(code.expires) {
			delete(state.codes, key)
		}
	}
	for key, token := range state.tokens {
		if !now.Before(token.expires) {
			delete(state.tokens, key)
		}
	}
}

func oauthSingleValues(values url.Values) bool {
	for _, entries := range values {
		if len(entries) != 1 {
			return false
		}
	}
	return true
}

func writeOAuthCookie(writer http.ResponseWriter, request *http.Request, value string, maxAge int) {
	http.SetCookie(writer, &http.Cookie{Name: "borq-oauth", Value: value, Path: "/", HttpOnly: true, Secure: secureBrowserCookie(request), SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func oauthJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	_ = json.NewEncoder(writer).Encode(value)
}

func oauthProblem(writer http.ResponseWriter, request *http.Request, status int, stage, code, description string, details ...any) {
	fields := []any{"method", request.Method, "path", request.URL.Path, "status", status, "stage", stage, "reason", code}
	slog.WarnContext(request.Context(), "mcp_oauth_rejected", append(fields, details...)...)
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}{code, description})
}
