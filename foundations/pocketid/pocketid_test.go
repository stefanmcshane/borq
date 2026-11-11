package pocketid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
)

// TestPocketIdTransportBoundaries keeps administrator credentials on the configured service transport.
func TestPocketIdTransportBoundaries(t *testing.T) {
	for _, address := range []string{"http://example.com", "https://user:password@example.com", "https://example.com?query", "https://example.com#fragment", "//example.com"} {
		t.Run(address, func(t *testing.T) {
			_, err := New(Config{BaseURL: address, ApiKey: strings.Repeat("k", 32)})
			if err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
	is := is.New(t)
	_, err := New(Config{BaseURL: "https://example.com", ApiKey: "bad\r\nkey"})
	is.True(err != nil)
	var leaked atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	service := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		is.Equal(request.Header.Get("X-API-KEY"), "private-management-key")
		is.Equal(request.URL.Path, "/api/v1/namespaces/management/services/pocketid:1411/proxy/api/users/b7bd858a-95b7-4a24-bb17-c47c9126e320/webauthn-credentials")
		writer.Header().Set("Location", destination.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer service.Close()
	client, err := New(Config{BaseURL: service.URL + "/api/v1/namespaces/management/services/pocketid:1411/proxy/", ApiKey: "private-management-key", HttpClient: service.Client()})
	is.NoErr(err)
	_, err = client.HasPasskey(t.Context(), "b7bd858a-95b7-4a24-bb17-c47c9126e320")
	is.True(err != nil)
	is.Equal(leaked.Load(), int64(0))
}

// TestPocketIdRejectsIdentityConflicts ensures retries cannot take over an unrelated user or OIDC client.
func TestPocketIdRejectsIdentityConflicts(t *testing.T) {
	admin := Admin{ID: "b7bd858a-95b7-4a24-bb17-c47c9126e320", Username: "owner", Email: "owner@example.com"}
	input := OidcClient{ID: "borq-test-client", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)}
	for _, test := range []struct {
		name string
		body string
		user bool
	}{
		{name: "another user", user: true, body: `{"id":"0c7a7d44-57bf-4e0d-8489-b4365de597fb","username":"owner","email":"owner@example.com","isAdmin":true}`},
		{name: "another email", user: true, body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"owner","email":"attacker@example.com","isAdmin":true}`},
		{name: "disabled admin", user: true, body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"owner","email":"owner@example.com","isAdmin":true,"disabled":true}`},
		{name: "non-admin", user: true, body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"owner","email":"owner@example.com","isAdmin":false}`},
		{name: "other callback", body: `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://attacker.example.com/callback"],"pkceEnabled":true,"logoutCallbackURLs":["https://borq.example.com/login"],"launchURL":"https://borq.example.com/"}`},
		{name: "extra callback", body: `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://borq.example.com/callback","https://attacker.example.com/callback"],"pkceEnabled":true,"logoutCallbackURLs":["https://borq.example.com/login"],"launchURL":"https://borq.example.com/"}`},
		{name: "public client", body: `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://borq.example.com/callback"],"pkceEnabled":true,"isPublic":true,"logoutCallbackURLs":["https://borq.example.com/login"],"launchURL":"https://borq.example.com/"}`},
		{name: "no PKCE", body: `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://borq.example.com/callback"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				is.Equal(request.Method, http.MethodGet)
				fmt.Fprint(writer, test.body)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			is.NoErr(err)
			if test.user {
				err = client.EnsureAdmin(t.Context(), admin)
			} else {
				err = client.EnsureClient(t.Context(), input)
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "does not match"))
		})
	}
}

// TestPocketIdRejectsInvalidInput prevents path traversal and wildcard callback registrations before sending credentials.
func TestPocketIdRejectsInvalidInput(t *testing.T) {
	is := is.New(t)
	var contacted atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { contacted.Add(1) }))
	defer service.Close()
	client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
	is.NoErr(err)
	for _, identifier := range []string{"", "../signup/setup", "00000000-0000-0000-0000-000000000000"} {
		is.True(client.EnsureAdmin(t.Context(), Admin{ID: identifier, Username: "owner", Email: "owner@example.com"}) != nil)
		_, err := client.NewOneTimeAccess(t.Context(), identifier)
		is.True(err != nil)
	}
	for _, input := range []OidcClient{
		{ID: "../other", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)},
		{ID: "borq", Name: "Borq", CallbackURL: "https://borq.example.com/*", Secret: strings.Repeat("s", 32)},
		{ID: "borq", Name: "Borq", CallbackURL: "http://borq.example.com/callback", Secret: strings.Repeat("s", 32)},
		{ID: "borq", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: "short"},
		{ID: "borq", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: "secret-with-line\nbreak"},
	} {
		is.True(client.EnsureClient(t.Context(), input) != nil)
		is.True(client.VerifyClient(t.Context(), input) != nil)
	}
	is.Equal(contacted.Load(), int64(0))
}

// TestPocketIdProvisioningFailuresArePrivate prevents provider-controlled bodies and request errors leaking secrets.
func TestPocketIdProvisioningFailuresArePrivate(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "rejected", status: http.StatusForbidden, body: `{"error":"private-api-key private-client-secret"}`},
		{name: "malformed", status: http.StatusOK, body: `private-api-key private-client-secret`},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat(" ", 1<<20) + `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, test.body)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: "private-api-key-123456"})
			is.NoErr(err)
			_, err = client.HasPasskey(t.Context(), "b7bd858a-95b7-4a24-bb17-c47c9126e320")
			is.True(err != nil)
			is.True(!strings.Contains(err.Error(), "private-"))
		})
	}
	is := is.New(t)
	client, err := New(Config{BaseURL: "https://example.com", ApiKey: "private-api-key-123456"})
	is.NoErr(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.HasPasskey(ctx, "b7bd858a-95b7-4a24-bb17-c47c9126e320")
	is.True(err != nil)
	is.True(!strings.Contains(err.Error(), "private-"))
}

// TestPocketIdEnrollmentGuard permits one-time enrollment only before a passkey exists and validates the returned code.
func TestPocketIdEnrollmentGuard(t *testing.T) {
	for _, test := range []struct {
		name        string
		credentials string
		token       string
		wantError   string
	}{
		{name: "already enrolled", credentials: `[{"id":"a-passkey"}]`, wantError: "already complete"},
		{name: "malformed token", credentials: `[]`, token: "../evil/path/", wantError: "invalid enrollment token"},
		{name: "short token", credentials: `[]`, token: "abc", wantError: "invalid enrollment token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					fmt.Fprint(writer, test.credentials)
					return
				}
				is.True(test.token != "")
				json.NewEncoder(writer).Encode(map[string]string{"token": test.token})
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			is.NoErr(err)
			_, err = client.NewOneTimeAccess(t.Context(), "b7bd858a-95b7-4a24-bb17-c47c9126e320")
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), test.wantError))
			if test.name == "already enrolled" {
				is.True(errors.Is(err, ErrAlreadyEnrolled))
			}
		})
	}
}

// TestPocketIdSetupRemainsPrivate refuses a real administrator response if first-user signup remains open.
func TestPocketIdSetupRemainsPrivate(t *testing.T) {
	is := is.New(t)
	service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/users" {
			fmt.Fprint(writer, `{"data":[{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320"}],"pagination":{"totalItems":1,"totalPages":1,"currentPage":1}}`)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/signup/setup") {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(writer, `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"owner","email":"owner@example.com","isAdmin":true}`)
	}))
	defer service.Close()
	client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
	is.NoErr(err)
	err = client.EnsureAdmin(t.Context(), Admin{ID: "b7bd858a-95b7-4a24-bb17-c47c9126e320", Username: "owner", Email: "owner@example.com"})
	is.Equal(err, errors.New("Pocket ID has not closed initial administrator setup"))
}

// TestPocketIdCredentialCheckFailsClosed does not add secrets when the provider cannot authenticate the saved credential reliably.
func TestPocketIdCredentialCheckFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `{"error":"invalid_grant"}`},
		{name: "unexpected success", status: http.StatusOK, body: `{"error":"invalid_grant"}`},
		{name: "malformed error", status: http.StatusBadRequest, body: `private-client-secret`},
		{name: "unrecognized error", status: http.StatusBadRequest, body: `{"error":"private-client-secret"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/oidc/clients/borq-test-client" && request.Method == http.MethodGet {
					fmt.Fprint(writer, `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://borq.example.com/callback"],"pkceEnabled":true,"logoutCallbackURLs":["https://borq.example.com/login"],"launchURL":"https://borq.example.com/"}`)
					return
				}
				is.Equal(request.URL.Path, "/api/oidc/token")
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, test.body)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			is.NoErr(err)
			err = client.EnsureClient(t.Context(), OidcClient{ID: "borq-test-client", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)})
			is.True(err != nil)
			is.True(!strings.Contains(err.Error(), "private-client-secret"))
		})
	}
}

// TestPocketIdRecoversInterruptedCreates verifies that an HTTP response loss never leads to adopting a different ID or rotating the saved secret.
func TestPocketIdRecoversInterruptedCreates(t *testing.T) {
	is := is.New(t)
	admin := Admin{ID: "b7bd858a-95b7-4a24-bb17-c47c9126e320", Username: "owner", Email: "owner@example.com"}
	input := OidcClient{ID: "borq-test-client", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)}
	var userCreated, clientCreated, secretCreated atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path != "/api/oidc/token" {
			writes.Add(1)
			switch request.URL.Path {
			case "/api/users":
				userCreated.Store(true)
			case "/api/oidc/clients":
				clientCreated.Store(true)
			case "/api/oidc/clients/borq-test-client/secrets":
				secretCreated.Store(true)
			default:
				t.Errorf("unexpected mutation path %s", request.URL.Path)
			}
			connection, _, err := writer.(http.Hijacker).Hijack()
			is.NoErr(err)
			connection.Close()
			return
		}
		switch request.URL.Path {
		case "/api/users":
			users := []userResponse{{ID: "00000000-0000-0000-0000-000000000000"}}
			if userCreated.Load() {
				users = append(users, userResponse{ID: admin.ID})
			}
			json.NewEncoder(writer).Encode(map[string]any{"data": users, "pagination": map[string]int{"totalItems": len(users), "totalPages": 1, "currentPage": 1}})
		case "/api/users/" + admin.ID:
			if !userCreated.Load() {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(writer, `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"owner","email":"owner@example.com","isAdmin":true}`)
		case "/api/signup/setup":
			writer.WriteHeader(http.StatusNotFound)
		case "/api/oidc/clients/borq-test-client":
			if !clientCreated.Load() {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(writer, `{"id":"borq-test-client","name":"Borq","callbackURLs":["https://borq.example.com/callback"],"pkceEnabled":true,"logoutCallbackURLs":["https://borq.example.com/login"],"launchURL":"https://borq.example.com/"}`)
		case "/api/oidc/token":
			if !secretCreated.Load() {
				writer.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(writer, `{"error":"invalid_client"}`)
				return
			}
			writer.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(writer, `{"error":"invalid_grant"}`)
		default:
			t.Errorf("unexpected read path %s", request.URL.Path)
		}
	}))
	defer service.Close()
	client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
	is.NoErr(err)
	is.NoErr(client.EnsureAdmin(t.Context(), admin))
	is.NoErr(client.EnsureClient(t.Context(), input))
	is.Equal(writes.Load(), int64(3))
}

// TestPocketIdRejectsUnexpectedInitialUsers catches prior or concurrent setup takeover, including retries after an interrupted bootstrap.
func TestPocketIdRejectsUnexpectedInitialUsers(t *testing.T) {
	admin := Admin{ID: "b7bd858a-95b7-4a24-bb17-c47c9126e320", Username: "owner", Email: "owner@example.com"}
	for _, test := range []struct {
		name             string
		existingAdmin    bool
		unexpectedBefore bool
		truncatedList    bool
		wantCreates      int64
		wantError        bool
	}{
		{name: "prior takeover", unexpectedBefore: true, wantError: true},
		{name: "concurrent takeover", wantCreates: 1, wantError: true},
		{name: "truncated users", truncatedList: true, wantError: true},
		{name: "retry after takeover", existingAdmin: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			var creates atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.URL.Path == "/api/users" && request.Method == http.MethodGet:
					users := []userResponse{{ID: "00000000-0000-0000-0000-000000000000"}}
					if creates.Load() > 0 || test.existingAdmin {
						users = append(users, userResponse{ID: admin.ID})
					}
					if test.unexpectedBefore || creates.Load() > 0 || test.existingAdmin {
						users = append(users, userResponse{ID: "4de6d8eb-c25c-447d-8754-410a2fd33a1d", Username: "unexpected-admin", IsAdmin: true})
					}
					count := len(users)
					if test.truncatedList {
						count = 10
					}
					json.NewEncoder(writer).Encode(map[string]any{"data": users, "pagination": map[string]int{"totalItems": count, "totalPages": 1, "currentPage": 1}})
				case request.URL.Path == "/api/signup/setup":
					writer.WriteHeader(http.StatusNotFound)
				case request.Method == http.MethodPost && request.URL.Path == "/api/users":
					creates.Add(1)
					writer.WriteHeader(http.StatusCreated)
					json.NewEncoder(writer).Encode(userResponse{ID: admin.ID, Username: admin.Username, Email: admin.Email, IsAdmin: true})
				case request.URL.Path == "/api/users/"+admin.ID:
					if !test.existingAdmin && creates.Load() == 0 {
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					json.NewEncoder(writer).Encode(userResponse{ID: admin.ID, Username: admin.Username, Email: admin.Email, IsAdmin: true})
				default:
					t.Errorf("unexpected Pocket ID endpoint %s", request.URL.Path)
				}
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			is.NoErr(err)
			err = client.EnsureAdmin(t.Context(), admin)
			if test.wantError {
				is.True(err != nil)
				is.True(strings.Contains(err.Error(), "unexpected users"))
			} else {
				is.NoErr(err)
			}
			is.Equal(creates.Load(), test.wantCreates)
		})
	}
}

// TestPocketIdClientLinksPreserveSettings ensures a metadata backfill preserves
// administrator settings, credentials, group restrictions, and additional URLs.
func TestPocketIdClientLinksPreserveSettings(t *testing.T) {
	for _, launchURL := range []string{"", "https://custom.example.com/"} {
		t.Run(launchURL, func(t *testing.T) {
			assert := is.New(t)
			input := OidcClient{ID: "borq-test-client", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/"}
			existing := clientResponse{ID: input.ID, Name: "Admin display name", CallbackURLs: []string{input.CallbackURL}, PkceEnabled: true,
				LogoutCallbackURLs: []string{"https://custom.example.com/logout"}, LaunchURL: launchURL, Description: "Keep this description",
				BackchannelLogoutURL: "https://custom.example.com/backchannel", RequiresReauthentication: true, RequiresPushedAuthorizationRequests: true,
				SkipConsent: true, Credentials: json.RawMessage(`{"federatedIdentities":[{"issuer":"https://issuer.example.com","subject":"admin","audience":"audience","jwks":"https://issuer.example.com/keys","replayProtection":true}],"secrets":[{"id":"saved-secret","isActive":true}]}`),
				HasLogo: true, HasDarkLogo: true, IsGroupRestricted: true, AccessTokenDurationMinutes: 15, RefreshTokenDurationMinutes: 90,
			}
			var writes int
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/oidc/clients/"+input.ID)
				if request.Method == http.MethodGet {
					assert.NoErr(json.NewEncoder(writer).Encode(existing))
					return
				}
				assert.Equal(request.Method, http.MethodPut)
				writes++
				var settings map[string]json.RawMessage
				assert.NoErr(json.NewDecoder(request.Body).Decode(&settings))
				originalJSON, err := json.Marshal(existing)
				assert.NoErr(err)
				var original map[string]json.RawMessage
				assert.NoErr(json.Unmarshal(originalJSON, &original))
				for key, value := range original {
					if key == "id" || key == "launchURL" || key == "logoutCallbackURLs" {
						continue
					}
					var before, after any
					assert.NoErr(json.Unmarshal(value, &before))
					assert.NoErr(json.Unmarshal(settings[key], &after))
					if !reflect.DeepEqual(before, after) {
						t.Errorf("setting %s changed during metadata update", key)
					}
				}
				assert.NoErr(json.Unmarshal(settings["launchURL"], &existing.LaunchURL))
				assert.NoErr(json.Unmarshal(settings["logoutCallbackURLs"], &existing.LogoutCallbackURLs))
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			assert.NoErr(err)
			assert.NoErr(client.EnsureClientLinks(t.Context(), input))
			assert.NoErr(client.EnsureClientLinks(t.Context(), input))
			assert.Equal(writes, 1)
			if launchURL == "" {
				assert.Equal(existing.LaunchURL, input.LaunchURL)
			} else {
				assert.Equal(existing.LaunchURL, launchURL)
			}
			assert.True(slices.Contains(existing.LogoutCallbackURLs, "https://custom.example.com/logout"))
			assert.True(slices.Contains(existing.LogoutCallbackURLs, input.LogoutCallbackURL))
		})
	}
}

// TestPocketIdClientLinksFailClosed keeps upgrade backfills from creating missing
// clients, adopting another callback, or trusting an unpersisted provider response.
func TestPocketIdClientLinksFailClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		missing     bool
		altered     bool
		ignored     bool
		interrupted bool
	}{
		{name: "missing client", missing: true},
		{name: "different callback", altered: true},
		{name: "unpersisted update", ignored: true},
		{name: "interrupted update", interrupted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			input := OidcClient{ID: "borq-test-client", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/"}
			existing := clientResponse{ID: input.ID, Name: "Borq", CallbackURLs: []string{input.CallbackURL}, PkceEnabled: true}
			if test.altered {
				existing.CallbackURLs = []string{"https://other.example.com/callback"}
			}
			var writes int
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/oidc/clients/"+input.ID)
				if request.Method == http.MethodGet {
					if test.missing {
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					assert.NoErr(json.NewEncoder(writer).Encode(existing))
					return
				}
				assert.Equal(request.Method, http.MethodPut)
				writes++
				if !test.ignored {
					var updated clientUpdate
					assert.NoErr(json.NewDecoder(request.Body).Decode(&updated))
					existing.LaunchURL, existing.LogoutCallbackURLs = updated.LaunchURL, updated.LogoutCallbackURLs
				}
				if test.interrupted {
					connection, _, err := writer.(http.Hijacker).Hijack()
					assert.NoErr(err)
					assert.NoErr(connection.Close())
					return
				}
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			assert.NoErr(err)
			err = client.EnsureClientLinks(t.Context(), input)
			if test.interrupted {
				assert.NoErr(err)
			} else {
				assert.True(err != nil)
			}
			if test.missing || test.altered {
				assert.Equal(writes, 0)
			} else {
				assert.Equal(writes, 1)
			}
		})
	}
}

// TestPocketIdClientLinksValidateDestinations rejects unsafe metadata before any
// client read, including the secret-free ready-instance upgrade path.
func TestPocketIdClientLinksValidateDestinations(t *testing.T) {
	assert := is.New(t)
	var contacted atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { contacted.Add(1) }))
	defer service.Close()
	client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
	assert.NoErr(err)
	for _, address := range []string{"", "http://public.example.com/", "https://example.com/*", "https://example.com/?redirect=elsewhere", "https://user:pass@example.com/"} {
		input := OidcClient{ID: "borq-test-client", Name: "Borq", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: address, LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)}
		err := client.EnsureClient(t.Context(), input)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "destinations"))
		input.LogoutCallbackURL, input.LaunchURL = input.LaunchURL, address
		err = client.EnsureClientLinks(t.Context(), input)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "destinations"))
		err = client.VerifyClient(t.Context(), input)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "destinations"))
	}
	assert.Equal(contacted.Load(), int64(0))
}

// TestPocketIdVerifyClientContinuity keeps ready recovery read-only and fails
// closed when the persisted client, exact callback, or durable secret is missing.
func TestPocketIdVerifyClientContinuity(t *testing.T) {
	for _, test := range []struct {
		name                                        string
		missing, mismatch, wrongSecret, unavailable bool
	}{
		{name: "owned renamed client"}, {name: "missing client", missing: true}, {name: "changed callback", mismatch: true},
		{name: "wrong durable secret", wrongSecret: true}, {name: "unavailable credential check", unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			input := OidcClient{ID: "borq-test-client", CallbackURL: "https://borq.example.com/callback", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: strings.Repeat("s", 32)}
			if test.wrongSecret {
				input.Secret = strings.Repeat("x", 32)
			}
			var probes int
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/oidc/clients/"+input.ID {
					if test.missing {
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					existing := clientResponse{ID: input.ID, Name: "Administrator renamed client", CallbackURLs: []string{input.CallbackURL}, PkceEnabled: true}
					if test.mismatch {
						existing.CallbackURLs = []string{"https://other.example.com/callback"}
					}
					assert.NoErr(json.NewEncoder(writer).Encode(existing))
					return
				}
				if request.Method == http.MethodPost && request.URL.Path == "/api/oidc/token" {
					probes++
					assert.NoErr(request.ParseForm())
					assert.Equal(request.Form.Get("client_id"), input.ID)
					assert.Equal(request.Form.Get("redirect_uri"), input.CallbackURL)
					assert.Equal(request.Form.Get("grant_type"), "authorization_code")
					if test.unavailable {
						writer.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprint(writer, `{"error":"invalid_grant"}`)
						return
					}
					if request.Form.Get("client_secret") != strings.Repeat("s", 32) {
						writer.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(writer, `{"error":"invalid_client"}`)
						return
					}
					writer.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(writer, `{"error":"invalid_grant"}`)
					return
				}
				t.Errorf("continuity verification attempted an unexpected provider mutation or endpoint: %s %s", request.Method, request.URL.Path)
				writer.WriteHeader(http.StatusMethodNotAllowed)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			assert.NoErr(err)
			err = client.VerifyClient(t.Context(), input)
			if test.missing || test.mismatch || test.wrongSecret || test.unavailable {
				assert.True(err != nil)
			} else {
				assert.NoErr(err)
			}
			if test.missing || test.mismatch {
				assert.Equal(probes, 0)
			} else {
				assert.Equal(probes, 1)
			}
		})
	}
}

// TestPocketIdVerifyRetainedAdmin allows profile edits while refusing a missing, disabled, or replaced administrator.
func TestPocketIdVerifyRetainedAdmin(t *testing.T) {
	id := "b7bd858a-95b7-4a24-bb17-c47c9126e320"
	for _, test := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{name: "renamed profile", body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","username":"renamed","email":"changed@example.test","isAdmin":true}`, valid: true},
		{name: "missing", status: http.StatusNotFound},
		{name: "another identity", body: `{"id":"0c7a7d44-57bf-4e0d-8489-b4365de597fb","isAdmin":true}`},
		{name: "disabled", body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","isAdmin":true,"disabled":true}`},
		{name: "demoted", body: `{"id":"b7bd858a-95b7-4a24-bb17-c47c9126e320","isAdmin":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/users/"+id)
				if test.status != 0 {
					writer.WriteHeader(test.status)
				}
				fmt.Fprint(writer, test.body)
			}))
			defer service.Close()
			client, err := New(Config{BaseURL: service.URL, ApiKey: strings.Repeat("k", 32)})
			assert.NoErr(err)
			assert.Equal(client.VerifyAdmin(t.Context(), id) == nil, test.valid)
		})
	}
}
