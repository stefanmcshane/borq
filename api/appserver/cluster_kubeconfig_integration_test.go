//go:build integration

package appserver_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TestApplicationIntegrationClusterKubeconfig exports only the registered context
// and uses its embedded credentials to read the real target Kubernetes API.
func TestApplicationIntegrationClusterKubeconfig(t *testing.T) {
	assert := is.New(t)
	fixturePath := os.Getenv("BORQ_KUBECONFIG")
	if fixturePath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for kubeconfig integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, err := clientcmd.LoadFromFile(fixturePath)
	assert.NoErr(err)
	assert.NoErr(clientcmdapi.FlattenConfig(fixture))
	selected := fixture.Contexts[fixture.CurrentContext]
	if selected == nil || fixture.Clusters[selected.Cluster] == nil || fixture.AuthInfos[selected.AuthInfo] == nil {
		t.Fatal("integration fixture must have a complete selected context")
	}
	clientApiServer := fixture.Clusters[selected.Cluster].Server
	targetCluster := fixture.Clusters[selected.Cluster].DeepCopy()
	targetUser := fixture.AuthInfos[selected.AuthInfo].DeepCopy()
	source := clientcmdapi.Config{
		CurrentContext: "unrelated-context",
		Clusters: map[string]*clientcmdapi.Cluster{
			"selected-target":  targetCluster,
			"unrelated-target": {Server: "https://192.0.2.11:6443"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"selected-user":  targetUser,
			"unrelated-user": {Token: "unrelated-credential-must-not-be-exported"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"selected-context":  {Cluster: "selected-target", AuthInfo: "selected-user", Namespace: "kube-system"},
			"unrelated-context": {Cluster: "unrelated-target", AuthInfo: "unrelated-user"},
		},
	}
	encoded, err := clientcmd.Write(source)
	assert.NoErr(err)
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(t.TempDir(), "clusters.sqlite")})
	assert.NoErr(err)
	handler, err := appapi.NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); assert.NoErr(manager.Close()) })
	client, err := appapi.NewClientWithResponses(server.URL)
	assert.NoErr(err)
	organization := fmt.Sprintf("download-%x", time.Now().UnixNano())
	for _, name := range []string{organization, organization + "-other"} {
		response, err := client.PutOrganizationWithResponse(ctx, name)
		assert.NoErr(err)
		assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusOK)
	}
	selectedContext, inline, reconcile := "selected-context", string(encoded), false
	registered, err := client.PutClusterWithResponse(ctx, organization, "target", appapi.ClusterInput{
		Context: &selectedContext, Kubeconfig: &inline, ClientApiServer: &clientApiServer, Reconcile: &reconcile,
	})
	assert.NoErr(err)
	assertApplicationResponse(t, registered.StatusCode(), registered.Body, http.StatusOK)
	if registered.JSON200 == nil {
		t.Fatal("cluster registration did not return its private credential location")
	}
	stored, err := clientcmd.LoadFromFile(registered.JSON200.KubeconfigPath)
	assert.NoErr(err)
	stored.Clusters["selected-target"].Server = "https://192.0.2.10:6443"
	unreachable, err := clientcmd.Write(*stored)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(registered.JSON200.KubeconfigPath, unreachable, 0o600))

	t.Log("export reads saved credentials without contacting the unreachable server-side endpoint")
	exported, err := client.GetClusterKubeconfigWithResponse(ctx, organization, "target")
	assert.NoErr(err)
	assertApplicationResponse(t, exported.StatusCode(), exported.Body, http.StatusOK)
	if exported.JSON200 == nil {
		t.Fatal("kubeconfig export did not return a document")
	}
	assert.Equal(exported.HTTPResponse.Header.Get("Cache-Control"), "no-store")
	configuration, err := clientcmd.Load([]byte(exported.JSON200.Kubeconfig))
	assert.NoErr(err)
	assert.Equal(configuration.CurrentContext, selectedContext)
	assert.Equal(len(configuration.Contexts), 1)
	assert.Equal(len(configuration.Clusters), 1)
	assert.Equal(len(configuration.AuthInfos), 1)
	selectedExport := configuration.Contexts[selectedContext]
	if selectedExport == nil {
		t.Fatal("exported kubeconfig omitted the registered context")
	}
	assert.Equal(selectedExport.Namespace, "kube-system")
	cluster, user := configuration.Clusters[selectedExport.Cluster], configuration.AuthInfos[selectedExport.AuthInfo]
	if cluster == nil || user == nil {
		t.Fatal("exported kubeconfig has incomplete selected credentials")
	}
	assert.Equal(cluster.Server, clientApiServer)
	assert.Equal(cluster.CertificateAuthority, "")
	assert.True(bytes.Equal(cluster.CertificateAuthorityData, targetCluster.CertificateAuthorityData))
	assert.True(len(cluster.CertificateAuthorityData) > 0)
	assert.Equal(user.ClientCertificate, "")
	assert.Equal(user.ClientKey, "")
	assert.Equal(user.TokenFile, "")
	assert.True(bytes.Equal(user.ClientCertificateData, targetUser.ClientCertificateData))
	assert.True(bytes.Equal(user.ClientKeyData, targetUser.ClientKeyData))
	assert.True(len(user.ClientCertificateData) > 0 && len(user.ClientKeyData) > 0)
	assert.True(!strings.Contains(exported.JSON200.Kubeconfig, "unrelated-credential"))
	assert.True(!strings.Contains(exported.JSON200.Kubeconfig, "192.0.2."))
	access, err := clientcmd.RESTConfigFromKubeConfig([]byte(exported.JSON200.Kubeconfig))
	assert.NoErr(err)
	access.Timeout = 10 * time.Second
	kube, err := kubernetes.NewForConfig(access)
	assert.NoErr(err)
	version, err := kube.Discovery().ServerVersion()
	assert.NoErr(err)
	assert.True(version.GitVersion != "")
	namespaces, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	foundSystem := false
	for _, namespace := range namespaces.Items {
		foundSystem = foundSystem || namespace.Name == "kube-system"
	}
	assert.True(foundSystem)

	t.Log("organization-scoped exports and public cluster lists do not expose unrelated credentials")
	missing, err := client.GetClusterKubeconfigWithResponse(ctx, organization+"-other", "target")
	assert.NoErr(err)
	assertApplicationResponse(t, missing.StatusCode(), missing.Body, http.StatusNotFound)
	assert.Equal(missing.HTTPResponse.Header.Get("Cache-Control"), "no-store")
	listed, err := client.ListClustersWithResponse(ctx, organization)
	assert.NoErr(err)
	assertApplicationResponse(t, listed.StatusCode(), listed.Body, http.StatusOK)
	assert.True(!bytes.Contains(listed.Body, []byte("client-key-data")))
	assert.True(!bytes.Contains(listed.Body, []byte("client-certificate-data")))
	assert.True(!bytes.Contains(listed.Body, []byte("unrelated-credential")))
}
