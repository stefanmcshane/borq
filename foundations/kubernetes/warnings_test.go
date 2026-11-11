package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestApiWarningsUseApplicationLogger exercises the HTTP warning path through
// both the connection's object client and a client built from its copied config.
// API operations must succeed while preserving warnings in the application log.
func TestApiWarningsUseApplicationLogger(t *testing.T) {
	is := is.New(t)
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			fmt.Fprint(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"namespaces","kind":"Namespace","namespaced":false}]}`)
		case "/api/v1/namespaces/borq-test":
			w.Header().Add("Warning", `299 kube-apiserver "deprecated test annotation"`)
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"borq-test"}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	is.NoErr(os.WriteFile(path, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", server.URL)), 0600))
	connection, err := NewConnection(Config{KubeconfigPath: path}, logger.Logger{})
	is.NoErr(err)
	config := connection.RestConfig()
	copied, err := ctrlclient.New(&config, ctrlclient.Options{Scheme: NewScheme()})
	is.NoErr(err)
	for _, client := range []ctrlclient.Client{connection.objectClient, copied} {
		output.Reset()
		var namespace corev1.Namespace
		is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKey{Name: "borq-test"}, &namespace))
		is.Equal(namespace.Name, "borq-test")
		is.True(strings.Contains(output.String(), `"level":"WARN"`))
		is.True(strings.Contains(output.String(), "deprecated test annotation"))
		is.True(!strings.Contains(output.String(), "goroutine"))
	}
}
