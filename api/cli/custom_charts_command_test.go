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

// TestCustomChartCommands owns the addons custom command path, action aliases,
// flags, YAML overrides, polling, and failed-operation exit behavior.
func TestCustomChartCommands(t *testing.T) {
	assert := is.New(t)
	status := appapi.Succeeded
	expectedApply := "true"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/organizations/team/spaces/dev/custom-charts/cache":
			if request.Method == http.MethodPut {
				assert.Equal(request.URL.Query().Get("apply"), expectedApply)
				var body appapi.CustomChartInput
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				assert.Equal(body.Chart, "redis")
				assert.Equal(body.Repository, "oci://registry.example.com/charts")
				assert.Equal(body.Version, "1.2.3")
				assert.True(body.Logs != nil && !*body.Logs)
				assert.Equal((*body.Values)["replicas"], float64(0))
				assert.Equal((*body.PodSelector)["role"], "api")
			}
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.CustomChart{Name: "cache", Operation: "install-1"})
		case "/api/v1/custom-chart-operations/install-1":
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "install-1", Status: status, Message: "operation result"})
		case "/api/v1/charts/defaults":
			_ = json.NewEncoder(writer).Encode(appapi.ChartDefaults{Values: "# upstream comment\nreplicas: 1\n"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "values.yaml")
	assert.NoErr(os.WriteFile(file, []byte("replicas: 0\n"), 0o600))
	arguments := []string{"addons", "custom", "apply", "cache", "--server", server.URL, "--organization", "team", "--space", "dev", "--repo", "oci://registry.example.com/charts", "--chart", "redis", "--version", "1.2.3", "--logs=false", "--pod-selector", "role=api", "-f", file}
	output, _, err := runApplicationCommand(t, arguments...)
	assert.NoErr(err)
	assert.True(strings.Contains(output, "succeeded"))
	for _, action := range []string{"install", "apply"} {
		arguments[2] = action
		expectedApply = "true"
		if action == "install" {
			expectedApply = "false"
		}
		_, _, err = runApplicationCommand(t, arguments...)
		assert.NoErr(err)
	}
	status = appapi.Failed
	_, _, err = runApplicationCommand(t, arguments...)
	assert.True(err != nil && strings.Contains(err.Error(), "operation result"))
	output, _, err = runApplicationCommand(t, "addons", "custom", "defaults", "--server", server.URL, "--repo", "https://charts.example.com", "--chart", "redis", "--version", "1.2.3")
	assert.NoErr(err)
	assert.Equal(output, "# upstream comment\nreplicas: 1\n")
}

// TestCustomChartValuesRejectAmbiguity prevents duplicate keys, scalars, nulls, and multiple YAML documents before transport.
func TestCustomChartValuesRejectAmbiguity(t *testing.T) {
	for _, source := range []string{"null", "- value", "key: one\nkey: two", "{}\n---\n{}"} {
		t.Run(source, func(t *testing.T) {
			assert := is.New(t)
			file := filepath.Join(t.TempDir(), "values.yaml")
			assert.NoErr(os.WriteFile(file, []byte(source), 0o600))
			_, err := readCustomChartValues(file)
			assert.True(err != nil)
		})
	}
}
