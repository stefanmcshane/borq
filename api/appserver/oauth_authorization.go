package appserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/apps"
)

func (auth *authentication) oauthAuthorize(writer http.ResponseWriter, request *http.Request) {
	origin, err := auth.oauthOrigin(request.Context(), request)
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if err != nil || parseErr != nil || len(request.URL.RawQuery) > 8192 || !oauthSingleValues(query) || query.Get("response_type") != "code" ||
		query.Get("resource") != origin+"/api/mcp" || query.Get("code_challenge_method") != "S256" || len(query.Get("state")) > 1024 {
		oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_request", "Use authorization code flow, the advertised MCP resource, and PKCE S256.")
		return
	}
	challenge, err := base64.RawURLEncoding.DecodeString(query.Get("code_challenge"))
	if err != nil || len(challenge) != sha256.Size || len(query.Get("code_challenge")) != 43 {
		oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_request", "A valid PKCE S256 challenge is required.")
		return
	}
	scope := query.Get("scope")
	if scope == "" {
		scope = oauthDefaultScope
	}
	scopes := []apps.Permission{}
	for _, value := range strings.Fields(scope) {
		permission := apps.Permission(value)
		if !oauthPermission(permission) || slices.Contains(scopes, permission) || len(scopes) >= 64 {
			oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_scope", "Request named organization permissions without token creation or installation administration.")
			return
		}
		scopes = append(scopes, permission)
	}
	if len(scopes) == 0 {
		oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_scope", "Request at least one organization permission.")
		return
	}
	select {
	case auth.oauth.fetches <- struct{}{}:
		defer func() { <-auth.oauth.fetches }()
	default:
		oauthProblem(writer, request, http.StatusServiceUnavailable, "authorize", "temporarily_unavailable", "Too many client metadata requests. Retry shortly.")
		return
	}
	client, err := fetchOAuthClient(request.Context(), auth.oauth.client, query.Get("client_id"))
	if err != nil {
		oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_client", "Could not verify public HTTPS client metadata. Only public clients using PKCE are supported.", "detail", err)
		return
	}
	if !slices.Contains(client.RedirectURIs, query.Get("redirect_uri")) {
		oauthProblem(writer, request, http.StatusBadRequest, "authorize", "invalid_client", "The redirect URI does not exactly match the client's metadata.")
		return
	}
	value := oidcRandom()
	auth.oauth.mutex.Lock()
	auth.oauth.expire()
	if cookie, err := request.Cookie("borq-oauth"); err == nil {
		delete(auth.oauth.pending, sha256.Sum256([]byte(cookie.Value)))
	}
	if len(auth.oauth.pending) >= 64 {
		var oldest [32]byte
		var expiry time.Time
		// Unsigned requests must not reserve every slot; preserve consent already
		// displayed to an authenticated browser while replacing abandoned starts.
		for key, pending := range auth.oauth.pending {
			if pending.sessionHash == [32]byte{} && (expiry.IsZero() || pending.expires.Before(expiry)) {
				oldest, expiry = key, pending.expires
			}
		}
		if expiry.IsZero() {
			auth.oauth.mutex.Unlock()
			oauthProblem(writer, request, http.StatusTooManyRequests, "authorize", "temporarily_unavailable", "Too many pending consent forms. Retry shortly.")
			return
		}
		delete(auth.oauth.pending, oldest)
	}
	auth.oauth.pending[sha256.Sum256([]byte(value))] = oauthAuthorization{client: client, redirect: query.Get("redirect_uri"), resource: origin + "/api/mcp",
		state: query.Get("state"), challenge: query.Get("code_challenge"), scopes: scopes, expires: time.Now().Add(10 * time.Minute)}
	auth.oauth.mutex.Unlock()
	writeOAuthCookie(writer, request, value, 600)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(writer, request, "/api/auth/oauth/consent", http.StatusSeeOther)
}

func (auth *authentication) oauthPending(request *http.Request) (oauthAuthorization, string, bool) {
	cookie, err := request.Cookie("borq-oauth")
	if err != nil || len(cookie.Value) != 43 {
		return oauthAuthorization{}, "", false
	}
	auth.oauth.mutex.Lock()
	pending, ok := auth.oauth.pending[sha256.Sum256([]byte(cookie.Value))]
	auth.oauth.mutex.Unlock()
	origin, err := auth.oauthOrigin(request.Context(), request)
	return pending, cookie.Value, ok && time.Now().Before(pending.expires) && err == nil && pending.resource == origin+"/api/mcp"
}

func (auth *authentication) oauthLoginReturn(request *http.Request, fallback string) string {
	if _, _, ok := auth.oauthPending(request); ok {
		return "/api/auth/oauth/consent"
	}
	return fallback
}

func (auth *authentication) oauthPrincipal(request *http.Request, pending oauthAuthorization) (apps.Principal, [32]byte, bool) {
	session, ok := auth.session(request)
	if !ok || session.admin {
		return apps.Principal{}, [32]byte{}, false
	}
	principal := session.principal()
	ctx := apps.WithPrincipal(request.Context(), principal)
	if auth.manager.Authorize(ctx, apps.PermissionTokenCreate, principal.Login.Organization) != nil {
		return apps.Principal{}, [32]byte{}, false
	}
	for _, scope := range pending.scopes {
		if auth.manager.Authorize(ctx, scope, principal.Login.Organization) != nil {
			return apps.Principal{}, [32]byte{}, false
		}
	}
	cookie, _ := request.Cookie("borq-session")
	principal.Permissions = slices.Clone(pending.scopes)
	return principal, sha256.Sum256([]byte(cookie.Value)), true
}

func (auth *authentication) oauthConsent(writer http.ResponseWriter, request *http.Request) {
	pending, value, ok := auth.oauthPending(request)
	if !ok {
		oauthProblem(writer, request, http.StatusBadRequest, "consent", "invalid_request", "Authorization expired. Connect your MCP client again.")
		return
	}
	if _, ok := auth.session(request); !ok {
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return
	}
	principal, sessionHash, ok := auth.oauthPrincipal(request, pending)
	if !ok {
		oauthProblem(writer, request, http.StatusForbidden, "consent", "access_denied", "Sign in with an organization account that can create tokens and grant the requested permissions.")
		return
	}
	pending.sessionHash = sessionHash
	auth.oauth.mutex.Lock()
	if _, exists := auth.oauth.pending[sha256.Sum256([]byte(value))]; !exists {
		auth.oauth.mutex.Unlock()
		oauthProblem(writer, request, http.StatusBadRequest, "consent", "invalid_request", "Authorization was already completed.")
		return
	}
	auth.oauth.pending[sha256.Sum256([]byte(value))] = pending
	auth.oauth.mutex.Unlock()
	client, _ := url.Parse(pending.client.ID)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	// form-action also constrains redirects; approval returns to the verified client's origin.
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	_ = auth.oauth.page.Execute(writer, struct {
		Client, Host, Redirect, Organization, Csrf string
		Scopes                                     []apps.Permission
	}{pending.client.Name, client.Host, pending.redirect, principal.Login.Organization, value, pending.scopes})
}

func (auth *authentication) oauthApprove(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	pending, value, ok := auth.oauthPending(request)
	if request.ParseForm() != nil || !oauthSingleValues(request.PostForm) || !ok ||
		subtle.ConstantTimeCompare([]byte(request.PostForm.Get("csrf")), []byte(value)) != 1 ||
		(request.PostForm.Get("decision") != "allow" && request.PostForm.Get("decision") != "deny") {
		oauthProblem(writer, request, http.StatusBadRequest, "consent", "invalid_request", "Authorization expired or the consent form is invalid.")
		return
	}
	principal, sessionHash, ok := auth.oauthPrincipal(request, pending)
	if !ok || pending.sessionHash == [32]byte{} || pending.sessionHash != sessionHash {
		oauthProblem(writer, request, http.StatusForbidden, "consent", "access_denied", "Your account or permissions changed. Connect your MCP client again.")
		return
	}
	code := oidcRandom()
	auth.oauth.mutex.Lock()
	auth.oauth.expire()
	if _, exists := auth.oauth.pending[sha256.Sum256([]byte(value))]; !exists || len(auth.oauth.codes) >= 64 {
		auth.oauth.mutex.Unlock()
		oauthProblem(writer, request, http.StatusBadRequest, "consent", "invalid_request", "Authorization expired or was already completed.")
		return
	}
	delete(auth.oauth.pending, sha256.Sum256([]byte(value)))
	allowed := request.PostForm.Get("decision") == "allow"
	if allowed {
		auth.oauth.codes[sha256.Sum256([]byte(code))] = oauthCode{authorization: pending, principal: principal, expires: time.Now().Add(time.Minute)}
	}
	auth.oauth.mutex.Unlock()
	writeOAuthCookie(writer, request, "", -1)
	redirect, _ := url.Parse(pending.redirect)
	query := redirect.Query()
	query.Set("state", pending.state)
	query.Set("iss", strings.TrimSuffix(pending.resource, "/api/mcp"))
	if allowed {
		query.Set("code", code)
	} else {
		query.Set("error", "access_denied")
	}
	redirect.RawQuery = query.Encode()
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(writer, request, redirect.String(), http.StatusSeeOther)
}

const oauthConsentPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect an MCP client · Borq</title><link rel="stylesheet" href="/assets/style.css"></head><body><main class="card"><h1>Connect {{.Client}}</h1><p>The client at <strong>{{.Host}}</strong> is requesting access to the <strong>{{.Organization}}</strong> organization.</p><p>Allow these permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><p>After approval, Borq returns you to <code>{{.Redirect}}</code>.</p><form method="post" action="/api/auth/oauth/consent"><input type="hidden" name="csrf" value="{{.Csrf}}"><button class="btn" name="decision" value="allow" type="submit">Allow access</button><button class="btn" name="decision" value="deny" type="submit">Cancel</button></form></main></body></html>`
