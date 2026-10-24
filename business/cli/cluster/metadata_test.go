package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// TestLocalMetadataRetainsCustomKubeconfig keeps named bootstrap independent of the working directory or output override used during creation.
func TestLocalMetadataRetainsCustomKubeconfig(t *testing.T) {
	is := is.New(t)
	homeDirectory := t.TempDir()
	t.Setenv("HOME", homeDirectory)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDirectory, ".config"))
	_, err := GetLocal("../escape")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "cluster name must"))
	path, err := LocalKubeconfigPath("management")
	is.NoErr(err)
	legacy, err := GetLocal("management")
	is.NoErr(err)
	is.Equal(legacy, Local{Name: "management", Runtime: "native", KubeconfigPath: path})
	configured := Local{Name: "management", Runtime: "native", KubeconfigPath: filepath.Join(t.TempDir(), "custom-kubeconfig")}
	is.NoErr(writeLocalRegistry(filepath.Join(homeDirectory, ".config", "borq", "clusters.yaml"), []Local{configured}))
	actual, err := GetLocal("management")
	is.NoErr(err)
	is.Equal(actual, configured)
	registry := filepath.Join(homeDirectory, ".config", "borq", "clusters.yaml")
	info, err := os.Stat(registry)
	is.NoErr(err)
	is.Equal(info.Mode().Perm(), os.FileMode(0600))
	is.NoErr(os.WriteFile(registry, []byte("invalid: ["), 0600))
	_, err = GetLocal("management")
	is.True(err != nil)
}

// TestLocalRegistryMigratesKubeconfigs imports all old records, flattens relative
// credential files, and lets client-go merge the new directory's kubeconfigs.
func TestLocalRegistryMigratesKubeconfigs(t *testing.T) {
	assert := is.New(t)
	homeDirectory := t.TempDir()
	t.Setenv("HOME", homeDirectory)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDirectory, ".config"))
	paths := []string{}
	for _, name := range []string{"first", "second"} {
		directory := filepath.Join(homeDirectory, ".borq", "clusters", name)
		assert.NoErr(os.MkdirAll(directory, 0700))
		assert.NoErr(os.WriteFile(filepath.Join(directory, "ca.crt"), []byte("test certificate"), 0600))
		assert.NoErr(os.WriteFile(filepath.Join(directory, "token"), []byte("test-token"), 0600))
		configuration := clientcmdapi.NewConfig()
		configuration.CurrentContext = name
		configuration.Clusters[name] = &clientcmdapi.Cluster{Server: "https://" + name + ".test:6443", CertificateAuthority: "ca.crt"} // The Kubernetes config API stores entries by pointer.
		configuration.AuthInfos[name] = &clientcmdapi.AuthInfo{TokenFile: "token"}
		configuration.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
		encoded, err := clientcmd.Write(*configuration)
		assert.NoErr(err)
		legacyPath := filepath.Join(directory, "kubeconfig")
		assert.NoErr(os.WriteFile(legacyPath, encoded, 0600))
		if name == "first" {
			metadata, err := yaml.Marshal(Local{Name: name, Runtime: "native", KubeconfigPath: legacyPath})
			assert.NoErr(err)
			assert.NoErr(os.WriteFile(filepath.Join(directory, "cluster.yaml"), metadata, 0600))
		}
		path, err := LocalKubeconfigPath(name)
		assert.NoErr(err)
		paths = append(paths, path)
	}
	first, err := GetLocal("first")
	assert.NoErr(err)
	assert.Equal(first.Runtime, "native")
	assert.Equal(first.KubeconfigPath, paths[0])
	second, err := GetLocal("second")
	assert.NoErr(err)
	assert.Equal(second.Runtime, "docker")
	assert.Equal(second.KubeconfigPath, paths[1])
	// Explicit precedence is how kubectl consumes a KUBECONFIG list of files.
	merged, err := (&clientcmd.ClientConfigLoadingRules{Precedence: paths}).Load()
	assert.NoErr(err)
	assert.Equal(len(merged.Clusters), 2)
	assert.Equal(len(merged.Contexts), 2)
	for _, name := range []string{"first", "second"} {
		assert.Equal(string(merged.Clusters[name].CertificateAuthorityData), "test certificate")
		assert.Equal(merged.Clusters[name].CertificateAuthority, "")
		assert.Equal(merged.AuthInfos[name].TokenFile, "")
		assert.Equal(merged.AuthInfos[name].Token, "test-token")
		_, err := os.Stat(filepath.Join(homeDirectory, ".borq", "clusters", name, "kubeconfig"))
		assert.NoErr(err) // Keep the original credentials as a migration backup.
		info, err := os.Stat(filepath.Join(homeDirectory, ".config", "borq", "kubeconfigs", name))
		assert.NoErr(err)
		assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	}
	entries, err := os.ReadDir(filepath.Join(homeDirectory, ".config", "borq", "kubeconfigs"))
	assert.NoErr(err)
	assert.Equal(len(entries), 2) // Registry records, locks and temporary files stay outside the glob.
}

// TestLocalMigrationProtectsNewCredentials refuses to overwrite an existing
// destination and keeps authoritative consolidated records ahead of legacy backups.
func TestLocalMigrationProtectsNewCredentials(t *testing.T) {
	assert := is.New(t)
	homeDirectory := t.TempDir()
	t.Setenv("HOME", homeDirectory)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDirectory, ".config"))
	directory := filepath.Join(homeDirectory, ".borq", "clusters", "local")
	legacy := filepath.Join(directory, "kubeconfig")
	assert.NoErr(writePrivateFile(legacy, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")))
	metadata, err := yaml.Marshal(Local{Name: "local", Runtime: "native", KubeconfigPath: legacy})
	assert.NoErr(err)
	assert.NoErr(writePrivateFile(filepath.Join(directory, "cluster.yaml"), metadata))
	destination, err := LocalKubeconfigPath("local")
	assert.NoErr(err)
	assert.NoErr(writePrivateFile(destination, []byte("fresh credentials")))
	_, err = GetLocal("local")
	assert.True(err != nil)
	contents, err := os.ReadFile(destination)
	assert.NoErr(err)
	assert.Equal(string(contents), "fresh credentials")
	recreated := Local{Name: "local", Runtime: "docker", KubeconfigPath: destination}
	assert.NoErr(writeLocalRegistry(filepath.Join(homeDirectory, ".config", "borq", "clusters.yaml"), []Local{recreated}))
	// A stale or malformed backup must not override the consolidated registry.
	assert.NoErr(os.WriteFile(filepath.Join(directory, "cluster.yaml"), []byte("malformed: ["), 0600))
	actual, err := GetLocal("local")
	assert.NoErr(err)
	assert.Equal(actual, recreated)
	_, err = os.Stat(legacy)
	assert.NoErr(err)
}

// TestSharedRegistryMigratesIntoConfig moves the previous consolidated layout
// into the shared config directory while retaining backups and authoritative new records.
func TestSharedRegistryMigratesIntoConfig(t *testing.T) {
	assert := is.New(t)
	homeDirectory := t.TempDir()
	t.Setenv("HOME", homeDirectory)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDirectory, "custom-config"))
	legacyPath := filepath.Join(homeDirectory, ".borq", "kubeconfigs", "local")
	assert.NoErr(writePrivateFile(legacyPath, []byte("apiVersion: v1\nkind: Config\ncurrent-context: local\nclusters:\n- name: local\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: local\nusers:\n- name: local\n  user:\n    token: migration-test\n")))
	legacyRegistry := filepath.Join(homeDirectory, ".borq", "clusters.yaml")
	encoded, err := yaml.Marshal([]Local{{Name: "local", Runtime: "docker", KubeconfigPath: legacyPath}})
	assert.NoErr(err)
	assert.NoErr(writePrivateFile(legacyRegistry, encoded))
	local, err := GetLocal("local")
	assert.NoErr(err)
	assert.Equal(local.KubeconfigPath, filepath.Join(homeDirectory, "custom-config", "borq", "kubeconfigs", "local"))
	configuration, err := clientcmd.LoadFromFile(local.KubeconfigPath)
	assert.NoErr(err)
	assert.Equal(configuration.Clusters["local"].Server, "https://127.0.0.1:6443")
	assert.Equal(configuration.AuthInfos["local"].Token, "migration-test")
	_, err = os.Stat(filepath.Join(homeDirectory, "custom-config", "borq", "clusters.yaml"))
	assert.NoErr(err)
	_, err = os.Stat(legacyPath)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(legacyRegistry, []byte("broken old backup: ["), 0600))
	again, err := GetLocal("local")
	assert.NoErr(err)
	assert.Equal(again, local)
}

// TestLocalRegistryRejectsInvalidRecords prevents corrupt or ambiguous registry
// content from silently being replaced during cluster creation.
func TestLocalRegistryRejectsInvalidRecords(t *testing.T) {
	for _, contents := range []string{
		"", "null", "name: wrong-shape", "invalid: [",
		"- {name: duplicate, runtime: docker, kubeconfigPath: /one}\n- {name: duplicate, runtime: native, kubeconfigPath: /two}",
		"- {name: ../escape, runtime: docker, kubeconfigPath: /one}",
		"- {name: valid, runtime: invalid, kubeconfigPath: /one}",
		"- {name: valid, runtime: docker, kubeconfigPath: ''}",
	} {
		t.Run(contents, func(t *testing.T) {
			assert := is.New(t)
			homeDirectory := t.TempDir()
			t.Setenv("HOME", homeDirectory)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDirectory, ".config"))
			registry := filepath.Join(homeDirectory, ".config", "borq", "clusters.yaml")
			assert.NoErr(writePrivateFile(registry, []byte(contents)))
			_, err := GetLocal("valid")
			assert.True(err != nil)
			after, err := os.ReadFile(registry)
			assert.NoErr(err)
			assert.Equal(string(after), contents)
		})
	}
}
