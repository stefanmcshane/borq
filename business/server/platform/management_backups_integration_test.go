//go:build integration

package platform_test

import (
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

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type nativeAcceptance struct {
	sqlite, bao managementbackup.Artifact
	identity    string
	original    openbao.InitResult
}

func seedNativeManagement(t *testing.T, ctx context.Context, connection kubernetes.Connection, releases helm.ReleaseManager, kube *kubeclient.Clientset, kubeconfig, namespace string, storage objectstore.Config) nativeAcceptance {
	t.Helper()
	assert := is.New(t)
	installNativeVault(t, ctx, releases, namespace, "dev")
	waitNativeVaultPod(t, ctx, kube, namespace)
	settings := platform.Settings{Profile: "dev", Namespace: namespace, StorageClass: "local-path", Timeout: 3 * time.Minute}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	stop := startOffsiteForward(t, ctx, kubeconfig, namespace, "openbao", port, 8200)
	defer stop()
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	fresh, err := openbao.NewClient(openbao.NewClientInput{Address: address})
	assert.NoErr(err)
	assert.NoErr(fresh.WaitForReady(ctx, time.Second, 2*time.Minute))
	initialized, err := fresh.Init(ctx)
	assert.NoErr(err)
	original := *initialized
	for _, key := range original.UnsealKeysB64 {
		assert.NoErr(fresh.Unseal(ctx, key))
	}
	keys, err := json.Marshal(original.UnsealKeysB64)
	assert.NoErr(err)
	assert.NoErr(connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao-admin", Namespace: namespace, Data: map[string]string{"token": original.RootToken, "unsealKeys": string(keys)}}))
	vault, err := platform.ManagementVault(ctx, kubeconfig, namespace)
	assert.NoErr(err)
	for deadline := time.Now().Add(30 * time.Second); ; {
		err := vault.EnsureKvV2(ctx, "secret")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unsealed Raft vault did not become active: %v", err)
		}
		time.Sleep(time.Second)
	}
	_, err = vault.WriteKv(ctx, "secret", "native-recovery/probe", openbao.WriteKvInput{Values: map[string]string{"value": "retained-native-secret"}})
	assert.NoErr(err)
	directory := t.TempDir()
	copied := filepath.Join(directory, "source-kubeconfig")
	configuration, err := clientcmd.LoadFromFile(kubeconfig)
	assert.NoErr(err)
	assert.NoErr(clientcmdapi.MinifyConfig(configuration))
	assert.NoErr(clientcmdapi.FlattenConfig(configuration))
	for _, cluster := range configuration.Clusters {
		file := filepath.Join(directory, "ca.crt")
		assert.NoErr(os.WriteFile(file, cluster.CertificateAuthorityData, 0600))
		cluster.CertificateAuthority = file
		cluster.CertificateAuthorityData = nil
	}
	encoded, err := clientcmd.Write(*configuration)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(copied, encoded, 0600))
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(directory, "borq.sqlite"), ManagementKubeconfigPath: kubeconfig, PlatformSettings: settings, PollInterval: time.Millisecond})
	assert.NoErr(err)
	closed := false
	defer func() {
		if !closed {
			_ = manager.Close()
		}
	}()
	_, err = manager.CreateOrganization(ctx, "native")
	assert.NoErr(err)
	_, err = manager.RegisterCluster(ctx, apps.ClusterInput{Organization: "native", Name: "saved", KubeconfigPath: copied})
	assert.NoErr(err)
	_, err = manager.PutManagementS3(ctx, apps.ManagementS3Input{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: true, AccessKey: storage.Credentials.AccessKey, SecretKey: storage.Credentials.SecretKey})
	assert.NoErr(err)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	_, err = manager.ApplyManagementConfiguration(ctx, apps.ManagementConfiguration{Backups: &apps.ManagementBackupInput{Enabled: true, Interval: "1h", KeepLast: 2, Recipient: identity.Recipient().String()}})
	assert.NoErr(err)
	first := waitNativeResults(t, ctx, manager, "succeeded", "succeeded", "")
	assert.True(first.Components["sqlite"].LastSuccess.Equal(first.Components["sqlite"].Artifact.CapturedAt))
	assert.True(first.Components["openbao"].LastSuccess.Equal(first.Components["openbao"].Artifact.CapturedAt))
	// An unsupported saved credential plugin breaks only the next SQLite capture.
	for _, auth := range configuration.AuthInfos {
		auth.Exec = &clientcmdapi.ExecConfig{Command: "unavailable-recovery-helper", APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode}
	}
	encoded, err = clientcmd.Write(*configuration)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(copied, encoded, 0600))
	_, err = manager.ApplyManagementConfiguration(ctx, apps.ManagementConfiguration{Backups: &apps.ManagementBackupInput{Enabled: true, Interval: "1h", KeepLast: 3, Recipient: identity.Recipient().String()}})
	assert.NoErr(err)
	second := waitNativeResults(t, ctx, manager, "failed", "succeeded", first.Components["openbao"].Artifact.ID)
	assert.Equal(second.Components["sqlite"].Artifact, first.Components["sqlite"].Artifact)
	assert.Equal(second.Components["sqlite"].Storage, first.Components["sqlite"].Storage)
	assert.True(second.Components["openbao"].Artifact.ID != first.Components["openbao"].Artifact.ID)
	assert.NoErr(manager.Close())
	closed = true
	assert.NoErr(os.RemoveAll(directory))
	stateful, err := kube.AppsV1().StatefulSets(namespace).Get(ctx, "openbao", metav1.GetOptions{})
	assert.NoErr(err)
	zero := int32(0)
	stateful.Spec.Replicas = &zero
	_, err = kube.AppsV1().StatefulSets(namespace).Update(ctx, stateful, metav1.UpdateOptions{})
	assert.NoErr(err)
	t.Log("native SQLite and Raft artifacts completed independently; a later SQLite capture failure retained its checkpoint while Raft succeeded")
	return nativeAcceptance{sqlite: first.Components["sqlite"].Artifact, bao: second.Components["openbao"].Artifact, identity: identity.String(), original: original}
}
func waitNativeResults(t *testing.T, ctx context.Context, manager apps.Manager, sqliteState, baoState, previousBao string) apps.ManagementBackupStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		configuration, err := manager.GetManagementConfiguration(ctx)
		if err == nil && configuration.Backups != nil && configuration.Backups.Status != nil {
			status := *configuration.Backups.Status
			if status.Components["sqlite"].State == sqliteState && status.Components["openbao"].State == baoState && status.Components["openbao"].Artifact.ID != previousBao {
				return status
			}
		}
		if time.Now().After(deadline) {
			configuration, _ := manager.GetManagementConfiguration(ctx)
			state, _ := json.Marshal(configuration.Backups)
			t.Fatalf("native results did not reach %s/%s: %s", sqliteState, baoState, state)
		}
		select {
		case <-ctx.Done():
			t.Fatal("native capture canceled")
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func installNativeVault(t *testing.T, ctx context.Context, releases helm.ReleaseManager, namespace, profile string) {
	t.Helper()
	assert := is.New(t)
	catalog, err := platform.GetComponentCatalog(platform.ComponentCatalogInput{Settings: platform.Settings{Profile: profile, Namespace: namespace, StorageClass: "local-path", Timeout: 3 * time.Minute}, Scope: platform.ComponentScopeManagement})
	assert.NoErr(err)
	definition := catalog.Components[0]
	values := definition.Values
	values["server"].(map[string]any)["service"].(map[string]any)["type"] = "ClusterIP"
	delete(values["server"].(map[string]any)["service"].(map[string]any), "nodePort")
	_, err = releases.Reconcile(ctx, helm.InstallOrUpgradeInput{Namespace: namespace, ReleaseName: "openbao", ChartRef: definition.Chart, RepositoryUrl: definition.Repository, Version: definition.Version, Values: values, Timeout: 3 * time.Minute, SkipWait: true, Owner: namespace})
	assert.NoErr(err)
}
func recoverNativeManagement(t *testing.T, ctx context.Context, kubeconfig, binary, namespace string, connection kubernetes.Connection, releases helm.ReleaseManager, kube *kubeclient.Clientset, native nativeAcceptance, config offsite.RecoveryConfig) {
	t.Helper()
	assert := is.New(t)
	targetNamespace := namespace + "-recover"
	_, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNamespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = kube.CoreV1().Namespaces().Delete(cleanup, targetNamespace, metav1.DeleteOptions{})
		_ = kube.RbacV1().ClusterRoleBindings().Delete(cleanup, targetNamespace+"-openbao-server-binding", metav1.DeleteOptions{})
	})
	installNativeVault(t, ctx, releases, targetNamespace, "production")
	waitNativeVaultPod(t, ctx, kube, targetNamespace)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	stop := startOffsiteForward(t, ctx, kubeconfig, targetNamespace, "openbao", port, 8200)
	defer stop()
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	fresh, err := openbao.NewClient(openbao.NewClientInput{Address: address})
	assert.NoErr(err)
	assert.NoErr(fresh.WaitForReady(ctx, time.Second, 2*time.Minute))
	config.Local = config.Destination
	config.Identity = native.identity
	config.OpenBao = managementbackup.OpenBaoRecovery{Address: address, OriginalToken: native.original.RootToken, UnsealKeys: native.original.UnsealKeysB64}
	directory := t.TempDir()
	file := filepath.Join(directory, "recovery.json")
	data, _ := json.Marshal(config)
	assert.NoErr(os.WriteFile(file, data, 0600))
	command := func(arguments ...string) []byte {
		t.Helper()
		run := exec.CommandContext(ctx, binary, append([]string{"management", "backups", "local"}, arguments...)...)
		output, err := run.CombinedOutput()
		if err != nil {
			t.Fatalf("offline native command failed: %v %s", err, output)
		}
		return output
	}
	listed := command("list", "--config", file)
	assert.True(strings.Contains(string(listed), native.sqlite.ID))
	assert.True(strings.Contains(string(listed), native.bao.ID))
	assert.True(!strings.Contains(string(listed), native.original.RootToken))
	assert.True(!strings.Contains(string(listed), native.identity))
	wrong, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	bad := config
	bad.Identity = wrong.String()
	badData, _ := json.Marshal(bad)
	badFile := filepath.Join(directory, "wrong.json")
	assert.NoErr(os.WriteFile(badFile, badData, 0600))
	failed := exec.CommandContext(ctx, binary, "management", "backups", "local", "restore", "openbao", native.bao.ID, "--config", badFile)
	assert.True(failed.Run() != nil)
	status, err := fresh.Status(ctx)
	assert.NoErr(err)
	assert.True(!status.Initialized)
	recoveredDir := filepath.Join(directory, "recovered")
	command("restore", "sqlite", native.sqlite.ID, "--data-dir", recoveredDir, "--config", file)
	_, err = apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(recoveredDir, "borq.sqlite")})
	assert.True(err != nil && strings.Contains(err.Error(), "offline review"))
	inspection := command("inspect", "--data-dir", recoveredDir)
	assert.True(strings.Contains(string(inspection), "management-operation"))
	database, err := sqlite.New(ctx, filepath.Join(recoveredDir, "borq.sqlite"))
	assert.NoErr(err)
	record, err := database.Get(ctx, "organization", "native")
	assert.NoErr(err)
	assert.True(strings.Contains(string(record.Value), "native"))
	saved, err := database.Get(ctx, "cluster", "native/saved")
	assert.NoErr(err)
	var cluster apps.Cluster
	assert.NoErr(json.Unmarshal(saved.Value, &cluster))
	assert.NoErr(database.Close())
	savedConfig, err := clientcmd.LoadFromFile(cluster.KubeconfigPath)
	assert.NoErr(err)
	for _, cluster := range savedConfig.Clusters {
		assert.Equal(cluster.CertificateAuthority, "")
		assert.True(len(cluster.CertificateAuthorityData) > 0)
	}
	command("restore", "openbao", native.bao.ID, "--config", file)
	restored, err := openbao.NewClient(openbao.NewClientInput{Address: address, Token: native.original.RootToken})
	assert.NoErr(err)
	entry, err := restored.ReadKv(ctx, "secret", "native-recovery/probe", 0)
	assert.NoErr(err)
	assert.Equal(entry.Values["value"], "retained-native-secret")
	command("rebind", "--data-dir", recoveredDir, "--config", file)
	rebound, err := restored.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	assert.NoErr(err)
	assert.Equal(rebound.Values["RUSTFS_ACCESS_KEY"], config.Local.AccessKey)
	assert.Equal(rebound.Values["RUSTFS_SECRET_KEY"], config.Local.SecretKey)
	database, err = sqlite.New(ctx, filepath.Join(recoveredDir, "borq.sqlite"))
	assert.NoErr(err)
	shared, err := database.Get(ctx, "management-s3", "shared")
	assert.NoErr(err)
	assert.True(strings.Contains(string(shared.Value), config.Local.Endpoint))
	assert.NoErr(database.Close())
	_, err = os.Stat(filepath.Join(recoveredDir, "RECOVERY_REQUIRED"))
	assert.NoErr(err)
	t.Log("offline native recovery authenticated complete age payloads, recovered self-contained SQLite and Raft secrets with originals absent, and rebound fresh S3 while queued work remained paused")
}

func waitNativeVaultPod(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, namespace string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		pod, err := kube.CoreV1().Pods(namespace).Get(ctx, "openbao-0", metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("native Raft pod did not start: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("native Raft startup canceled")
		case <-time.After(time.Second):
		}
	}
}
