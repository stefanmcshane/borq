package appserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/apps"
)

func (auth *authentication) oauthToken(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 8192)
	if request.ParseForm() != nil || !oauthSingleValues(request.PostForm) || request.PostForm.Get("grant_type") != "authorization_code" ||
		len(request.Header.Values("Authorization")) != 0 || request.PostForm.Has("client_secret") {
		oauthProblem(writer, request, http.StatusBadRequest, "token", "invalid_request", "Use authorization code flow with a public client and PKCE.")
		return
	}
	values := request.PostForm
	digest := sha256.Sum256([]byte(values.Get("code")))
	auth.oauth.mutex.Lock()
	code, ok := auth.oauth.codes[digest]
	// Every exchange attempt consumes the code, including incorrect PKCE proofs.
	delete(auth.oauth.codes, digest)
	auth.oauth.mutex.Unlock()
	origin, err := auth.oauthOrigin(request.Context(), request)
	verifier := values.Get("code_verifier")
	challenge := sha256.Sum256([]byte(verifier))
	if !ok || !time.Now().Before(code.expires) || err != nil || code.authorization.resource != origin+"/api/mcp" ||
		values.Get("resource") != code.authorization.resource || values.Get("client_id") != code.authorization.client.ID ||
		values.Get("redirect_uri") != code.authorization.redirect || len(verifier) < 43 || len(verifier) > 128 ||
		strings.Trim(verifier, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~") != "" ||
		subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(challenge[:])), []byte(code.authorization.challenge)) != 1 ||
		auth.manager.ValidateOrganizationLogin(request.Context(), code.principal.Login) != nil {
		oauthProblem(writer, request, http.StatusBadRequest, "token", "invalid_grant", "Authorization code is invalid, expired, or does not match this client, redirect, resource, and PKCE proof.")
		return
	}
	delegator := code.principal
	delegator.Permissions = append(slices.Clone(delegator.Permissions), apps.PermissionTokenCreate)
	ctx := apps.WithPrincipal(request.Context(), delegator)
	if auth.manager.Authorize(ctx, apps.PermissionTokenCreate, code.principal.Login.Organization) != nil {
		oauthProblem(writer, request, http.StatusBadRequest, "token", "invalid_grant", "Token creation permission changed. Connect your MCP client again.")
		return
	}
	for _, scope := range code.principal.Permissions {
		if auth.manager.Authorize(ctx, scope, code.principal.Login.Organization) != nil {
			oauthProblem(writer, request, http.StatusBadRequest, "token", "invalid_grant", "Organization permissions changed. Connect your MCP client again.")
			return
		}
	}
	value := "borq_mcp_" + oidcRandom() + oidcRandom()
	auth.oauth.mutex.Lock()
	auth.oauth.expire()
	if len(auth.oauth.tokens) >= 1024 {
		auth.oauth.mutex.Unlock()
		oauthProblem(writer, request, http.StatusServiceUnavailable, "token", "temporarily_unavailable", "Too many active MCP tokens. Retry after existing tokens expire.")
		return
	}
	// Keep issuance within the same capacity check, and reuse durable token
	// revocation and queued-operation authorization behind the MCP-only handle.
	token, err := auth.manager.CreateApiToken(ctx, apps.CreateApiTokenInput{Permissions: code.principal.Permissions, ExpiresInSeconds: 3600})
	if err != nil {
		auth.oauth.mutex.Unlock()
		oauthProblem(writer, request, http.StatusBadRequest, "token", "invalid_grant", "Organization access changed. Connect your MCP client again.")
		return
	}
	auth.oauth.tokens[sha256.Sum256([]byte(value))] = oauthAccess{credential: token.Token, resource: code.authorization.resource, expires: token.ExpiresAt}
	auth.oauth.mutex.Unlock()
	scopeNames := make([]string, len(code.principal.Permissions))
	for index, permission := range code.principal.Permissions {
		scopeNames[index] = string(permission)
	}
	slog.InfoContext(request.Context(), "mcp_oauth_token_issued", "method", request.Method, "path", request.URL.Path, "status", http.StatusOK)
	oauthJSON(writer, struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}{value, "Bearer", int(time.Until(token.ExpiresAt).Seconds()), strings.Join(scopeNames, " ")})
}

func (auth *authentication) oauthAuthenticate(request *http.Request, token string) (apps.Principal, bool) {
	if request.URL.Path != "/api/mcp" || !strings.HasPrefix(token, "borq_mcp_") || len(token) > 128 {
		return apps.Principal{}, false
	}
	auth.oauth.mutex.Lock()
	access, ok := auth.oauth.tokens[sha256.Sum256([]byte(token))]
	auth.oauth.mutex.Unlock()
	origin, err := auth.oauthOrigin(request.Context(), request)
	if !ok || err != nil || access.resource != origin+"/api/mcp" || !time.Now().Before(access.expires) {
		return apps.Principal{}, false
	}
	principal, err := auth.manager.AuthenticateApiToken(request.Context(), access.credential)
	return principal, err == nil
}
