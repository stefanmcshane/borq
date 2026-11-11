package management

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matryer/is"
	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestLocalBootstrapFiles selects saved legacy credentials and their offline image,
// and rejects an explicitly missing kubeconfig before installing components.
func TestLocalBootstrapFiles(t *testing.T) {
	assert := is.New(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "/unrelated/current-context")
	request := Request{ClusterName: "local", KubeconfigPath: filepath.Join(t.TempDir(), "missing-kubeconfig")}
	bundle := t.TempDir()
	executable := filepath.Join(bundle, "borq")
	_, err := resolveBootstrapFiles(request, executable)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "must already be running"))
	request.KubeconfigPath = ""
	config, err := localcluster.LocalKubeconfigPath("local")
	assert.NoErr(err)
	assert.NoErr(os.MkdirAll(filepath.Dir(config), 0o700))
	assert.NoErr(os.WriteFile(config, bootstrapTestKubeconfig(t), 0o600))
	// This fixture represents an explicitly created development cluster.
	metadata, err := yaml.Marshal([]localcluster.Local{{Name: "local", Runtime: "docker", KubeconfigPath: config}})
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(config)), "clusters.yaml"), metadata, 0o600))
	resolved, err := resolveBootstrapFiles(request, executable)
	assert.NoErr(err)
	assert.Equal(resolved.KubeconfigPath, config)
	assert.Equal(resolved.BorqImage, borqImage)
	assert.Equal(resolved.ServerImageArchivePath, "")
	request.BorqImage = "borq-bundle:explicit"
	resolved, err = resolveBootstrapFiles(request, executable)
	assert.NoErr(err)
	assert.Equal(resolved.KubeconfigPath, config)
	assert.Equal(resolved.ServerImageArchivePath, "")
	request.BorqImage = ""
	archive := filepath.Join(bundle, "borq-server-linux-"+runtime.GOARCH+".tar")
	writeServerArchive(t, archive, []string{"borq-bundle:test"})
	resolved, err = resolveBootstrapFiles(request, executable)
	assert.NoErr(err)
	assert.Equal(resolved.KubeconfigPath, config)
	assert.Equal(resolved.ServerImageArchivePath, archive)
	assert.Equal(resolved.BorqImage, "borq-bundle:test")
	for _, selection := range []struct{ tag, image string }{
		{"latest", borqImage},
		{"v1.2.3", borqImageRepository + ":v1.2.3"},
		{"ghcr.io/example/borq:custom", "ghcr.io/example/borq:custom"},
	} {
		request.BorqImage = selection.tag
		resolved, err = resolveBootstrapFiles(request, executable)
		assert.NoErr(err)
		assert.Equal(resolved.BorqImage, selection.image)
		assert.Equal(resolved.ServerImageArchivePath, "")
	}
	request.BorqImage = ""
	request.KubeconfigPath = filepath.Join(t.TempDir(), "external-kubeconfig")
	assert.NoErr(os.WriteFile(request.KubeconfigPath, bootstrapTestKubeconfig(t), 0600))
	resolved, err = resolveBootstrapFiles(request, executable)
	assert.NoErr(err)
	assert.Equal(resolved.ServerImageArchivePath, "")
	assert.Equal(resolved.BorqImage, borqImage)
	request.KubeconfigPath = ""
	request.Mode = "prod"
	resolved, err = resolveBootstrapFiles(request, executable)
	assert.NoErr(err)
	assert.Equal(resolved.ServerImageArchivePath, archive)
	assert.Equal(resolved.Mode, "prod")
	assert.True(strings.Contains(resolved.validate().Error(), "require development mode"))
}

// TestBootstrapImageReferences accepts Borq tags and full registry references,
// fetching mutable latest images while allowing imported archives to stay offline.
func TestBootstrapImageReferences(t *testing.T) {
	for _, test := range []struct {
		name, selected, expected, archive string
		pullPolicy                        corev1.PullPolicy
	}{
		{name: "default registry", expected: borqImage, pullPolicy: corev1.PullAlways},
		{name: "explicit latest", selected: "latest", expected: borqImage, pullPolicy: corev1.PullAlways},
		{name: "Borq version tag", selected: "v1.2.3", expected: borqImageRepository + ":v1.2.3", pullPolicy: corev1.PullIfNotPresent},
		{name: "full reference", selected: "registry.test:5000/team/borq:custom", expected: "registry.test:5000/team/borq:custom", pullPolicy: corev1.PullIfNotPresent},
		{name: "untagged registry image", selected: borqImageRepository, expected: borqImageRepository, pullPolicy: corev1.PullAlways},
		{name: "pinned digest", selected: borqImageRepository + "@sha256:" + strings.Repeat("a", 64), expected: borqImageRepository + "@sha256:" + strings.Repeat("a", 64), pullPolicy: corev1.PullIfNotPresent},
		{name: "imported latest", selected: borqImage, expected: borqImage, archive: "server.tar", pullPolicy: corev1.PullIfNotPresent},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			request := Request{KubeconfigPath: "/existing/kubeconfig", BorqImage: test.selected, ServerImageArchivePath: test.archive}.withDefaults()
			assert.Equal(request.BorqImage, test.expected)
			assert.NoErr(request.validate())
			container := borqDeployment(request).Spec.Template.Spec.Containers[0]
			assert.Equal(container.Image, test.expected)
			assert.Equal(container.ImagePullPolicy, test.pullPolicy)
		})
	}
}

// TestBootstrapInfersComponentMode distinguishes local Docker tooling from
// persistent native and external installations without using a client profile.
func TestBootstrapInfersComponentMode(t *testing.T) {
	for _, test := range []struct {
		name, runtime, explicitMode, expectedMode, expectedProbe string
		external                                                 bool
	}{
		{name: "Docker", runtime: "docker", expectedMode: "dev", expectedProbe: "http://localhost:7833/api/readyz"},
		{name: "native", runtime: "native", expectedMode: "prod", expectedProbe: "http://localhost:31833/api/readyz"},
		{name: "external", external: true, expectedMode: "prod", expectedProbe: "http://localhost:31833/api/readyz"},
		{name: "explicit development", external: true, explicitMode: "dev", expectedMode: "dev", expectedProbe: "http://localhost:31833/api/readyz"},
		{name: "persistent Docker", runtime: "docker", explicitMode: "prod", expectedMode: "prod", expectedProbe: "http://localhost:7833/api/readyz"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("HOME", t.TempDir())
			request := Request{ClusterName: "local", Mode: test.explicitMode, BorqImage: "borq-test:matching"}
			config := filepath.Join(t.TempDir(), "custom-kubeconfig")
			assert.NoErr(os.WriteFile(config, bootstrapTestKubeconfig(t), 0o600))
			if test.external {
				request.KubeconfigPath = config
			} else {
				path, err := localcluster.LocalKubeconfigPath("local")
				assert.NoErr(err)
				assert.NoErr(os.MkdirAll(filepath.Dir(path), 0o700))
				metadata, err := yaml.Marshal([]localcluster.Local{{Name: "local", Runtime: test.runtime, KubeconfigPath: config}})
				assert.NoErr(err)
				assert.NoErr(os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "clusters.yaml"), metadata, 0o600))
			}
			resolved, err := resolveBootstrapFiles(request, filepath.Join(t.TempDir(), "borq"))
			assert.NoErr(err)
			assert.Equal(resolved.KubeconfigPath, config)
			assert.Equal(resolved.Mode, test.expectedMode)
			assert.Equal(resolved.ProbeURL, test.expectedProbe)
			if test.runtime == "docker" && test.explicitMode == "prod" {
				assert.True(strings.Contains(resolved.validate().Error(), "require development mode"))
			} else {
				assert.NoErr(resolved.validate())
			}
		})
	}
}

// TestServerArchiveSelection avoids pulling another image when a bundle already
// contains the selected server and rejects ambiguous or mismatched offline inputs.
func TestServerArchiveSelection(t *testing.T) {
	for _, test := range []struct {
		name, selected, expected string
		tags                     []string
	}{
		{name: "bundle default", selected: borqImage, tags: []string{"borq-bundle:v1"}, expected: "borq-bundle:v1"},
		{name: "explicit tag", selected: "borq-bundle:v1", tags: []string{"borq-bundle:v1", "borq-bundle:alias"}, expected: "borq-bundle:v1"},
		{name: "missing tag", selected: "other:v2", tags: []string{"borq-bundle:v1"}},
		{name: "ambiguous default", selected: borqImage, tags: []string{"borq-bundle:v1", "borq-bundle:v2"}},
		{name: "untagged", selected: borqImage},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			path := filepath.Join(t.TempDir(), "server.tar")
			writeServerArchive(t, path, test.tags)
			image, err := serverArchiveImage(path, test.selected)
			if test.expected == "" {
				assert.True(err != nil)
				return
			}
			assert.NoErr(err)
			assert.Equal(image, test.expected)
		})
	}
	assert := is.New(t)
	path := filepath.Join(t.TempDir(), "invalid.tar")
	_, err := serverArchiveImage(path, borqImage)
	assert.True(err != nil)
	assert.NoErr(os.WriteFile(path, []byte("invalid archive"), 0o600))
	_, err = serverArchiveImage(path, borqImage)
	assert.True(err != nil)
}

func writeServerArchive(t *testing.T, path string, tags []string) {
	t.Helper()
	assert := is.New(t)
	file, err := os.Create(path)
	assert.NoErr(err)
	archive := tar.NewWriter(file)
	manifest, err := json.Marshal([]map[string]any{{"RepoTags": tags}})
	assert.NoErr(err)
	assert.NoErr(archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))}))
	_, err = archive.Write(manifest)
	assert.NoErr(err)
	assert.NoErr(archive.Close())
	assert.NoErr(file.Close())
}

func bootstrapTestKubeconfig(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/bootstrap.kubeconfig")
	is.New(t).NoErr(err)
	return data
}

// TestKindBootstrapDiscovery uses the selected kubeconfig cluster, independently
// of the Borq installation name, and preserves explicit image/access choices.
func TestKindBootstrapDiscovery(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	file := filepath.Join(directory, "kubeconfig")
	config := strings.ReplaceAll(string(bootstrapTestKubeconfig(t)), "external", "kind-actual-cluster")
	assert.NoErr(os.WriteFile(file, []byte(config), 0600))
	archive := filepath.Join(directory, "borq-server-linux-"+runtime.GOARCH+".tar")
	writeServerArchive(t, archive, []string{"borq-bundle:matching"})
	for _, explicit := range []bool{false, true} {
		request := Request{ClusterName: "production", KubeconfigPath: file}
		if explicit {
			request.BorqImage = "ghcr.io/example/borq:chosen"
			request.ApiUrl = "https://borq.example.test"
		}
		resolved, err := resolveBootstrapFiles(request, filepath.Join(directory, "borq"))
		assert.NoErr(err)
		assert.NoErr(resolved.validate())
		assert.Equal(resolved.localClusterName, "actual-cluster")
		assert.Equal(resolved.Mode, "prod")
		assert.True(resolved.bootstrapProxy)
		if explicit {
			assert.Equal(resolved.ServerImageArchivePath, "")
			assert.Equal(resolved.BorqImage, request.BorqImage)
			assert.Equal(resolved.ProbeURL, request.ApiUrl+borqHealthPath)
		} else {
			assert.Equal(resolved.ServerImageArchivePath, archive)
			assert.Equal(resolved.BorqImage, "borq-bundle:matching")
		}
	}
}

// TestBootstrapInfersClusterName uses the selected kubeconfig cluster rather than
// its current-context alias, with stable DNS names for provider keys.
func TestBootstrapInfersClusterName(t *testing.T) {
	for _, test := range []struct{ cluster, expected string }{
		{"existing-cluster", "existing-cluster"},
		{"kind-existing-cluster", "existing-cluster"},
		{"k3d-existing-cluster", "existing-cluster"},
		{"arn:aws:eks:us-east-1:123456789012:cluster/demo", ""},
		{strings.Repeat("a", 90), ""},
	} {
		t.Run(test.cluster, func(t *testing.T) {
			assert := is.New(t)
			path := filepath.Join(t.TempDir(), "kubeconfig")
			configuration := strings.ReplaceAll(string(bootstrapTestKubeconfig(t)), "external", test.cluster)
			assert.NoErr(os.WriteFile(path, []byte(configuration), 0600))
			resolved, err := resolveBootstrapFiles(Request{KubeconfigPath: path, BorqImage: "borq-test:matching"}, filepath.Join(t.TempDir(), "borq"))
			assert.NoErr(err)
			assert.NoErr(resolved.validate())
			assert.True(resolved.inferredClusterName)
			if test.expected != "" {
				assert.Equal(resolved.ClusterName, test.expected)
			} else {
				assert.Equal(resolved.ClusterName, bootstrapClusterName(test.cluster))
				assert.True(resolved.ClusterName != bootstrapClusterName(test.cluster+"-different"))
				assert.True(len(resolved.ClusterName) <= 58)
			}
		})
	}
}
