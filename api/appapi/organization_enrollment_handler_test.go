package appapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestAdministratorEnrollmentAuthorization reserves provider enrollment and initial OIDC identity selection for installation administrators.
func TestAdministratorEnrollmentAuthorization(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	dispatched := 0
	transport, err := NewAuthorizationHandler(manager, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dispatched++
		writer.WriteHeader(http.StatusNoContent)
	}))
	assert.NoErr(err)
	for _, path := range []string{"/pocketid/admin-enrollment", "/oidc/administrator"} {
		for _, test := range []struct {
			name          string
			principal     apps.Principal
			authenticated bool
			status        int
		}{
			{"anonymous", apps.Principal{}, false, http.StatusUnauthorized},
			{"organization administrator", apps.Principal{Permissions: []apps.Permission{"*"}, Login: apps.OrganizationLogin{Organization: "team"}}, true, http.StatusForbidden},
			{"management administrator", apps.Principal{Admin: true}, true, http.StatusNoContent},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert := is.New(t)
				request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations/team"+path, nil)
				if test.authenticated {
					request = request.WithContext(apps.WithPrincipal(request.Context(), test.principal))
				}
				response := httptest.NewRecorder()
				transport.ServeHTTP(response, request)
				assert.Equal(response.Code, test.status)
			})
		}
	}
	assert.Equal(dispatched, 2)
}

// TestPocketIdEnrollmentRoute verifies unavailable and invalid enrollment requests retain cache and referrer protection through the generated API transport.
func TestPocketIdEnrollmentRoute(t *testing.T) {
	assert := is.New(t)
	manager, err := apps.NewManager(t.Context(), apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	transport, err := NewHandler(manager)
	assert.NoErr(err)
	for _, test := range []struct {
		name   string
		status int
	}{{"team", http.StatusNotFound}, {"missing", http.StatusNotFound}, {"INVALID", http.StatusBadRequest}} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations/"+test.name+"/pocketid/admin-enrollment", nil)
		response := httptest.NewRecorder()
		transport.ServeHTTP(response, request)
		assert.Equal(response.Code, test.status)
		if test.name != "INVALID" {
			assert.Equal(response.Header().Get("Cache-Control"), "no-store, private")
			assert.Equal(response.Header().Get("Referrer-Policy"), "no-referrer")
		}
	}
}
