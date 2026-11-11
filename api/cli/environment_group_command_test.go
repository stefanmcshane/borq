package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestEnvironmentGroupCommands uses the generated JSON client for env creation and reads,
// keeping literal values out of apps.yaml while preserving complete config output.
func TestEnvironmentGroupCommands(t *testing.T) {
	assert := is.New(t)
	base := "/api/v1/organizations/team/spaces/dev/environment-groups"
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		result := appapi.EnvironmentGroup{Name: "backend", Version: 2, Values: []appapi.Variable{{Name: "TOKEN", Value: "full-secret-value"}, {Name: "EMPTY", Value: ""}}}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == base:
			_ = json.NewEncoder(writer).Encode([]appapi.EnvironmentGroup{result})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions"):
			assert.Equal(request.Header.Get("Content-Type"), "application/json")
			var body appapi.PublishEnvironmentGroupInput
			assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(body.Values, result.Values)
			assert.True(body.BaseVersion != nil)
			assert.Equal(*body.BaseVersion, 1)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(result)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/clone"):
			var body appapi.CloneEnvironmentGroupInput
			assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(body.SourceName, "backend")
			assert.Equal(*body.SourceVersion, "2")
			result.Name = "backend-next"
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(result)
		default:
			_ = json.NewEncoder(writer).Encode(result)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "env.yaml")
	assert.NoErr(os.WriteFile(file, []byte("values:\n- name: TOKEN\n  value: full-secret-value\n- name: EMPTY\n  value: ''\n"), 0o600))
	flags := []string{"--server", server.URL, "--organization", "team", "--space", "dev"}
	for _, arguments := range [][]string{
		{"environment-group", "create", "backend", "-f", file, "--base-version", "1"},
		{"environment-group", "get", "backend"},
		{"env", "get", "backend", "--version", "2"},
		{"environment-groups", "list"},
		{"environment-group", "clone", "backend-next", "--from", "backend", "--version", "2"},
	} {
		output, _, err := runApplicationCommand(t, append(arguments, flags...)...)
		assert.NoErr(err)
		assert.True(strings.Contains(output, "full-secret-value"))
	}
	assert.Equal(requests, []string{
		"POST " + base + "/backend/versions",
		"GET " + base + "/backend/versions/latest",
		"GET " + base + "/backend/versions/2",
		"GET " + base,
		"POST " + base + "/backend-next/clone",
	})
}

// TestEnvironmentGroupYamlValidation rejects ambiguous or malformed value files before
// transport and preserves empty maps for intentional complete-config publication.
func TestEnvironmentGroupYamlValidation(t *testing.T) {
	for _, source := range []string{
		"null", "{}", "values: null", "values: {}", "values: []\n---\n", "values: []\nvalues: []\n",
		"values:\n- name: DEBUG\n  value: true\n", "values:\n- name: TOKEN\n  value: private-value\n  unknown: true\n",
		"values:\n- name: TOKEN\n", "values: []\nunknown: true\n", "values: []\nbaseVersion: -1\n",
	} {
		assert := is.New(t)
		_, err := decodeEnvironmentGroup([]byte(source))
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), "private-value"))
	}
	assert := is.New(t)
	body, err := decodeEnvironmentGroup([]byte("values: []\n"))
	assert.NoErr(err)
	assert.True(body.Values != nil)
	assert.Equal(len(body.Values), 0)
	assert.True(body.BaseVersion == nil)
	_, _, err = runApplicationCommand(t, "environment-group", "get", "backend")
	assert.True(err != nil)
}
