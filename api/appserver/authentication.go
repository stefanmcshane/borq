package appserver

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
)

type browserSession struct {
	login   apps.OrganizationLogin
	admin   bool
	expires time.Time
	// The provider's verified logout hint is kept only in this private process-local session.
	logoutURL string
}

func (session browserSession) principal() apps.Principal {
	if session.admin {
		return apps.Principal{Admin: true}
	}
	if session.login.ExpiresAt.After(session.expires) {
		session.login.ExpiresAt = session.expires
	}
	return apps.Principal{Login: session.login, Permissions: []apps.Permission{"*"}}
}

// Sessions are deliberately process-local: a restart revokes browser access while
// the persistent bootstrap token remains available for recovery.
type authentication struct {
	manager   apps.Manager
	tokenHash [32]byte
	mutex     sync.Mutex
	sessions  map[[32]byte]browserSession
	page      *template.Template
	// OAuth handlers share pending grants, access tokens and their mutex.
	oauth *oauthState
}

func newAuthentication(manager apps.Manager, input NewInput) (*authentication, error) {
	path := input.AdminTokenFile
	if path == "" {
		path = filepath.Join(input.DataDir, "admin-token")
		if err := createAdminToken(path); err != nil {
			return nil, err
		}
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("could not read admin token file")
	}
	value := strings.TrimSpace(string(token))
	if len(value) < 32 || len(value) > 4096 || strings.ContainsAny(value, " \r\n\t") {
		return nil, errors.New("admin token must contain 32 to 4096 characters without whitespace")
	}
	page, err := template.New("login").Parse(loginPage)
	if err != nil {
		return nil, err
	}
	if _, err := page.New("logout").Parse(logoutPage); err != nil {
		return nil, err
	}
	if _, err := page.New("account").Parse(accountPage); err != nil {
		return nil, err
	}
	oauth, err := newOAuthState()
	if err != nil {
		return nil, err
	}
	return &authentication{manager: manager, tokenHash: sha256.Sum256([]byte(value)), sessions: map[[32]byte]browserSession{}, page: page, oauth: oauth}, nil
}

func createAdminToken(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Link a complete private file so a concurrent start cannot publish a partial
	// credential or replace the credential chosen by another bootstrap process.
	file, err := os.CreateTemp(filepath.Dir(path), ".admin-token-*")
	if err != nil {
		return errors.New("could not create admin token file")
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(rand.Text() + rand.Text() + "\n")
	closeErr := file.Close()
	if errors.Join(writeErr, closeErr) != nil {
		return errors.New("could not write admin token file")
	}
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("could not publish admin token file")
	}
	return nil
}

func (auth *authentication) close() {
	auth.oauth.client.CloseIdleConnections()
	auth.oauth.mutex.Lock()
	clear(auth.oauth.pending)
	clear(auth.oauth.codes)
	clear(auth.oauth.tokens)
	auth.oauth.mutex.Unlock()
	auth.mutex.Lock()
	clear(auth.sessions)
	auth.mutex.Unlock()
}

func (auth *authentication) validToken(request *http.Request) bool {
	values := request.Header.Values("Authorization")
	proxyValues := request.Header.Values("X-Borq-Admin-Token")
	var token string
	if len(values) == 1 && len(proxyValues) == 0 && strings.HasPrefix(values[0], "Bearer ") {
		token = strings.TrimPrefix(values[0], "Bearer ")
	} else if len(values) == 0 && len(proxyValues) == 1 {
		token = proxyValues[0]
	}
	if token == "" || len(token) > 4096 {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], auth.tokenHash[:]) == 1
}

func (auth *authentication) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if oauthPublicPath(path) {
			next.ServeHTTP(writer, request)
			return
		}
		if path == "/login" || path == "/login/admin" || path == "/api/auth/login" || path == "/api/auth/logout" ||
			path == "/api/healthz" || path == "/api/readyz" || path == "/api/livez" ||
			strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/_astro/") || strings.HasPrefix(path, "/api/v1/ingest/") ||
			path == "/api/auth/oidc/login" || strings.HasPrefix(path, "/api/auth/oidc/callback/") || appapi.IsJobRestoreCheckRequest(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if path != "/" && path != "/account" && path != "/account/tokens" && !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(writer, request)
			return
		}
		if auth.validToken(request) {
			next.ServeHTTP(writer, request.WithContext(apps.WithPrincipal(request.Context(), apps.Principal{Admin: true})))
			return
		}
		if values := request.Header.Values("Authorization"); len(values) == 1 && len(request.Header.Values("X-Borq-Admin-Token")) == 0 && strings.HasPrefix(values[0], "Bearer ") {
			if principal, ok := auth.oauthAuthenticate(request, strings.TrimPrefix(values[0], "Bearer ")); ok {
				next.ServeHTTP(writer, request.WithContext(apps.WithPrincipal(request.Context(), principal)))
				return
			}
			if principal, err := auth.manager.AuthenticateApiToken(request.Context(), strings.TrimPrefix(values[0], "Bearer ")); err == nil {
				next.ServeHTTP(writer, request.WithContext(apps.WithPrincipal(request.Context(), principal)))
				return
			}
		}
		// Browser cookies authorize the UI only. Administrative API and MCP callers
		// must explicitly supply a token; they cannot borrow a browser session.
		if path == "/" || path == "/account" || path == "/account/tokens" || strings.HasPrefix(path, "/api/ui/") || strings.HasPrefix(path, "/api/auth/") && path != "/api/auth/session" {
			if session, ok := auth.session(request); ok {
				principal := session.principal()
				if !principal.Admin {
					permissions, err := auth.manager.OrganizationLoginPermissions(request.Context(), principal.Login)
					if err != nil {
						authProblem(writer, http.StatusUnauthorized, "Organization access changed. Sign in again.")
						return
					}
					principal.Permissions = permissions
				}
				next.ServeHTTP(writer, request.WithContext(apps.WithPrincipal(request.Context(), principal)))
				return
			}
		}
		if path == "/" || path == "/account" {
			http.Redirect(writer, request, "/login", http.StatusSeeOther)
			return
		}
		reason := "missing_credentials"
		if len(request.Header.Values("Authorization")) > 0 || len(request.Header.Values("X-Borq-Admin-Token")) > 0 {
			reason = "invalid_credentials"
		}
		challenge := "Bearer"
		if path == "/api/mcp" {
			if origin, err := auth.oauthOrigin(request.Context(), request); err == nil {
				challenge += ` resource_metadata="` + origin + `/.well-known/oauth-protected-resource/api/mcp", scope="` + oauthDefaultScope + `"`
			}
		}
		slog.WarnContext(request.Context(), "authentication_rejected",
			"method", request.Method, "path", path, "status", http.StatusUnauthorized,
			"reason", reason, "authentication", "bearer_token", "oauth_advertised", challenge != "Bearer")
		writer.Header().Set("WWW-Authenticate", challenge)
		authProblem(writer, http.StatusUnauthorized, "Sign in to Borq. CLI: borq login --token-file PATH --api-url URL")
	})
}

func (auth *authentication) session(request *http.Request) (browserSession, bool) {
	cookie, err := request.Cookie("borq-session")
	if err != nil || len(cookie.Value) > 128 {
		return browserSession{}, false
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	auth.mutex.Lock()
	session, ok := auth.sessions[digest]
	if ok && !time.Now().Before(session.expires) {
		delete(auth.sessions, digest)
		ok = false
	}
	auth.mutex.Unlock()
	if ok && !session.admin && auth.manager.ValidateOrganizationLogin(request.Context(), session.login) != nil {
		return browserSession{}, false
	}
	return session, ok
}

func (auth *authentication) createSession(writer http.ResponseWriter, request *http.Request, session browserSession) bool {
	value := oidcRandom()
	if session.expires.IsZero() {
		session.expires = time.Now().Add(8 * time.Hour)
	}
	if maximum := time.Now().Add(8 * time.Hour); session.expires.After(maximum) {
		session.expires = maximum
	}
	auth.mutex.Lock()
	defer auth.mutex.Unlock()
	for key, previous := range auth.sessions {
		if !time.Now().Before(previous.expires) {
			delete(auth.sessions, key)
		}
	}
	if len(auth.sessions) >= 1024 {
		authProblem(writer, http.StatusServiceUnavailable, "Too many active sessions. Retry after existing sessions expire.")
		return false
	}
	// Reauthentication replaces the old session, including a previous administrator's session.
	if cookie, err := request.Cookie("borq-session"); err == nil {
		delete(auth.sessions, sha256.Sum256([]byte(cookie.Value)))
	}
	auth.sessions[sha256.Sum256([]byte(value))] = session
	http.SetCookie(writer, &http.Cookie{Name: "borq-session", Value: value, Path: "/", HttpOnly: true, Secure: secureBrowserCookie(request), SameSite: http.SameSiteLaxMode, MaxAge: int(time.Until(session.expires).Seconds())})
	return true
}

func secureBrowserCookie(request *http.Request) bool {
	host := request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return request.TLS != nil || !(host == "localhost" || net.ParseIP(strings.Trim(host, "[]")).IsLoopback())
}

func (auth *authentication) routes(mux *http.ServeMux) {
	auth.oauthRoutes(mux)
	mux.HandleFunc("GET /login", auth.loginPage)
	mux.HandleFunc("GET /login/admin", auth.loginPage)
	mux.Handle("POST /api/auth/login", http.NewCrossOriginProtection().Handler(http.HandlerFunc(auth.loginAdmin)))
	mux.Handle("POST /api/auth/logout", http.NewCrossOriginProtection().Handler(http.HandlerFunc(auth.logout)))
	mux.HandleFunc("GET /api/auth/session", func(writer http.ResponseWriter, request *http.Request) {
		principal, _ := apps.PrincipalFromContext(request.Context())
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]bool{"authenticated": true, "admin": principal.Admin})
	})
	mux.HandleFunc("GET /account", auth.account)
	mux.Handle("POST /account/tokens", http.NewCrossOriginProtection().Handler(managementApplicationAccess(auth.manager, http.HandlerFunc(auth.downloadToken))))
}

func (auth *authentication) loginPage(writer http.ResponseWriter, request *http.Request) {
	admin := request.URL.Path == "/login/admin"
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	policy := "default-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	// Browsers apply form-action to redirects too: organization sign-in leaves Borq for its provider.
	if admin {
		policy += "; form-action 'self'"
	}
	writer.Header().Set("Content-Security-Policy", policy)
	var organizations []string
	if !admin {
		var err error
		organizations, err = auth.manager.ListOrganizationSignInNames(request.Context())
		if err != nil {
			authProblem(writer, http.StatusServiceUnavailable, "Organization sign-in is temporarily unavailable. Use /login/admin for setup or recovery access.")
			return
		}
	}
	selected := ""
	for _, name := range organizations {
		if len(organizations) == 1 || name == request.URL.Query().Get("organization") {
			selected = name
		}
	}
	_ = auth.page.Execute(writer, struct {
		Admin                bool
		Organizations        []string
		SelectedOrganization string
	}{admin, organizations, selected})
}

func (auth *authentication) loginAdmin(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 8192)
	if request.ParseForm() != nil {
		authProblem(writer, http.StatusBadRequest, "Invalid login form.")
		return
	}
	token := request.PostForm.Get("token")
	digest := sha256.Sum256([]byte(token))
	if len(token) > 4096 || subtle.ConstantTimeCompare(digest[:], auth.tokenHash[:]) != 1 {
		authProblem(writer, http.StatusUnauthorized, "Invalid admin token.")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	if auth.createSession(writer, request, browserSession{admin: true}) {
		http.Redirect(writer, request, auth.oauthLoginReturn(request, "/"), http.StatusSeeOther)
	}
}

func (auth *authentication) logout(writer http.ResponseWriter, request *http.Request) {
	var session browserSession
	if cookie, err := request.Cookie("borq-session"); err == nil {
		auth.mutex.Lock()
		digest := sha256.Sum256([]byte(cookie.Value))
		session = auth.sessions[digest]
		delete(auth.sessions, digest)
		auth.mutex.Unlock()
	}
	http.SetCookie(writer, &http.Cookie{Name: "borq-session", Path: "/", HttpOnly: true, Secure: secureBrowserCookie(request), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	if !session.admin && session.logoutURL != "" {
		// A new document ends the form submission before leaving Borq; account forms keep form-action 'self'.
		nonce := oidcRandom()
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		_ = auth.page.ExecuteTemplate(writer, "logout", struct{ Nonce, Location string }{nonce, session.logoutURL})
		return
	}
	http.Redirect(writer, request, "/login", http.StatusSeeOther)
}

func authProblem(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"message": message})
}

const loginPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in · Borq</title><link rel="stylesheet" href="/assets/style.css"></head><body><main class="card"><h1>Sign in to Borq</h1>
{{if .Admin}}<h2>Setup and recovery</h2><p>Use the admin token provided by bootstrap to configure organization sign-in or recover access.</p><form method="post" action="/api/auth/login"><label>Admin token <input class="input" type="password" name="token" autocomplete="off" required maxlength="4096"></label><button class="btn" type="submit">Continue as administrator</button></form><p><a href="/login">Sign in with your organization</a></p>
{{else}}<p>Choose your organization to sign in with its configured identity provider.</p>{{if .Organizations}}<form method="post" action="/api/auth/oidc/login"><label for="organization">Organization</label><select class="input" name="organization" required id="organization">{{if and (gt (len .Organizations) 1) (not .SelectedOrganization)}}<option value="" selected disabled>Choose an organization</option>{{end}}{{range .Organizations}}<option value="{{.}}"{{if eq . $.SelectedOrganization}} selected{{end}}>{{.}}</option>{{end}}</select><button class="btn" type="submit">Continue to sign in</button></form>{{else}}<p>No organizations are ready for sign-in. Continue with your admin token, then configure OIDC when you are ready.</p>{{end}}<p><a href="/login/admin">Sign in with an admin token</a></p>{{end}}</main></body></html>`

const logoutPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Signing out · Borq</title></head><body><main><h1>Signing out</h1><p>You are signed out of Borq. Continue to your identity provider to finish signing out.</p><a href="{{.Location}}" rel="noreferrer">Finish signing out</a><script nonce="{{.Nonce}}">window.location.replace({{.Location}});</script></main></body></html>`
