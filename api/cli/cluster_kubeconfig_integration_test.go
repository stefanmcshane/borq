//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TestApplicationCliIntegrationClusterKubeconfig downloads working cluster credentials
// from the management API after the client's source kubeconfig has been removed.
func TestApplicationCliIntegrationClusterKubeconfig(t *testing.T) {
	assert := is.New(t)
	binary, fixturePath := os.Getenv("BORQ_APPLICATION_TEST_BINARY"), os.Getenv("BORQ_KUBECONFIG")
	if binary == "" {
		t.Skip("run make test-apps-e2e to exercise the real cluster kubeconfig command")
	}
	if fixturePath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for kubeconfig integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	original, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	assert.NoErr(err)
	fixture, err := clientcmd.LoadFromFile(fixturePath)
	assert.NoErr(err)
	assert.NoErr(clientcmdapi.FlattenConfig(fixture))
	selectedName := fixture.CurrentContext
	selected := fixture.Contexts[selectedName]
	if selected == nil || fixture.Clusters[selected.Cluster] == nil {
		t.Fatal("integration fixture must have a complete selected context")
	}
	clientApiServer := fixture.Clusters[selected.Cluster].Server
	fixture.Clusters["unrelated-target"] = &clientcmdapi.Cluster{Server: "https://192.0.2.12:6443"}
	fixture.AuthInfos["unrelated-user"] = &clientcmdapi.AuthInfo{Token: "unrelated-cli-credential-must-not-be-exported"}
	fixture.Contexts["unrelated-context"] = &clientcmdapi.Context{Cluster: "unrelated-target", AuthInfo: "unrelated-user"}
	fixture.CurrentContext = "unrelated-context"
	source, err := clientcmd.Write(*fixture)
	assert.NoErr(err)
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.kubeconfig")
	assert.NoErr(os.WriteFile(sourcePath, source, 0o600))
	serverApi, err := localcluster.ResolveLocalApiServer(ctx, sourcePath, selectedName)
	assert.NoErr(err)
	assert.True(serverApi != clientApiServer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	serverDirectory := filepath.Join(directory, "server")
	assert.NoErr(os.MkdirAll(serverDirectory, 0o700))
	serverUrl, _ := startApplicationTestServer(t, ctx, binary, serverDirectory, port)
	organization := fmt.Sprintf("get-cli-%x", time.Now().UnixNano())
	clientHome := filepath.Join(directory, "empty-client-home")
	assert.NoErr(os.MkdirAll(clientHome, 0o700))
	missingConfig := filepath.Join(clientHome, "missing.kubeconfig")
	var environment []string
	for _, variable := range os.Environ() {
		if strings.HasPrefix(variable, "HOME=") || strings.HasPrefix(variable, "XDG_CONFIG_HOME=") || strings.HasPrefix(variable, "KUBECONFIG=") || strings.HasPrefix(variable, "BORQ_KUBECONFIG=") {
			continue
		}
		environment = append(environment, variable)
	}
	environment = append(environment, "HOME="+clientHome, "XDG_CONFIG_HOME="+filepath.Join(clientHome, ".config"), "KUBECONFIG="+missingConfig, "BORQ_KUBECONFIG="+missingConfig)
	for _, arguments := range [][]string{
		{"org", "create", organization, "--api-url", serverUrl},
		{"config", "set", "--profile", "downloaded-target", "--api-url", serverUrl, "--organization", organization},
	} {
		_, err := runClusterDownloadBinary(ctx, binary, environment, arguments...)
		assert.NoErr(err)
	}
	linked, err := runClusterDownloadBinary(ctx, binary, environment, "cluster", "link", "target", "--profile", "downloaded-target", "--kubeconfig", sourcePath, "--context", selectedName, "--api-server", serverApi, "--reconcile=false", "--diff=false", "--output", "json")
	assert.NoErr(err)
	var cluster appapi.Cluster
	assert.NoErr(json.Unmarshal(linked, &cluster))
	_, err = runClusterDownloadBinary(ctx, binary, environment, "config", "set", "--profile", "downloaded-target", "--cluster", cluster.Name)
	assert.NoErr(err)
	assert.NoErr(os.Remove(sourcePath))
	_, err = os.Stat(missingConfig)
	assert.True(os.IsNotExist(err))

	t.Log("a named client profile retrieves the default cluster without local Kubernetes credentials")
	stdout, err := runClusterDownloadBinary(ctx, binary, environment, "cluster", "kubeconfig", "--profile", "downloaded-target")
	assert.NoErr(err)
	assertDownloadedClusterAccess(t, ctx, stdout, selectedName, clientApiServer)
	assert.True(!bytes.Contains(stdout, []byte("unrelated-cli-credential")))
	assert.True(!bytes.Contains(stdout, []byte("192.0.2.")))
	outputPath := filepath.Join(directory, "downloaded.kubeconfig")
	stdout, err = runClusterDownloadBinary(ctx, binary, environment, "cluster", "kubeconfig", "--profile", "downloaded-target", "--output", outputPath)
	assert.NoErr(err)
	assert.Equal(len(stdout), 0)
	info, err := os.Stat(outputPath)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0o600))
	written, err := os.ReadFile(outputPath)
	assert.NoErr(err)
	assertDownloadedClusterAccess(t, ctx, written, selectedName, clientApiServer)

	t.Log("an explicit client endpoint override does not change the saved registration")
	override, err := runClusterDownloadBinary(ctx, binary, environment, "cluster", "kubeconfig", cluster.Name, "--profile", "downloaded-target", "--api-server", "https://127.0.0.1:1")
	assert.NoErr(err)
	configuration, err := clientcmd.Load(override)
	assert.NoErr(err)
	assert.Equal(configuration.Clusters[configuration.Contexts[configuration.CurrentContext].Cluster].Server, "https://127.0.0.1:1")
	stdout, err = runClusterDownloadBinary(ctx, binary, environment, "cluster", "kubeconfig", "--profile", "downloaded-target")
	assert.NoErr(err)
	assertDownloadedClusterAccess(t, ctx, stdout, selectedName, clientApiServer)
	stdout, err = runClusterDownloadBinary(ctx, binary, environment, "cluster", "kubeconfig", cluster.Name, "--profile", "downloaded-target", "--organization", organization+"-other")
	assert.True(err != nil)
	assert.Equal(len(stdout), 0)
	unchanged, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	assert.NoErr(err)
	assert.Equal(unchanged.CurrentContext, original.CurrentContext)
}

func runClusterDownloadBinary(ctx context.Context, binary string, environment []string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environment
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		// Kubeconfig stdout contains credentials, so never include process output in errors.
		return output.Bytes(), fmt.Errorf("borq %s: %w", strings.Join(arguments, " "), err)
	}
	return output.Bytes(), nil
}

func assertDownloadedClusterAccess(t *testing.T, ctx context.Context, source []byte, contextName, apiServer string) {
	t.Helper()
	assert := is.New(t)
	configuration, err := clientcmd.Load(source)
	assert.NoErr(err)
	assert.Equal(configuration.CurrentContext, contextName)
	assert.Equal(len(configuration.Contexts), 1)
	assert.Equal(len(configuration.Clusters), 1)
	assert.Equal(len(configuration.AuthInfos), 1)
	selected := configuration.Contexts[contextName]
	if selected == nil || configuration.Clusters[selected.Cluster] == nil || configuration.AuthInfos[selected.AuthInfo] == nil {
		t.Fatal("downloaded kubeconfig has incomplete selected credentials")
	}
	cluster, user := configuration.Clusters[selected.Cluster], configuration.AuthInfos[selected.AuthInfo]
	assert.Equal(cluster.Server, apiServer)
	assert.Equal(cluster.CertificateAuthority, "")
	assert.True(len(cluster.CertificateAuthorityData) > 0)
	assert.Equal(user.ClientCertificate, "")
	assert.Equal(user.ClientKey, "")
	assert.Equal(user.TokenFile, "")
	assert.True(len(user.ClientCertificateData) > 0 && len(user.ClientKeyData) > 0)
	access, err := clientcmd.RESTConfigFromKubeConfig(source)
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
}
