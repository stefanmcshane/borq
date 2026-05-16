package helm

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"

	"k8s.io/client-go/rest"
)

// TestValidateInstallOrUpgradeInput verifies that the wrapper rejects missing
// required release inputs before invoking Helm.
func TestValidateInstallOrUpgradeInput(t *testing.T) {
	is := is.New(t)

	err := validateInstallOrUpgradeInput(InstallOrUpgradeInput{})
	is.Equal(err.Error(), "helm namespace must not be empty")

	err = validateInstallOrUpgradeInput(InstallOrUpgradeInput{Namespace: "borq-system"})
	is.Equal(err.Error(), "helm release name must not be empty")

	err = validateInstallOrUpgradeInput(InstallOrUpgradeInput{
		Namespace:   "borq-system",
		ReleaseName: "gitea",
	})
	is.Equal(err.Error(), "helm chart ref must not be empty")

	err = validateInstallOrUpgradeInput(InstallOrUpgradeInput{
		Namespace:   "borq-system",
		ReleaseName: "gitea",
		ChartRef:    "gitea",
	})
	is.NoErr(err)
}

// TestNewReleaseManager verifies that Helm bootstrap creates one shared CLI
// workspace and reuses it for release operations.
func TestNewReleaseManager(t *testing.T) {
	is := is.New(t)

	manager, err := NewReleaseManager(NewReleaseManagerInput{
		Connection:     kubernetes.Connection{},
		KubeconfigPath: "/tmp/kubeconfig",
	})
	is.NoErr(err)
	defer manager.Close()

	settings := manager.newEnvSettings("borq-system")

	is.Equal(settings.KubeConfig, "/tmp/kubeconfig")
	is.True(strings.Contains(settings.RepositoryCache, "borq-helm-"))
	is.True(strings.Contains(settings.RepositoryConfig, "borq-helm-"))
	is.True(strings.Contains(settings.RegistryConfig, "borq-helm-"))
	is.True(strings.Contains(settings.ContentCache, "borq-helm-"))
}

// TestHelmRestClientGetter verifies that the Helm REST client getter uses the
// existing Kubernetes connection details and namespace override.
func TestHelmRestClientGetter(t *testing.T) {
	is := is.New(t)

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	err := os.WriteFile(kubeconfigPath, []byte(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test-token
`), 0o600)
	is.NoErr(err)

	getter := helmRestClientGetter{
		restConfig: rest.Config{
			Host: "https://127.0.0.1:6443",
		},
		kubeconfigPath: kubeconfigPath,
		namespace:      "flux-system",
	}

	restConfig, err := getter.ToRESTConfig()
	is.NoErr(err)
	is.Equal(restConfig.Host, "https://127.0.0.1:6443")

	loader := getter.ToRawKubeConfigLoader()
	namespace, _, err := loader.Namespace()
	is.NoErr(err)
	is.Equal(namespace, "flux-system")
}

// TestTimeoutForInput verifies that Helm actions fall back to a bounded
// default timeout when none is provided.
func TestTimeoutForInput(t *testing.T) {
	is := is.New(t)

	is.Equal(timeoutForInput(0), defaultActionTimeout)
	is.Equal(timeoutForInput(30*time.Second), 30*time.Second)
}

// TestNewReleaseManagerRejectsEmptyKubeconfigPath verifies that the shared
// Helm setup fails fast when bootstrap does not provide an explicit kubeconfig.
func TestNewReleaseManagerRejectsEmptyKubeconfigPath(t *testing.T) {
	is := is.New(t)

	_, err := NewReleaseManager(NewReleaseManagerInput{Connection: kubernetes.Connection{}})
	is.Equal(err.Error(), "helm kubeconfig path must not be empty")
}
