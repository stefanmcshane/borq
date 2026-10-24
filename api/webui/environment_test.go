package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"golang.org/x/net/html"
)

// TestEnvironmentPublicationForms owns the browser-to-API translation: either
// editor publishes literal values with the version guard kept outside the YAML.
func TestEnvironmentPublicationForms(t *testing.T) {
	for _, format := range []string{"fields", "yaml"} {
		t.Run(format, func(t *testing.T) {
			assert := is.New(t)
			var published appapi.PublishEnvironmentGroupInput
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPost)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/environment-groups/web-config/versions")
				assert.NoErr(json.NewDecoder(request.Body).Decode(&published))
				_, _ = writer.Write([]byte(`{"name":"web-config","version":4,"values":[]}`))
			}))
			assert.NoErr(err)
			form := url.Values{"action": {"publish-group"}, "organization": {"team"}, "space": {"dev"}, "view": {"environment"}, "name": {"web-config"}, "format": {format}, "baseVersion": {"3"},
				"variableName": {"LOG_LEVEL", "EMPTY", "MULTILINE"}, "variableValue": {"info", "", "first\nsecond\n"},
				"values": {"- name: LOG_LEVEL\n  value: info\n- name: EMPTY\n  value: \"\"\n- name: MULTILINE\n  value: |\n    first\n    second\n"}}
			response := postForm(web, form, false)
			assert.Equal(response.Code, http.StatusSeeOther)
			location, err := url.Parse(response.Header().Get("Location"))
			assert.NoErr(err)
			assert.Equal(location.Query().Get("view"), "environment-group")
			assert.Equal(location.Query().Get("group"), "web-config")
			assert.Equal(published.Values, []appapi.Variable{{Name: "LOG_LEVEL", Value: "info"}, {Name: "EMPTY", Value: ""}, {Name: "MULTILINE", Value: "first\nsecond\n"}})
			assert.True(published.BaseVersion != nil)
			assert.Equal(*published.BaseVersion, 3)
		})
	}
}

// TestEnvironmentEditorConversion preserves unsaved strings when switching entry
// modes, without publishing or exposing the publication wrapper in the textarea.
func TestEnvironmentEditorConversion(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("switching editors must not publish or fetch sensitive values")
	}))
	assert.NoErr(err)
	form := url.Values{"format": {"fields"}, "editor": {"yaml"}, "baseVersion": {"3"}, "variableName": {"TOKEN", "EMPTY"}, "variableValue": {"\n<private>&\"\r\nsecond", ""}}
	request := httptest.NewRequest(http.MethodPost, "/ui/environment-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.True(strings.Contains(response.Body.String(), `name="format" value="yaml"`))
	assert.True(!strings.Contains(response.Body.String(), "baseVersion:"))
	assert.True(!strings.Contains(response.Body.String(), "values:"))
	assert.True(strings.Contains(response.Body.String(), "&lt;private&gt;"))
	document, err := html.Parse(strings.NewReader(response.Body.String()))
	assert.NoErr(err)
	var source string
	for node := range document.Descendants() {
		if node.Data == "textarea" && node.FirstChild != nil {
			source = node.FirstChild.Data
		}
	}
	form = url.Values{"format": {"yaml"}, "editor": {"fields"}, "baseVersion": {"3"}, "values": {source}}
	request = httptest.NewRequest(http.MethodPost, "/ui/environment-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	document, err = html.Parse(strings.NewReader(response.Body.String()))
	assert.NoErr(err)
	var literals []string
	for node := range document.Descendants() {
		if node.Data == "textarea" && node.Parent.Data == "label" && node.FirstChild != nil {
			literals = append(literals, node.FirstChild.Data)
		}
	}
	assert.Equal(literals, []string{"\n<private>&\"\nsecond"})
}

// TestEnvironmentFormValidation rejects malformed input before it can publish,
// keeping YAML errors and missing version guards visible without echoing values.
func TestEnvironmentFormValidation(t *testing.T) {
	for _, input := range []struct {
		name, source, message string
	}{
		{"wrapper", "values:\n- name: TOKEN\n  value: do-not-echo-secret", "YAML list"},
		{"null list", "null", "YAML list"},
		{"unknown field", "- name: TOKEN\n  value: do-not-echo-secret\n  extra: ignored", "YAML list"},
		{"missing value", "- name: TOKEN", "YAML list"},
		{"number", "- name: TOKEN\n  value: 42", "string value"},
		{"duplicate key", "- name: TOKEN\n  value: first\n  value: do-not-echo-secret", "unique keys"},
		{"multiple documents", "[]\n---\n[]", "one YAML document"},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input must not reach publication") }))
			assert.NoErr(err)
			response := postForm(web, url.Values{"action": {"publish-group"}, "format": {"yaml"}, "baseVersion": {"0"}, "values": {input.source}}, true)
			assert.Equal(response.Code, http.StatusBadRequest)
			assert.Equal(response.Header().Get("HX-Retarget"), "#notice")
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
	t.Run("missing version guard", func(t *testing.T) {
		assert := is.New(t)
		web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unguarded publication must not reach the API") }))
		assert.NoErr(err)
		response := postForm(web, url.Values{"action": {"publish-group"}, "format": {"yaml"}, "values": {"[]"}}, true)
		assert.Equal(response.Code, http.StatusBadRequest)
		assert.True(strings.Contains(response.Body.String(), "Reload this group"))
	})
}

// TestEnvironmentEditorWithoutJavascript preserves the draft and its version
// guard across a full-page form submission, rather than only an htmx fragment.
func TestEnvironmentEditorWithoutJavascript(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		switch request.URL.Path {
		case "/api/v1/organizations":
			_, _ = writer.Write([]byte(`[{"name":"team"}]`))
		case "/api/v1/organizations/team/spaces":
			_, _ = writer.Write([]byte(`[{"id":"space-id","organization":"team","name":"dev","cluster":"target"}]`))
		case "/api/v1/organizations/team/clusters", "/api/v1/organizations/team/spaces/dev/environment-groups":
			_, _ = writer.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	assert.NoErr(err)
	form := url.Values{"organization": {"team"}, "space": {"dev"}, "name": {"web-config"}, "format": {"fields"}, "editor": {"yaml"}, "baseVersion": {"3"}, "groupVersion": {"2"}, "variableName": {"LOG_LEVEL"}, "variableValue": {"info"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/environment-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), "<html"))
	assert.True(strings.Contains(response.Body.String(), `name="name" value="web-config"`))
	assert.True(strings.Contains(response.Body.String(), `name="baseVersion" value="3"`))
	assert.True(strings.Contains(response.Body.String(), `name="groupVersion" value="2"`))
	assert.True(strings.Contains(response.Body.String(), "Viewing version 2"))
	assert.True(strings.Contains(response.Body.String(), "- name: LOG_LEVEL\n  value: info"))
}

// TestEnvironmentGroupPages owns resource navigation, shared group identity,
// version readback, and the separation of list metadata from editable values.
func TestEnvironmentGroupPages(t *testing.T) {
	for _, input := range []struct {
		query, present, absent string
		status                 int
	}{
		{"view=environment", "Create an environment group", `name="baseVersion"`, 200},
		{"view=environment", "web-config", "private-current-value", 200},
		{"view=environment-group", "Create group", `name="sourceName"`, 200},
		{"view=environment-group&group=web-config", "Publish new version", "worker-private-value", 200},
		{"view=environment-group&group=web-config", "private-current-value", `name="sourceName"`, 200},
		{"view=environment&group=web-config", "<h1>web-config</h1>", "Create group", 200},
		{"view=environment-group&group=web-config&tab=versions", "Published versions", "private-current-value", 200},
		{"view=environment-group&group=web-config&version=3", "private-old-value", "private-current-value", 200},
		{"view=environment-group&group=web-config&version=3", `name="baseVersion" value="4"`, "worker-private-value", 200},
		{"view=environment-group&group=web-config&tab=applications", ">web</a>", ">worker</a>", 200},
		{"view=environment-group&group=web-config&tab=settings", "Clone this group", "private-current-value", 200},
		{"view=environment-group&group=web-config&tab=settings", `name="sourceName" value="web-config"`, `<select class="select" name="sourceName"`, 200},
		{"view=environment-group&group=missing", "group was not found", "private-current-value", 404},
		{"view=environment-group&group=web-config&tab=missing", "page was not found", "private-current-value", 404},
		{"view=environment-group&tab=settings", "Create an environment group before", "Clone this group", 404},
	} {
		t.Run(input.query+input.present, func(t *testing.T) {
			assert := is.New(t)
			web := environmentPageTestHandler(t)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?organization=team&space=dev&"+input.query, nil))
			assert.Equal(response.Code, input.status)
			assert.True(strings.Contains(response.Body.String(), input.present))
			assert.True(!strings.Contains(response.Body.String(), input.absent))
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			selection, err := url.ParseQuery(input.query)
			assert.NoErr(err)
			if input.status != http.StatusOK || selection.Get("group") == "" {
				return
			}
			document, err := html.Parse(strings.NewReader(response.Body.String()))
			assert.NoErr(err)
			var groupDetails []string
			for node := range document.Descendants() {
				for _, attribute := range node.Attr {
					if attribute.Key == "aria-label" && attribute.Val == "Environment group details" {
						groupDetails = append(groupDetails, navigationText(node))
					}
				}
			}
			assert.Equal(groupDetails, []string{"web-config Environment group · Latest v4 · Used by 1 application"})
		})
	}
}

// TestEnvironmentCloneNavigation keeps the source fixed to the selected resource
// and opens the newly created group after cloning a specific immutable version.
func TestEnvironmentCloneNavigation(t *testing.T) {
	assert := is.New(t)
	var cloned appapi.CloneEnvironmentGroupInput
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/environment-groups/copy/clone")
		assert.NoErr(json.NewDecoder(request.Body).Decode(&cloned))
		_, _ = writer.Write([]byte(`{"name":"copy","version":1,"values":[]}`))
	}))
	assert.NoErr(err)
	form := url.Values{"action": {"clone-group"}, "organization": {"team"}, "space": {"dev"}, "name": {"copy"}, "sourceName": {"web-config"}, "sourceVersion": {"3"}}
	response := postForm(web, form, false)
	assert.Equal(response.Code, http.StatusSeeOther)
	assert.Equal(cloned.SourceName, "web-config")
	assert.Equal(*cloned.SourceVersion, "3")
	location, err := url.Parse(response.Header().Get("Location"))
	assert.NoErr(err)
	assert.Equal(location.Query().Get("view"), "environment-group")
	assert.Equal(location.Query().Get("group"), "copy")
}

func environmentPageTestHandler(t *testing.T) http.Handler {
	t.Helper()
	assert := is.New(t)
	groups := []appapi.EnvironmentGroup{
		{Name: "web-config", Version: 4, Values: []appapi.Variable{{Name: "LABEL", Value: "private-current-value"}}},
		{Name: "worker-config", Version: 2, Values: []appapi.Variable{{Name: "TOKEN", Value: "worker-private-value"}}},
	}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{{ID: "dev-id", Name: "dev", Organization: "team", Cluster: "target", Namespace: "borq-dev", Configuration: appapi.Configuration{Apps: []appapi.App{
				{Name: "web", Image: "web-image", Env: appapi.EnvReference{Name: "web-config", Version: "3"}},
				{Name: "worker", Image: "worker-image", Env: appapi.EnvReference{Name: "worker-config", Version: "2"}},
			}}}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/organizations/team/spaces/dev/environment-groups":
			output = groups
		case "/api/v1/organizations/team/spaces/dev/environment-groups/web-config":
			output = groups[0]
		case "/api/v1/organizations/team/spaces/dev/environment-groups/web-config/versions/3":
			output = appapi.EnvironmentGroup{Name: "web-config", Version: 3, Values: []appapi.Variable{{Name: "LABEL", Value: "private-old-value"}}}
		case "/api/v1/organizations/team/spaces/dev/environment-groups/missing":
			writer.WriteHeader(http.StatusNotFound)
			output = appapi.Problem{Message: "The environment group was not found."}
		default:
			t.Errorf("unexpected API request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	return web
}
