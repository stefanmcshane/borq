package webui

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestLastSpaceNavigation preserves each organization's remembered selection,
// isolates global pages, and falls back when a remembered space disappears.
func TestLastSpaceNavigation(t *testing.T) {
	assert := is.New(t)
	spaces := []appapi.Space{{ID: "dev-id", Name: "dev"}, {ID: "prod-id", Name: "prod"}}
	web, err := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var output any
		switch {
		case r.URL.Path == "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}, {Name: "other"}}
		case strings.HasSuffix(r.URL.Path, "/spaces"):
			output = spaces
		case strings.HasSuffix(r.URL.Path, "/clusters"):
			output = []appapi.Cluster{}
		case strings.HasSuffix(r.URL.Path, "/apps"):
			output = appapi.ApplicationState{}
		case strings.HasSuffix(r.URL.Path, "/custom-charts"):
			output = []appapi.CustomChart{}
		default:
			t.Fatalf("unexpected API request %s", r.URL.Path)
		}
		assert.NoErr(json.NewEncoder(w).Encode(output))
	}))
	assert.NoErr(err)
	jar, err := cookiejar.New(nil)
	assert.NoErr(err)
	for _, step := range []struct {
		query, location string
		remove          bool
	}{
		{"organization=team&view=applications", "/?organization=team&space=dev&view=applications", false},
		{"organization=team&space=prod&view=applications", "/?organization=team&space=prod&view=applications", false},
		{"organization=team&view=overview", "/?organization=team&view=overview", false},
		{"organization=team&view=applications", "/?organization=team&space=prod&view=applications", false},
		{"organization=team&view=spaces", "/?organization=team&view=spaces", false},
		{"organization=team&view=secrets", "/?organization=team&space=prod&view=secrets", false},
		{"organization=team&view=custom-charts", "/?organization=team&space=prod&view=custom-charts", false},
		{"organization=other&view=overview", "/?organization=other&view=overview", false},
		{"organization=other&view=applications", "/?organization=other&space=dev&view=applications", false},
		{"organization=team&view=applications", "/?organization=team&space=prod&view=applications", false},
		{"organization=team&space=dev&view=applications", "/?organization=team&space=dev&view=applications", false},
		{"organization=team&view=overview", "/?organization=team&view=overview", false},
		{"organization=team&view=applications", "/?organization=team&space=dev&view=applications", false},
		{"organization=team&space=prod&view=applications", "/?organization=team&space=prod&view=applications", false},
		{"organization=team&view=applications", "/?organization=team&space=dev&view=applications", true},
	} {
		if step.remove {
			spaces = spaces[:1]
		}
		request := httptest.NewRequest(http.MethodGet, "https://borq.test/api/ui/page?"+step.query, nil)
		for _, cookie := range jar.Cookies(request.URL) {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		web.ServeHTTP(response, request)
		page := decodeView[pageData](t, response, "workspace")
		assert.Equal(page.Location, step.location)
		jar.SetCookies(request.URL, response.Result().Cookies())
		for _, cookie := range response.Result().Cookies() {
			assert.Equal(cookie.Path, "/")
			assert.True(cookie.HttpOnly)
			assert.True(cookie.Secure)
			assert.Equal(cookie.SameSite, http.SameSiteLaxMode)
		}
	}
}
