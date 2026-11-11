package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestApplicationSecretsPublication owns combining selected group values with
// custom overrides, publishing before planning, and retrying without exposing values.
func TestApplicationSecretsPublication(t *testing.T) {
	for _, test := range []struct {
		name, format, sourceVersion     string
		versionOnly, emptyName, convert bool
	}{
		{name: "custom fields", format: "fields"},
		{name: "custom YAML", format: "yaml"},
		{name: "group and custom fields", format: "fields", sourceVersion: "4"},
		{name: "group and custom YAML", format: "yaml", sourceVersion: "4"},
		{name: "current group with version change", format: "fields", sourceVersion: "4", versionOnly: true},
		{name: "empty YAML name keeps current group", format: "yaml", sourceVersion: "4", emptyName: true},
		{name: "empty YAML name survives form conversion", format: "yaml", sourceVersion: "4", emptyName: true, convert: true},
		{name: "latest source advances", format: "fields", sourceVersion: "latest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			published, plans, reads, sourceReads := 0, 0, 0, 0
			values := []appapi.Variable{{Name: "EMPTY", Value: ""}, {Name: "TOKEN", Value: " private-app-token \nsecond\n"}}
			if test.sourceVersion != "" {
				values = []appapi.Variable{{Name: "EMPTY", Value: ""}, {Name: "SHARED", Value: "source-private-value"}, {Name: "TOKEN", Value: " private-app-token \nsecond\n"}}
			}
			sourceVersion := 4
			groups := map[string]appapi.EnvironmentGroup{}
			selected := ""
			base := "/api/v1/organizations/team/spaces/dev/environment-groups/"
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !strings.HasPrefix(request.URL.Path, base) && serveActionPageFixture(writer, request) {
					return
				}
				switch {
				case request.URL.Path == base+"shared/versions/"+test.sourceVersion:
					assert.Equal(request.Method, http.MethodGet)
					sourceReads++
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.EnvironmentGroup{Name: "shared", Version: sourceVersion, Values: []appapi.Variable{{Name: "TOKEN", Value: "original-shared-token"}, {Name: "EMPTY", Value: "original-nonempty-value"}, {Name: "SHARED", Value: "source-private-value"}}}))
				case strings.HasPrefix(request.URL.Path, base+"web-draft"):
					if request.Method == http.MethodGet {
						reads++
						group, exists := groups[strings.TrimPrefix(request.URL.Path, base)]
						assert.True(exists)
						assert.NoErr(json.NewEncoder(writer).Encode(group))
						return
					}
					assert.Equal(request.Method, http.MethodPost)
					assert.True(strings.HasSuffix(request.URL.Path, "/versions"))
					if test.sourceVersion != "" {
						assert.True(sourceReads > 0)
					}
					var body appapi.PublishEnvironmentGroupInput
					assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
					assert.Equal(body.Values, values)
					assert.True(body.BaseVersion != nil)
					assert.Equal(*body.BaseVersion, 0)
					published++
					selected = strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, base), "/versions")
					if _, exists := groups[selected]; exists {
						writer.WriteHeader(http.StatusConflict)
						_, _ = writer.Write([]byte(`{"message":"already saved"}`))
						return
					}
					group := appapi.EnvironmentGroup{Name: selected, Version: 7, Values: values}
					groups[selected] = group
					assert.NoErr(json.NewEncoder(writer).Encode(group))
				case request.URL.Path == "/api/v1/organizations/team/spaces/dev/plans":
					assert.True(published > 0)
					var body struct {
						Apps []json.RawMessage `json:"apps"`
					}
					assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
					assert.Equal(len(body.Apps), 1)
					for _, private := range []string{"private-app-token", "source-private-value", "original-shared-token", "variableValue"} {
						assert.True(!strings.Contains(string(body.Apps[0]), private))
					}
					var patch appapi.AppPatch
					assert.NoErr(json.Unmarshal(body.Apps[0], &patch))
					assert.Equal(patch.Name, "web")
					assert.Equal(*patch.Env.Name, selected)
					assert.Equal(*patch.Env.Version, "7")
					plans++
					if plans == 1 {
						writer.WriteHeader(http.StatusConflict)
						_, _ = writer.Write([]byte(`{"message":"plan unavailable"}`))
						return
					}
					_, _ = writer.Write([]byte(`{"id":"app-plan"}`))
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
			}))
			assert.NoErr(err)
			form := url.Values{"action": {"plan-app"}, "organization": {"team"}, "space": {"dev"}, "format": {test.format},
				"name": {"web"}, "image": {"nginx"}, "port": {"8080"}, "replicas": {"1"}, "configuration": {"name: web\nimage: nginx\n"},
				"secretDraft": {"draft"}, "variableName": {" TOKEN ", "EMPTY", ""}, "variableValue": {" private-app-token \r\nsecond\r\n", "", ""}}
			if test.sourceVersion != "" {
				form.Set("environment", "shared")
				form.Set("environmentVersion", test.sourceVersion)
				form.Set("configuration", "name: web\nimage: nginx\nenv:\n  name: shared\n  version: '"+test.sourceVersion+"'\n")
			}
			if test.versionOnly {
				form.Set("document", `{"name":"web","env":{"name":"shared","version":"2"}}`)
				form.Del("environment")
			}
			if test.emptyName {
				form.Set("document", `{"name":"web","env":{"name":"shared","version":"2"}}`)
				form.Set("configuration", "name: web\nimage: nginx\nenv:\n  name: ''\n  version: '4'\n")
			}
			if test.convert {
				form.Set("editorFormat", "fields")
				form = convertApplicationEditor(t, web, form)
				form.Set("action", "plan-app")
			}
			response := postForm(web, form)
			assert.Equal(response.Code, http.StatusConflict)
			assert.True(strings.Contains(response.Body.String(), "plan unavailable"))
			response = postForm(web, form)
			assert.Equal(response.Code, http.StatusOK)
			assert.True(!strings.Contains(response.Body.String(), "private-app-token"))
			assert.True(!strings.Contains(response.Body.String(), "source-private-value"))
			assert.Equal(published, 2)
			assert.Equal(reads, 1)
			assert.Equal(plans, 2)
			assert.Equal(len(groups), 1)
			if test.sourceVersion == "latest" {
				sourceVersion++
				assert.Equal(postForm(web, form).Code, http.StatusOK)
				assert.Equal(len(groups), 2)
			}
		})
	}
}

// TestApplicationSecretsGuards keeps selected groups usable alone and prevents
// failed source reads or conflicting secret writes from producing a plan.
func TestApplicationSecretsGuards(t *testing.T) {
	for _, test := range []struct {
		name, failure, configuration string
		names, values                []string
		status, writes, plans        int
	}{
		{name: "unknown YAML selector field", configuration: "env: {nam: shared}", names: []string{"TOKEN"}, values: []string{"private-app-token"}, status: 400},
		{name: "null YAML selector", configuration: "env: null", names: []string{"TOKEN"}, values: []string{"private-app-token"}, status: 400},
		{name: "empty YAML version", configuration: "env: {version: ''}", names: []string{"TOKEN"}, values: []string{"private-app-token"}, status: 400},
		{name: "duplicate keys", names: []string{"TOKEN", " TOKEN "}, values: []string{"private-app-token", "other"}, status: 400},
		{name: "missing key", names: []string{""}, values: []string{"private-app-token"}, status: 400},
		{name: "missing value field", names: []string{"TOKEN"}, status: 400},
		{name: "save failure", names: []string{"TOKEN"}, values: []string{"private-app-token"}, failure: "unavailable", status: 503, writes: 1},
		{name: "source unavailable", names: []string{"TOKEN"}, values: []string{"private-app-token"}, failure: "source", status: 404},
		{name: "managed source", names: []string{"TOKEN"}, values: []string{"private-app-token"}, failure: "managed-source", status: 409},
		{name: "different existing group", names: []string{"TOKEN"}, values: []string{"private-app-token"}, failure: "collision", status: 409, writes: 1},
		{name: "managed group collision", names: []string{"TOKEN"}, values: []string{"private-app-token"}, failure: "managed", status: 409, writes: 1},
		{name: "existing group only", status: 200, plans: 1},
		{name: "empty custom secrets", names: []string{""}, values: []string{""}, status: 200, plans: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			writes, plans, sourceReads := 0, 0, 0
			base := "/api/v1/organizations/team/spaces/dev/environment-groups/"
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !strings.HasPrefix(request.URL.Path, base) && serveActionPageFixture(writer, request) {
					return
				}
				switch request.URL.Path {
				case base + "shared/versions/latest":
					assert.Equal(request.Method, http.MethodGet)
					sourceReads++
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.EnvironmentGroup{Name: "shared", ManagedBy: "postgres", Version: 1, Values: []appapi.Variable{{Name: "TOKEN", Value: "private-app-token"}}}))
				case base + "shared/versions/3":
					assert.Equal(request.Method, http.MethodGet)
					sourceReads++
					writer.WriteHeader(http.StatusNotFound)
					_, _ = writer.Write([]byte(`{"message":"source unavailable"}`))
				case base + "web-draft", base + "web-draft/versions":
					if request.Method == http.MethodGet {
						assert.Equal(request.URL.Path, base+"web-draft")
						assert.True(test.failure == "collision" || test.failure == "managed")
						secret := appapi.EnvironmentGroup{Name: "web-draft", Values: []appapi.Variable{{Name: "TOKEN", Value: "other-value"}}, Version: 1}
						if test.failure == "managed" {
							secret.ManagedBy = "postgres"
							secret.Values[0].Value = "private-app-token"
						}
						assert.NoErr(json.NewEncoder(writer).Encode(secret))
						return
					}
					assert.Equal(request.Method, http.MethodPost)
					assert.Equal(request.URL.Path, base+"web-draft/versions")
					writes++
					writer.WriteHeader(test.status)
					assert.NoErr(json.NewEncoder(writer).Encode(appapi.Problem{Message: test.failure}))
				case "/api/v1/organizations/team/spaces/dev/plans":
					plans++
					var body appapi.Patch
					assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
					assert.Equal(*(*body.Apps)[0].Env.Name, "shared")
					assert.Equal(*(*body.Apps)[0].Env.Version, "3")
					_, _ = writer.Write([]byte(`{"id":"app-plan"}`))
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
			}))
			assert.NoErr(err)
			form := url.Values{"action": {"plan-app"}, "organization": {"team"}, "space": {"dev"}, "format": {"fields"},
				"name": {"web"}, "image": {"nginx"}, "port": {"8080"}, "replicas": {"1"},
				"secretDraft": {"draft"}, "variableName": test.names, "variableValue": test.values}
			if test.configuration != "" {
				form.Set("format", "yaml")
				form.Set("configuration", "name: web\nimage: nginx\n"+test.configuration+"\n")
			}
			if test.plans > 0 || test.failure == "source" {
				form.Set("document", `{"env":{"name":"shared","version":"3"}}`)
			}
			if test.failure == "managed-source" {
				form.Set("document", `{"env":{"name":"shared","version":"latest"}}`)
			}
			response := postForm(web, form)
			assert.Equal(response.Code, test.status)
			assert.Equal(writes, test.writes)
			assert.Equal(plans, test.plans)
			if test.failure == "source" {
				assert.Equal(sourceReads, 1)
				assert.True(strings.Contains(response.Body.String(), "source unavailable"))
			}
			if test.configuration != "" {
				assert.True(strings.Contains(response.Body.String(), "valid secret group name and version"))
			}
			if test.failure == "managed-source" {
				assert.Equal(sourceReads, 1)
				assert.True(strings.Contains(response.Body.String(), "credentials rotate"))
			}
			assert.True(!strings.Contains(response.Body.String(), "private-app-token"))
		})
	}
}
