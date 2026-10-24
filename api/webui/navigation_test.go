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
	"golang.org/x/net/html"
)

// TestLastSpaceNavigation follows browser links through organization-wide pages
// and keeps each organization's last valid space for full and htmx navigation.
func TestLastSpaceNavigation(t *testing.T) {
	for _, mode := range []string{"document", "htmx"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			spaces := []appapi.Space{{ID: "dev-id", Name: "dev"}, {ID: "prod-id", Name: "prod"}}
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch {
				case request.URL.Path == "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}, {Name: "other"}}
				case strings.HasSuffix(request.URL.Path, "/spaces"):
					output = spaces
				case strings.HasSuffix(request.URL.Path, "/clusters"):
					output = []appapi.Cluster{}
				case strings.HasSuffix(request.URL.Path, "/apps"):
					output = appapi.ApplicationState{}
				case strings.HasSuffix(request.URL.Path, "/custom-charts"):
					output = []appapi.CustomChart{}
				default:
					t.Errorf("unexpected API request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				assert.NoErr(json.NewEncoder(writer).Encode(output))
			}))
			assert.NoErr(err)
			jar, err := cookiejar.New(nil)
			assert.NoErr(err)
			links := map[string]string{}
			for _, step := range []struct {
				path, link, location string
				removeSpace          bool
			}{
				{path: "/?organization=team&view=applications", location: "/?organization=team&space=dev&view=applications"},
				{path: "/?organization=team&space=prod&view=applications", location: "/?organization=team&space=prod&view=applications"},
				{link: "team", location: "/?organization=team&view=overview"},
				{link: "Applications", location: "/?organization=team&space=prod&view=applications"},
				{link: "Spaces", location: "/?organization=team&view=spaces"},
				{link: "Environment groups", location: "/?organization=team&space=prod&view=environment"},
				{link: "Custom addons", location: "/?organization=team&space=prod&view=custom-charts"},
				{link: "other", location: "/?organization=other&view=overview"},
				{link: "Applications", location: "/?organization=other&space=dev&view=applications"},
				{link: "team", location: "/?organization=team&view=overview"},
				{link: "Applications", location: "/?organization=team&space=prod&view=applications"},
				{path: "/?organization=team&space=dev&view=applications", location: "/?organization=team&space=dev&view=applications"},
				{link: "team", location: "/?organization=team&view=overview"},
				{link: "Applications", location: "/?organization=team&space=dev&view=applications"},
				{path: "/?organization=team&space=prod&view=applications", location: "/?organization=team&space=prod&view=applications"},
				{path: "/?organization=team&view=applications", location: "/?organization=team&space=dev&view=applications", removeSpace: true},
			} {
				if step.link != "" {
					step.path = links[step.link]
					if step.path == "" {
						t.Fatalf("navigation link %q is missing", step.link)
					}
				}
				// Removing the remembered space must restore the first available one.
				if step.removeSpace {
					spaces = spaces[:1]
				}
				request := httptest.NewRequest(http.MethodGet, "https://borq.test"+step.path, nil)
				if mode == "htmx" {
					request.Header.Set("HX-Request", "true")
				}
				for _, cookie := range jar.Cookies(request.URL) {
					request.AddCookie(cookie)
				}
				response := httptest.NewRecorder()
				web.ServeHTTP(response, request)
				assert.Equal(response.Code, http.StatusOK)
				jar.SetCookies(request.URL, response.Result().Cookies())
				if mode == "htmx" {
					assert.Equal(response.Header().Get("HX-Push-Url"), step.location)
				}
				document, err := html.Parse(response.Body)
				assert.NoErr(err)
				links = map[string]string{}
				location := ""
				for node := range document.Descendants() {
					attributes := map[string]string{}
					for _, attribute := range node.Attr {
						attributes[attribute.Key] = attribute.Val
					}
					if node.Data == "a" {
						links[navigationText(node)] = attributes["href"]
					}
					if attributes["id"] == "workspace" {
						location = attributes["hx-get"]
					}
				}
				if location != step.location {
					t.Fatalf("navigation to %s: got %s, want %s", step.path, location, step.location)
				}
			}
		})
	}
}
