package appserver

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/foundations/oidc"
)

type oidcAttempt struct {
	login        bool
	connection   apps.OrganizationOidcTest
	organization string
	cookieHash   [32]byte
	nonce        string
	verifier     string
	expires      time.Time
}

// Pointer receivers share the mutex, pending attempts and parsed HTML template across requests.
type oidcTests struct {
	authentication *authentication
	manager        apps.Manager
	mutex          sync.Mutex
	pending        map[string]oidcAttempt
	page           *template.Template
}

func newOidcTests(manager apps.Manager, authentication *authentication) (http.Handler, error) {
	page, err := template.New("oidc-result").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OIDC sign-in test · Borq</title></head>
<body><main><h1>{{.Title}}</h1><p>{{.Message}}</p>
{{if .Identity.Subject}}<dl><dt>Provider</dt><dd>{{.Identity.Issuer}}</dd><dt>Account ID</dt><dd>{{.Identity.Subject}}</dd>{{if .Identity.Name}}<dt>Name</dt><dd>{{.Identity.Name}}</dd>{{end}}{{if .Identity.Email}}<dt>Email</dt><dd>{{.Identity.Email}}</dd>{{end}}</dl>{{end}}
<p>This test does not sign you into Borq, grant permissions, or change its current access controls.</p>
<p><a href="{{.ReturnURL}}">Return to organization authentication settings</a></p></main></body></html>`)
	if err != nil {
		return nil, err
	}
	tests := &oidcTests{manager: manager, authentication: authentication, pending: map[string]oidcAttempt{}, page: page}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/oidc/test/{organization}", tests.start)
	mux.HandleFunc("POST /api/auth/oidc/login", tests.startLogin)
	mux.HandleFunc("GET /api/auth/oidc/callback/{provider}", tests.callback)
	return http.NewCrossOriginProtection().Handler(mux), nil
}

func (tests *oidcTests) startLogin(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	if request.ParseForm() != nil {
		authProblem(writer, http.StatusBadRequest, "Invalid sign-in form.")
		return
	}
	organization := request.PostForm.Get("organization")
	connection, err := tests.manager.PrepareOrganizationLogin(request.Context(), organization, request.Host)
	if err != nil {
		message := apps.OrganizationOidcRequestMessage(err)
		if message == "" {
			message = "Organization sign-in is unavailable. Use setup or recovery access to check its provider."
		}
		authProblem(writer, http.StatusForbidden, message)
		return
	}
	callback, _ := url.Parse(connection.CallbackURL)
	if callback == nil || !strings.EqualFold(callback.Host, request.Host) {
		authProblem(writer, http.StatusBadRequest, "Open the organization's configured Borq address before signing in.")
		return
	}
	tests.queueAttempt(writer, request, connection, organization, true, callback.Scheme == "https")
}

func (tests *oidcTests) queueAttempt(writer http.ResponseWriter, request *http.Request, connection apps.OrganizationOidcTest, organization string, login, secure bool) {
	state, cookie := oidcRandom(), oidcRandom()
	attempt := oidcAttempt{connection: connection, organization: organization, login: login, cookieHash: sha256.Sum256([]byte(cookie)), nonce: oidcRandom(), verifier: oidcRandom(), expires: time.Now().Add(5 * time.Minute)}
	tests.mutex.Lock()
	tests.expire()
	if len(tests.pending) >= 64 {
		tests.mutex.Unlock()
		authProblem(writer, http.StatusTooManyRequests, "Too many sign-in attempts. Retry in a few minutes.")
		return
	}
	tests.pending[state] = attempt
	tests.mutex.Unlock()
	writeOidcCookie(writer, cookie, secure, 300)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(writer, request, connection.Client.AuthorizationURL(state, attempt.nonce, attempt.verifier), http.StatusSeeOther)
}

func oidcRandom() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func (tests *oidcTests) start(writer http.ResponseWriter, request *http.Request) {
	organization := request.PathValue("organization")
	if err := tests.manager.Authorize(request.Context(), apps.PermissionOrganizationConfigure, organization); err != nil {
		authProblem(writer, http.StatusForbidden, "Permission denied.")
		return
	}
	settings, err := tests.manager.GetOrganizationOidc(request.Context(), organization)
	if err != nil || settings.CallbackURL == "" {
		tests.result(writer, http.StatusBadRequest, organization, "Save OIDC settings first", "Choose an organization and save its provider settings before testing sign-in.", oidc.Identity{})
		return
	}
	connection, err := tests.manager.PrepareOrganizationOidcTest(request.Context(), organization, request.Host)
	if err != nil {
		tests.result(writer, http.StatusBadGateway, organization, "Unable to start sign-in test", "Check the saved provider settings, provider availability, and management secret storage.", oidc.Identity{})
		return
	}
	// Recheck the snapshot actually used for discovery if settings changed during preparation.
	if !slices.Contains(settings.CallbackURLs, connection.CallbackURL) {
		tests.result(writer, http.StatusConflict, organization, "Settings changed", "Reload the settings and start a new sign-in test.", oidc.Identity{})
		return
	}
	callback, _ := url.Parse(connection.CallbackURL)
	tests.queueAttempt(writer, request, connection, organization, false, callback.Scheme == "https")
}

// expire is called only while holding the mutex; restart or expiry requires a new browser test.
func (tests *oidcTests) expire() {
	for state, attempt := range tests.pending {
		if !time.Now().Before(attempt.expires) {
			delete(tests.pending, state)
		}
	}
}

func (tests *oidcTests) callback(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	cookie, cookieErr := request.Cookie("borq-oidc-test")
	if cookieErr != nil || len(query["state"]) != 1 || len(query.Get("state")) != 43 {
		tests.result(writer, http.StatusBadRequest, "", "Sign-in test expired or invalid", "Start a new test from the organization's authentication settings.", oidc.Identity{})
		return
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	tests.mutex.Lock()
	tests.expire()
	attempt, found := tests.pending[query.Get("state")]
	if !found || subtle.ConstantTimeCompare(hash[:], attempt.cookieHash[:]) != 1 {
		tests.mutex.Unlock()
		tests.result(writer, http.StatusBadRequest, "", "Sign-in test expired or invalid", "Use the same browser that started the test, or start a new test.", oidc.Identity{})
		return
	}
	callback, _ := url.Parse(attempt.connection.CallbackURL)
	if !strings.EqualFold(callback.Host, request.Host) || callback.EscapedPath() != request.URL.EscapedPath() {
		tests.mutex.Unlock()
		tests.result(writer, http.StatusBadRequest, "", "Invalid callback address", "Return using the callback URL configured for this test.", oidc.Identity{})
		return
	}
	if err := tests.manager.ValidateOrganizationOidcCallback(request.Context(), attempt.organization, attempt.connection.CallbackURL); err != nil {
		delete(tests.pending, query.Get("state"))
		tests.mutex.Unlock()
		tests.result(writer, http.StatusBadRequest, attempt.organization, "Sign-in address changed", "Start sign-in again from a configured Borq address.", oidc.Identity{})
		return
	}
	// Consume before exchanging the code, including provider denial and failed exchanges.
	delete(tests.pending, query.Get("state"))
	tests.mutex.Unlock()
	writeOidcCookie(writer, "", callback.Scheme == "https", -1)
	if len(query["iss"]) > 1 || (query.Has("iss") && query.Get("iss") == "") || attempt.connection.Client.CheckResponseIssuer(query.Get("iss")) != nil {
		tests.result(writer, http.StatusBadRequest, attempt.organization, "Unexpected sign-in provider", "The response did not identify the provider configured for this test. Start a new sign-in test.", oidc.Identity{})
		return
	}
	if query.Has("error") || len(query["code"]) != 1 || query.Get("code") == "" {
		tests.result(writer, http.StatusBadRequest, attempt.organization, "Sign-in was not completed", "The provider declined or did not complete this sign-in. Check the app assignment and callback URL, then retry.", oidc.Identity{})
		return
	}
	authentication, err := attempt.connection.Client.Exchange(request.Context(), query.Get("code"), attempt.nonce, attempt.verifier)
	if err != nil {
		tests.result(writer, http.StatusBadRequest, attempt.organization, "Sign-in test failed", "Borq could not verify the provider's response. Check the client credentials and callback URL, then start a new test.", oidc.Identity{})
		return
	}
	identity := authentication.Identity
	if attempt.login {
		login, err := tests.manager.AuthorizeOrganizationLogin(request.Context(), attempt.organization, attempt.connection.Generation, identity)
		if err != nil {
			message := apps.OrganizationOidcRequestMessage(err)
			if message == "" {
				message = "This account is unavailable for this organization, or its provider settings changed."
			}
			authProblem(writer, http.StatusForbidden, message)
			return
		}
		if tests.authentication.createSession(writer, request, browserSession{login: login, expires: login.ExpiresAt, logoutURL: authentication.LogoutURL}) {
			http.Redirect(writer, request, tests.authentication.oauthLoginReturn(request, "/?"+url.Values{"organization": {attempt.organization}}.Encode()), http.StatusSeeOther)
		}
		return
	}
	if err := tests.manager.CompleteOrganizationOidcTest(request.Context(), attempt.organization, attempt.connection.Generation); err != nil {
		tests.result(writer, http.StatusConflict, attempt.organization, "Test result was not saved", "The settings may have changed during sign-in. Reload them and test again.", oidc.Identity{})
		return
	}
	tests.result(writer, http.StatusOK, attempt.organization, "Sign-in test passed", "Borq verified this account using the organization's saved OIDC configuration.", identity)
}

func writeOidcCookie(writer http.ResponseWriter, value string, secure bool, age int) {
	http.SetCookie(writer, &http.Cookie{Name: "borq-oidc-test", Value: value, Path: "/api/auth/oidc", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}

func (tests *oidcTests) result(writer http.ResponseWriter, status int, organization, title, message string, identity oidc.Identity) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = tests.page.Execute(writer, struct {
		Title, Message, ReturnURL string
		Identity                  oidc.Identity
	}{title, message, "/?" + url.Values{"view": {"organization-authentication"}, "organization": {organization}}.Encode(), identity})
}
