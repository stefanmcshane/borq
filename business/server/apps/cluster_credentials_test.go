package apps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TestClusterKubeconfigSelectedFiles embeds only the selected connection's files,
// retaining the client endpoint and excluding other contexts and credentials.
func TestClusterKubeconfigSelectedFiles(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	for name, value := range map[string]string{"ca.crt": "selected-ca", "client.crt": "selected-cert", "client.key": "selected-key", "token": "current-token\n"} {
		assert.NoErr(os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600))
	}
	configuration := clientcmdapi.Config{
		CurrentContext: "other",
		Clusters:       map[string]*clientcmdapi.Cluster{"selected": {Server: "https://internal.test:6443", CertificateAuthority: "ca.crt"}, "other": {Server: "https://other.test", CertificateAuthority: "missing-other-ca"}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"selected": {Token: "stale-token", TokenFile: "token", ClientCertificate: "client.crt", ClientKey: "client.key"}, "other": {Token: "other-private-token"}},
		Contexts:       map[string]*clientcmdapi.Context{"selected": {Cluster: "selected", AuthInfo: "selected", Namespace: "workspace"}, "other": {Cluster: "other", AuthInfo: "other"}},
	}
	encoded, err := clientcmd.Write(configuration)
	assert.NoErr(err)
	path := filepath.Join(directory, "config")
	assert.NoErr(os.WriteFile(path, encoded, 0o600))
	exported, err := exportClusterConfiguration(Cluster{KubeconfigPath: path, Context: "selected", ClientApiServer: "https://127.0.0.1:6443"})
	assert.NoErr(err)
	assert.Equal(exported.CurrentContext, "selected")
	assert.Equal(len(exported.Contexts), 1)
	assert.Equal(len(exported.Clusters), 1)
	assert.Equal(len(exported.AuthInfos), 1)
	assert.Equal(exported.Contexts["selected"].Namespace, "workspace")
	assert.Equal(exported.Clusters["selected"].Server, "https://127.0.0.1:6443")
	assert.Equal(string(exported.Clusters["selected"].CertificateAuthorityData), "selected-ca")
	assert.Equal(exported.Clusters["selected"].CertificateAuthority, "")
	assert.Equal(string(exported.AuthInfos["selected"].ClientCertificateData), "selected-cert")
	assert.Equal(string(exported.AuthInfos["selected"].ClientKeyData), "selected-key")
	assert.Equal(exported.AuthInfos["selected"].ClientCertificate, "")
	assert.Equal(exported.AuthInfos["selected"].ClientKey, "")
	assert.Equal(exported.AuthInfos["selected"].Token, "current-token")
	assert.Equal(exported.AuthInfos["selected"].TokenFile, "")
	result, err := clientcmd.Write(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(result), "other-private-token"))
	assert.True(!strings.Contains(string(result), "missing-other-ca"))
}

// TestClusterKubeconfigServiceAccountRefresh exercises the actual in-cluster REST
// representation without contacting its API and reads the current projected token.
func TestClusterKubeconfigServiceAccountRefresh(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	tokenPath, caPath := filepath.Join(directory, "token"), filepath.Join(directory, "ca.crt")
	assert.NoErr(os.WriteFile(caPath, []byte("service-account-ca"), 0o600))
	restConfiguration := rest.Config{Host: "https://192.0.2.1:6443", BearerToken: "initial-token", BearerTokenFile: tokenPath, TLSClientConfig: rest.TLSClientConfig{CAFile: caPath}}
	for _, token := range []string{"projected-token-one", "projected-token-two"} {
		assert.NoErr(os.WriteFile(tokenPath, []byte(token), 0o600))
		configuration, err := flattenClusterConfiguration(clusterConfigurationFromRest(restConfiguration), "https://client.test:6443")
		assert.NoErr(err)
		assert.Equal(configuration.AuthInfos["borq"].Token, token)
		assert.Equal(configuration.AuthInfos["borq"].TokenFile, "")
		assert.Equal(string(configuration.Clusters["borq"].CertificateAuthorityData), "service-account-ca")
		assert.Equal(configuration.Clusters["borq"].Server, "https://client.test:6443")
	}
}

// TestClusterKubeconfigOrganizationAndErrors prevents wrong-organization access
// and keeps private paths and parser payloads out of export failures.
func TestClusterKubeconfigOrganizationAndErrors(t *testing.T) {
	assert := is.New(t)
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	path := filepath.Join(t.TempDir(), "private-file-name-token-material")
	write, err := documentWrite("cluster", "team/dev", Cluster{Organization: "team", Name: "dev", KubeconfigPath: path}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(context.Background(), []sqlite.Write{write}))
	_, err = manager.GetClusterKubeconfig(context.Background(), "other", "dev")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.GetClusterKubeconfig(context.Background(), "team", "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.GetClusterKubeconfig(context.Background(), "../team", "dev")
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.GetClusterKubeconfig(context.Background(), "team", "dev")
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), "private-file"))
	assert.NoErr(os.WriteFile(path, []byte("private-parser-secret: [invalid"), 0o600))
	_, err = manager.GetClusterKubeconfig(context.Background(), "team", "dev")
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), "private-parser-secret"))
}

// TestClusterClientEndpointRelinking changes only download metadata, preserves
// omission, and keeps real connection changes protected by the existing conflict.
func TestClusterClientEndpointRelinking(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	_, err := manager.CreateOrganization(ctx, "team")
	assert.NoErr(err)
	input := ClusterInput{Organization: "team", Name: "dev", KubeconfigPath: cluster.kubeconfig}
	_, err = manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	for _, endpoint := range []string{"https://127.0.0.1:6443", "https://updated.test:6443"} {
		input.ClientApiServer = endpoint
		registered, err := manager.RegisterCluster(ctx, input)
		assert.NoErr(err)
		assert.Equal(registered.ClientApiServer, endpoint)
		omitted := input
		omitted.ClientApiServer = ""
		preserved, err := manager.RegisterCluster(ctx, omitted)
		assert.NoErr(err)
		assert.Equal(preserved.ClientApiServer, endpoint)
	}
	listed, err := manager.ListClusters(ctx, "team")
	assert.NoErr(err)
	assert.Equal(listed[0].ClientApiServer, input.ClientApiServer)
	input.Context = "alternate"
	_, err = manager.RegisterCluster(ctx, input)
	assert.True(errors.Is(err, ErrConflict))
	for _, endpoint := range []string{"file:///private", "https://user:private@client.test", "https://client.test?token=private", "https://client.test#private"} {
		input.Context = ""
		input.ClientApiServer = endpoint
		_, err = manager.RegisterCluster(ctx, input)
		assert.True(errors.Is(err, ErrInvalid))
		assert.True(!strings.Contains(err.Error(), "private"))
	}
}

// TestUploadedClusterKubeconfigDownload keeps credentials out of normal responses
// and exports them after the target is offline without probing it again.
func TestUploadedClusterKubeconfigDownload(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	_, err := manager.CreateOrganization(ctx, "team")
	assert.NoErr(err)
	source, err := os.ReadFile(cluster.kubeconfig)
	assert.NoErr(err)
	uploaded := strings.Replace(string(source), "user: {}", "user: {token: uploaded-private-token}", 1)
	registered, err := manager.RegisterCluster(ctx, ClusterInput{Organization: "team", Name: "dev", Kubeconfig: uploaded})
	assert.NoErr(err)
	assert.Equal(registered.Kubeconfig, "")
	listed, err := manager.ListClusters(ctx, "team")
	assert.NoErr(err)
	encoded, err := json.Marshal(listed)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "uploaded-private-token"))
	cluster.server.Close()
	ctx = WithPrincipal(ctx, Principal{Login: OrganizationLogin{Organization: "team"}, Permissions: []Permission{PermissionClusterCredentials}})
	download, err := manager.GetClusterKubeconfig(ctx, "team", "dev")
	assert.NoErr(err)
	configuration, err := clientcmd.Load([]byte(download.Kubeconfig))
	assert.NoErr(err)
	assert.Equal(len(configuration.Contexts), 1)
	assert.Equal(configuration.AuthInfos["test"].Token, "uploaded-private-token")
}

// TestClusterKubeconfigExportAuthority keeps installation-owned credentials off
// organization exports while preserving explicit administrator and trusted access.
func TestClusterKubeconfigExportAuthority(t *testing.T) {
	assert := is.New(t)
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	directory := t.TempDir()
	privateToken := "installation-private-file-token"
	configuration := clientcmdapi.Config{
		CurrentContext: "target", Clusters: map[string]*clientcmdapi.Cluster{"target": {Server: "https://target.example.test"}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"target": {Token: privateToken}},
		Contexts:  map[string]*clientcmdapi.Context{"target": {Cluster: "target", AuthInfo: "target"}},
	}
	encoded, err := clientcmd.Write(configuration)
	assert.NoErr(err)
	privatePath := filepath.Join(directory, "installation-kubeconfig")
	assert.NoErr(os.WriteFile(privatePath, encoded, 0600))
	tokenPath := filepath.Join(directory, "installation-token")
	assert.NoErr(os.WriteFile(tokenPath, []byte(privateToken), 0600))
	referenced := configuration.DeepCopy()
	referenced.AuthInfos["target"].Token = ""
	referenced.AuthInfos["target"].TokenFile = tokenPath
	referenceBytes, err := clientcmd.Write(*referenced)
	assert.NoErr(err)
	user := WithPrincipal(t.Context(), Principal{Login: OrganizationLogin{Organization: "team"}, Permissions: []Permission{"*"}})
	for _, test := range []struct {
		name       string
		connection Cluster
	}{
		{"server-file", Cluster{KubeconfigPath: privatePath}},
		{"in-cluster", Cluster{InCluster: true}},
		{"default-connection", Cluster{}},
		{"referenced-token", Cluster{Kubeconfig: string(referenceBytes)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			connection := test.connection
			connection.Organization, connection.Name = "team", test.name
			connection.ID = platform.ClusterID("team", test.name)
			write, err := documentWrite("cluster", "team/"+test.name, connection, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			result, err := manager.GetClusterKubeconfig(user, "team", test.name)
			assert.True(errors.Is(err, ErrForbidden))
			assert.Equal(result.Kubeconfig, "")
			assert.True(!strings.Contains(err.Error(), privateToken))
			assert.True(!strings.Contains(err.Error(), privatePath))
		})
	}
	for _, context := range []context.Context{t.Context(), WithPrincipal(t.Context(), Principal{Admin: true})} {
		exported, err := manager.GetClusterKubeconfig(context, "team", "server-file")
		assert.NoErr(err)
		parsed, err := clientcmd.Load([]byte(exported.Kubeconfig))
		assert.NoErr(err)
		assert.Equal(parsed.AuthInfos["target"].Token, privateToken)
	}
	// Direct inline records remain safely delegable without any server file access.
	write, err := documentWrite("cluster", "team/inline", Cluster{ID: platform.ClusterID("team", "inline"), Organization: "team", Name: "inline", Kubeconfig: string(encoded)}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	exported, err := manager.GetClusterKubeconfig(user, "team", "inline")
	assert.NoErr(err)
	parsed, err := clientcmd.Load([]byte(exported.Kubeconfig))
	assert.NoErr(err)
	assert.Equal(parsed.AuthInfos["target"].Token, privateToken)
}

// TestClusterKubeconfigOwnedFileIdentity rejects an uploaded-path alias without
// its exact cluster identity, including symlinks to private installation files.
func TestClusterKubeconfigOwnedFileIdentity(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong identity", true: "symlink"}[symlink], func(t *testing.T) {
			assert := is.New(t)
			manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
			assert.NoErr(os.MkdirAll(manager.runtime.credentialsDir, 0700))
			expectedID := platform.ClusterID("team", "target")
			path := filepath.Join(manager.runtime.credentialsDir, expectedID+".yaml")
			storedID := "different-cluster"
			if symlink {
				storedID = expectedID
				secretPath := filepath.Join(t.TempDir(), "private")
				assert.NoErr(os.WriteFile(secretPath, []byte("installation-file-marker"), 0600))
				assert.NoErr(os.Symlink(secretPath, path))
			} else {
				assert.NoErr(os.WriteFile(path, []byte("installation-file-marker"), 0600))
			}
			write, err := documentWrite("cluster", "team/target", Cluster{ID: storedID, Organization: "team", Name: "target", KubeconfigPath: path}, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			user := WithPrincipal(t.Context(), Principal{Login: OrganizationLogin{Organization: "team"}, Permissions: []Permission{PermissionClusterCredentials}})
			result, err := manager.GetClusterKubeconfig(user, "team", "target")
			assert.True(errors.Is(err, ErrForbidden))
			assert.Equal(result.Kubeconfig, "")
		})
	}
}
