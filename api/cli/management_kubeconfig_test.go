package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"k8s.io/client-go/tools/clientcmd"
)

// TestManagementKubeconfigSelection owns the shared bootstrap/keys selection:
// merged current context, explicit precedence, private snapshot and cleanup.
func TestManagementKubeconfigSelection(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	first, second := filepath.Join(directory, "first"), filepath.Join(directory, "second")
	assert.NoErr(os.WriteFile(first, []byte("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: correct\n    user: correct\n"), 0600))
	assert.NoErr(os.WriteFile(second, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: correct\n  cluster:\n    server: https://selected.example.test\nusers:\n- name: correct\n  user:\n    token: private-kubernetes-token\n"), 0600))
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)
	selection, cleanup, err := selectManagementKubeconfig("", Config{KubeconfigPath: first, kubeconfigFromDefault: true})
	assert.NoErr(err)
	defer cleanup()
	assert.Equal(selection.flagPath, "")
	assert.Equal(selection.contextName, "selected")
	configuration, err := clientcmd.LoadFromFile(selection.path)
	assert.NoErr(err)
	assert.Equal(configuration.Clusters["correct"].Server, "https://selected.example.test")
	assert.True(configuration.AuthInfos["correct"].Token == "private-kubernetes-token")
	info, err := os.Stat(selection.path)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	cleanup()
	_, err = os.Stat(selection.path)
	assert.True(os.IsNotExist(err))
	// An explicit invalid file cannot borrow entries from KUBECONFIG.
	_, _, err = selectManagementKubeconfig(first, Config{})
	assert.True(err != nil && strings.Contains(err.Error(), "current Kubernetes context"))
	_, _, err = selectManagementKubeconfig(filepath.Join(directory, "missing"), Config{})
	assert.True(err != nil && strings.Contains(err.Error(), "read management kubeconfig"))
}

// TestManagementCommandsNeedCurrentContext rejects an empty local selection for
// both entry points without falling back to a cluster or a saved local install.
func TestManagementCommandsNeedCurrentContext(t *testing.T) {
	for _, name := range []string{"bootstrap", "keys"} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			file := filepath.Join(t.TempDir(), "empty-kubeconfig")
			assert.NoErr(os.WriteFile(file, []byte("apiVersion: v1\nkind: Config\n"), 0600))
			t.Setenv("KUBECONFIG", file)
			_, _, err := runApplicationCommand(t, "management", name)
			assert.True(err != nil && strings.Contains(err.Error(), "kubectl config use-context NAME or supply --kubeconfig PATH"))
		})
	}
}
