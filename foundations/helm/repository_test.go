package helm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
)

// TestRepositoryCharts discovers installable chart names and exact versions in Helm order,
// excluding broken entries, library charts, and duplicate versions from the UI choices.
func TestRepositoryCharts(t *testing.T) {
	assert := is.New(t)
	repository := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/helm/charts/index.yaml" {
			http.NotFound(writer, request)
			return
		}
		http.ServeFile(writer, request, "testdata/repository/index.yaml")
	}))
	t.Cleanup(repository.Close)
	charts, err := ListCharts(context.Background(), repository.URL+"/helm/charts/")
	assert.NoErr(err)
	assert.Equal(charts, []RepositoryChart{
		{Name: "another", Versions: []string{"2.0.0+build.1"}},
		{Name: "nats", Versions: []string{"1.11.0-rc.1", "1.10.0", "1.2.0"}},
	})
}

// TestRepositoryChartFailures rejects unusable repositories without returning downloaded
// error documents or credentials as chart names or error messages.
func TestRepositoryChartFailures(t *testing.T) {
	repository := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/missing/index.yaml" {
			http.Error(writer, "upstream private error text", http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte("<html>not a chart index</html>"))
	}))
	t.Cleanup(repository.Close)
	for _, input := range []struct{ repository, message string }{
		{"file:///etc/passwd", "Enter an HTTP(S) Helm repository URL without credentials, query, or fragment."},
		{"https://user:secret@example.com", "Enter an HTTP(S) Helm repository URL without credentials, query, or fragment."},
		{"https://example.com?token=secret", "Enter an HTTP(S) Helm repository URL without credentials, query, or fragment."},
		{repository.URL + "/missing", "The repository returned HTTP 404 for index.yaml. Check the repository URL."},
		{repository.URL, "The repository did not return a valid Helm index.yaml."},
	} {
		t.Run(input.message, func(t *testing.T) {
			assert := is.New(t)
			_, err := ListCharts(context.Background(), input.repository)
			assert.True(err != nil)
			assert.Equal(err.Error(), input.message)
		})
	}
}
