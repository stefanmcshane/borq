package helm

import (
	"context"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"k8s.io/client-go/rest"
)

// TestReconcileValidatesReleaseInput keeps malformed release identities from
// reaching Helm discovery or storage through the public reconciliation API.
func TestReconcileValidatesReleaseInput(t *testing.T) {
	for _, test := range []struct {
		input   InstallOrUpgradeInput
		message string
	}{
		{InstallOrUpgradeInput{}, "helm namespace must not be empty"},
		{InstallOrUpgradeInput{Namespace: "borq-system"}, "helm release name must not be empty"},
		{InstallOrUpgradeInput{Namespace: "borq-system", ReleaseName: "gitea"}, "helm chart ref must not be empty"},
	} {
		assert := is.New(t)
		_, err := (ReleaseManager{}).Reconcile(context.Background(), test.input)
		assert.True(err != nil)
		assert.Equal(err.Error(), test.message)
	}
}

// TestHelmRawLoaderUsesSelectedConnection verifies Helm's SDK adapters preserve
// selected authentication and namespace without reopening ambient kubeconfig.
func TestHelmRawLoaderUsesSelectedConnection(t *testing.T) {
	is := is.New(t)
	t.Setenv("KUBECONFIG", "/does/not/exist")
	getter := helmRestClientGetter{namespace: "borq", restConfig: rest.Config{Host: "https://selected.test:6443", BearerToken: "scoped-token", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}}
	connection, err := getter.ToRESTConfig()
	is.NoErr(err)
	is.Equal(connection.Host, "https://selected.test:6443")
	config, err := getter.ToRawKubeConfigLoader().ClientConfig()
	is.NoErr(err)
	is.Equal(config.Host, "https://selected.test:6443")
	is.Equal(config.BearerToken, "scoped-token")
	is.True(config.Insecure)
	namespace, _, err := getter.ToRawKubeConfigLoader().Namespace()
	is.NoErr(err)
	is.Equal(namespace, "borq")
}

// TestTimeoutForInput protects the five-minute action default and explicit overrides.
func TestTimeoutForInput(t *testing.T) {
	is := is.New(t)
	is.Equal(timeoutForInput(0), 5*time.Minute)
	is.Equal(timeoutForInput(30*time.Second), 30*time.Second)
}

// TestNewReleaseManagerRejectsMissingConnection verifies Helm fails before
// discovery when neither pod credentials nor a kubeconfig exist.
func TestNewReleaseManagerRejectsMissingConnection(t *testing.T) {
	is := is.New(t)
	_, err := NewReleaseManager(NewReleaseManagerInput{Connection: kubernetes.Connection{}})
	is.True(err != nil)
	is.Equal(err.Error(), "helm requires a Kubernetes connection or kubeconfig path")
}
