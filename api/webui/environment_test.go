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
)

// TestEnvironmentPublicationForms owns the browser-to-API translation: either
// editor publishes literal values with the version guard kept outside the YAML.
func TestEnvironmentPublicationForms(t *testing.T) {
	for _, format := range []string{"fields", "yaml"} {
		t.Run(format, func(t *testing.T) {
			assert := is.New(t)
			var published appapi.PublishEnvironmentGroupInput
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if serveActionPageFixture(writer, request) {
					return
				}
				assert.Equal(request.Method, http.MethodPost)
				assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/environment-groups/web-config/versions")
				assert.NoErr(json.NewDecoder(request.Body).Decode(&published))
				_, _ = writer.Write([]byte(`{"name":"web-config","version":4,"values":[]}`))
			}))
			assert.NoErr(err)
			form := url.Values{"action": {"publish-group"}, "organization": {" team "}, "space": {" dev "}, "view": {"environment"}, "name": {" web-config "}, "format": {format}, "baseVersion": {" 3 "},
				"variableName": {" LOG_LEVEL ", "EMPTY", "MULTILINE"}, "variableValue": {" info ", "", "first\nsecond\n"},
				"values": {"- name: ' LOG_LEVEL '\n  value: ' info '\n- name: EMPTY\n  value: \"\"\n- name: MULTILINE\n  value: |\n    first\n    second\n"}}
			response := postForm(web, form)
			location, err := url.Parse(decodeView[pageData](t, response, "workspace").Location)
			assert.NoErr(err)
			assert.Equal(location.Query().Get("view"), "secret")
			assert.Equal(location.Query().Get("group"), "web-config")
			assert.Equal(published.Values, []appapi.Variable{{Name: "LOG_LEVEL", Value: " info "}, {Name: "EMPTY", Value: ""}, {Name: "MULTILINE", Value: "first\nsecond\n"}})
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
		t.Fatal("editor conversion must not publish or fetch values")
	}))
	assert.NoErr(err)
	form := url.Values{"format": {"fields"}, "editor": {"yaml"}, "baseVersion": {"3"}, "variableName": {"TOKEN", "EMPTY"}, "variableValue": {"\n<private>&\"\r\nsecond", ""}}
	response := postEnvironmentEditor(web, form)
	editor := decodeView[environmentEditorData](t, response, "environment-editor")
	assert.Equal(editor.Format, "yaml")
	assert.True(!strings.Contains(editor.Yaml, "baseVersion:"))
	assert.True(!strings.Contains(editor.Yaml, "values:"))
	form = url.Values{"format": {"yaml"}, "editor": {"fields"}, "baseVersion": {"3"}, "values": {editor.Yaml}}
	editor = decodeView[environmentEditorData](t, postEnvironmentEditor(web, form), "environment-editor")
	assert.Equal(editor.Format, "fields")
	assert.Equal(editor.Values, []appapi.Variable{{Name: "TOKEN", Value: "\n<private>&\"\nsecond"}, {Name: "EMPTY", Value: ""}})
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
			response := postForm(web, url.Values{"action": {"publish-group"}, "format": {"yaml"}, "baseVersion": {"0"}, "values": {input.source}})
			assert.Equal(response.Code, http.StatusBadRequest)
			var problem appapi.Problem
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
			assert.True(problem.Message != "")
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
	t.Run("missing version guard", func(t *testing.T) {
		assert := is.New(t)
		web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unguarded publication must not reach the API") }))
		assert.NoErr(err)
		response := postForm(web, url.Values{"action": {"publish-group"}, "format": {"yaml"}, "values": {"[]"}})
		assert.Equal(response.Code, http.StatusBadRequest)
		assert.True(strings.Contains(response.Body.String(), "Reload this group"))
	})
}

func postEnvironmentEditor(web http.Handler, form url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/ui/environment-editor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	web.ServeHTTP(response, request)
	return response
}

// TestEnvironmentGroupPages owns resource navigation, shared group identity,
// version readback, and the separation of list metadata from editable values.
func TestEnvironmentGroupPages(t *testing.T) {
	for _, input := range []struct {
		query, view, tab, value string
		version, latest, status int
	}{
		{"view=environment", "environment", "", "", 0, 0, 200},
		{"view=environment-group", "environment-group", "variables", "", 0, 0, 200},
		{"view=environment-group&group=web-config", "environment-group", "variables", "private-current-value", 4, 4, 200},
		{"view=environment&group=web-config", "environment-group", "variables", "private-current-value", 4, 4, 200},
		{"view=environment-group&group=web-config&tab=versions", "environment-group", "versions", "", 4, 4, 200},
		{"view=environment-group&group=web-config&version=3", "environment-group", "variables", "private-old-value", 3, 4, 200},
		{"view=environment-group&group=web-config&tab=applications", "environment-group", "applications", "", 4, 4, 200},
		{"view=environment-group&group=web-config&tab=settings", "environment-group", "settings", "", 4, 4, 200},
		{"view=environment-group&group=missing", "", "", "", 0, 0, 404},
		{"view=environment-group&group=web-config&tab=missing", "", "", "", 0, 0, 404},
		{"view=environment-group&tab=settings", "", "", "", 0, 0, 404},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			environmentPageTestHandler(t).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?organization=team&space=dev&"+input.query, nil))
			assert.Equal(response.Code, input.status)
			var page pageData
			if input.status == http.StatusOK {
				page = decodeView[pageData](t, response, "workspace")
			}
			if input.status != 200 {
				return
			}
			assert.Equal(page.View, input.view)
			assert.Equal(page.GroupTab, input.tab)
			assert.Equal(page.Group.Version, input.version)
			assert.Equal(page.GroupLatestVersion, input.latest)
			for _, group := range page.Groups {
				assert.Equal(group.VariableCount, 1)
			}
			assert.True(!strings.Contains(response.Body.String(), "group-application-private"))
			if input.value == "" {
				for _, secret := range []string{"private-current-value", "private-old-value", "worker-private-value", "managed-current-value"} {
					assert.True(!strings.Contains(response.Body.String(), secret))
				}
				assert.Equal(len(page.Group.Values), 0)
			}
			if input.value != "" {
				assert.Equal(page.EnvironmentEditor.Values, []appapi.Variable{{Name: "LABEL", Value: input.value}})
			} else {
				assert.Equal(len(page.EnvironmentEditor.Values), 0)
			}
			if input.tab == "applications" {
				assert.Equal(len(page.GroupApplications), 1)
				assert.Equal(page.GroupApplications[0].Name, "web")
			}
			if input.tab == "versions" {
				assert.Equal(page.GroupVersions, []int{4, 3, 2, 1})
			}
		})
	}
}

// TestEnvironmentCloneNavigation keeps the source fixed to the selected resource
// and opens the newly created group after cloning a specific immutable version.
func TestEnvironmentCloneNavigation(t *testing.T) {
	assert := is.New(t)
	var cloned appapi.CloneEnvironmentGroupInput
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveActionPageFixture(writer, request) {
			return
		}
		assert.Equal(request.Method, http.MethodPost)
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/environment-groups/copy/clone")
		assert.NoErr(json.NewDecoder(request.Body).Decode(&cloned))
		_, _ = writer.Write([]byte(`{"name":"copy","version":1,"values":[]}`))
	}))
	assert.NoErr(err)
	form := url.Values{"action": {"clone-group"}, "organization": {"team"}, "space": {"dev"}, "name": {"copy"}, "sourceName": {"web-config"}, "sourceVersion": {"3"}}
	response := postForm(web, form)
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(cloned.SourceName, "web-config")
	assert.Equal(*cloned.SourceVersion, "3")
	location, err := url.Parse(decodeView[pageData](t, response, "workspace").Location)
	assert.NoErr(err)
	assert.Equal(location.Query().Get("view"), "secret")
	assert.Equal(location.Query().Get("group"), "copy")
}

// TestManagedEnvironmentGroupPages keeps generated values inspectable while
// reserving publication for the database and preserving the independent clone flow.
func TestManagedEnvironmentGroupPages(t *testing.T) {
	for _, input := range []struct {
		query, value string
		version      int
	}{
		{"view=environment", "", 0},
		{"view=environment-group&group=postgres-database-immutable-id", "managed-current-value", 2},
		{"view=environment-group&group=postgres-database-immutable-id&tab=versions", "", 2},
		{"view=environment-group&group=postgres-database-immutable-id&tab=settings", "", 2},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			page := requestPage(t, environmentPageTestHandler(t), "organization=team&space=dev&"+input.query, http.StatusOK)
			if input.version == 0 {
				assert.Equal(page.Groups[2].ManagedBy, "postgres/database")
				return
			}
			assert.Equal(page.Group.ManagedBy, "postgres/database")
			assert.Equal(page.Group.Version, input.version)
			assert.Equal(page.GroupLatestVersion, 2)
			assert.Equal(page.GroupVersions, []int{2})
			if input.value != "" {
				assert.Equal(page.EnvironmentEditor.Values, []appapi.Variable{{Name: "PGPASSWORD", Value: input.value}})
			} else {
				assert.Equal(len(page.EnvironmentEditor.Values), 0)
			}
		})
	}
	requestPage(t, environmentPageTestHandler(t), "organization=team&space=dev&view=environment-group&group=postgres-database-immutable-id&version=1", http.StatusNotFound)
}

func environmentPageTestHandler(t *testing.T) http.Handler {
	t.Helper()
	assert := is.New(t)
	groups := []appapi.EnvironmentGroup{
		{Name: "web-config", Version: 4, Values: []appapi.Variable{{Name: "LABEL", Value: "private-current-value"}}},
		{Name: "worker-config", Version: 2, Values: []appapi.Variable{{Name: "TOKEN", Value: "worker-private-value"}}},
		{Name: "postgres-database-immutable-id", ManagedBy: "postgres/database", Version: 2, Values: []appapi.Variable{{Name: "PGPASSWORD", Value: "managed-current-value"}}},
	}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		var output any
		switch strings.Replace(request.URL.Path, "/secrets", "/environment-groups", 1) {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/spaces":
			output = []appapi.Space{{ID: "dev-id", Name: "dev", Organization: "team", Cluster: "target", Namespace: "borq-dev", Configuration: appapi.Configuration{Apps: []appapi.App{
				{Name: "web", Image: "web-image", Command: "group-application-private", Env: appapi.EnvReference{Name: "web-config", Version: "3"}},
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
		case "/api/v1/organizations/team/spaces/dev/environment-groups/postgres-database-immutable-id":
			output = groups[2]
		case "/api/v1/organizations/team/spaces/dev/environment-groups/postgres-database-immutable-id/versions/1":
			writer.WriteHeader(http.StatusNotFound)
			output = appapi.Problem{Message: "Only the latest managed credentials are available."}
		case "/api/v1/organizations/team/spaces/dev/environment-groups/missing":
			writer.WriteHeader(http.StatusNotFound)
			output = appapi.Problem{Message: "The environment group was not found."}
		default:
			t.Errorf("unexpected API request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(request.URL.Path, "/secrets") {
			asSecret := func(group appapi.EnvironmentGroup) appapi.Secret {
				data := map[string]string{}
				for _, v := range group.Values {
					data[v.Name] = v.Value
				}
				secret := appapi.Secret{Name: group.Name, Version: group.Version, Type: appapi.SecretTypeSecretGroup, Data: data}
				if group.ManagedBy != "" {
					secret.ManagedBy = &group.ManagedBy
				}
				return secret
			}
			switch value := output.(type) {
			case []appapi.EnvironmentGroup:
				secrets := []appapi.Secret{}
				for _, group := range value {
					secrets = append(secrets, asSecret(group))
				}
				output = secrets
			case appapi.EnvironmentGroup:
				output = asSecret(value)
			}
		}
		assert.NoErr(json.NewEncoder(writer).Encode(output))
	}))
	assert.NoErr(err)
	return web
}
