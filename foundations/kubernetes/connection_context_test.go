package kubernetes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
)

// TestRestConfigContextSelection verifies registered targets can select a
// kubeconfig context without silently using or changing its current-context.
func TestRestConfigContextSelection(t *testing.T) {
	is := is.New(t)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	is.NoErr(os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: first
  cluster:
    server: https://first.test:6443
- name: target
  cluster:
    server: https://target.test:6443
contexts:
- name: first
  context:
    cluster: first
    user: first
- name: target
  context:
    cluster: target
    user: target
current-context: first
users:
- name: first
  user:
    token: first-token
- name: target
  user:
    token: target-token
`), 0o600))
	selected, err := restConfigForConfig(Config{KubeconfigPath: path, Context: "target"})
	is.NoErr(err)
	is.Equal(selected.Host, "https://target.test:6443")
	is.Equal(selected.BearerToken, "target-token")
	current, err := restConfigForConfig(Config{KubeconfigPath: path})
	is.NoErr(err)
	is.Equal(current.Host, "https://first.test:6443")
	_, err = restConfigForConfig(Config{KubeconfigPath: path, InCluster: true})
	is.True(err != nil)
}
