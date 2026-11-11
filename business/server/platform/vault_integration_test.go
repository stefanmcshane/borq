//go:build integration

package platform

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestPersistentVaultIntegration verifies production OpenBao initialization,
// pre-saved recovery material, auto-unsealing after a pod restart, and retained KV data.
func TestPersistentVaultIntegration(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	is.NoErr(err)
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	is.NoErr(err)
	namespace := fmt.Sprintf("borq-vault-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = client.RbacV1().ClusterRoleBindings().Delete(cleanup, namespace+"-openbao-server-binding", metav1.DeleteOptions{})
	})
	storageClass := os.Getenv("BORQ_TEST_STORAGE_CLASS")
	if storageClass == "" {
		storageClass = "local-path"
	}
	settings, err := normalizeSettings(Settings{Profile: "production", Namespace: namespace, StorageClass: storageClass, Timeout: 2 * time.Minute})
	is.NoErr(err)
	is.NoErr(prepareNamespace(ctx, connection, namespace))
	is.NoErr(ensureAdminSecret(ctx, connection, namespace))
	admin, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	is.NoErr(err)
	is.True(admin.Data["sealKey"] != "")
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfigPath})
	is.NoErr(err)
	defer releases.Close()
	definition := managementCatalog(settings, false)[0]
	installed, err := reconcileComponent(ctx, releases, definition, valuesInput{Settings: settings}, false, false)
	is.NoErr(err)
	is.Equal(installed.Revision, 1)
	// No Borq initialization callback runs until the independent OpenBao process
	// is already active. The pre-saved token must authenticate it nonetheless.
	vault, err := OpenManagementVault(ctx, connection, namespace)
	is.NoErr(err)
	is.NoErr(vault.WaitForActive(ctx, time.Second, settings.Timeout))
	is.NoErr(prepareVault(ctx, connection, settings))
	vault, err = OpenManagementVault(ctx, connection, namespace)
	is.NoErr(err)
	recipient, err := ManagementBackupRecipient(ctx, kubeconfigPath, namespace)
	is.NoErr(err)
	identity, err := managementbackup.RecoveryIdentity(admin.Data["sealKey"])
	is.NoErr(err)
	is.Equal(recipient, identity.Recipient().String())
	_, err = vault.WriteKv(ctx, "secret", "test/persistence", openbao.WriteKvInput{Values: map[string]string{"value": "survives-restart"}, CheckVersion: 0})
	is.NoErr(err)
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, "openbao-0", metav1.GetOptions{})
	is.NoErr(err)
	is.NoErr(client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current, err := client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err == nil && current.UID != pod.UID {
			status, err := vault.Status(ctx)
			if err == nil && status.Initialized && !status.Sealed {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("persistent vault did not restart and auto-unseal")
		case <-ticker.C:
		}
	}
	vault, err = ManagementVault(ctx, kubeconfigPath, namespace)
	is.NoErr(err)
	restoredRecipient, err := ManagementBackupRecipient(ctx, kubeconfigPath, namespace)
	is.NoErr(err)
	is.Equal(restoredRecipient, recipient)
	keyEntry, err := vault.ReadKv(ctx, "secret", managementBackupKeyPath, 0)
	is.NoErr(err)
	is.Equal(keyEntry.Values["identity"], identity.String())
	// A changed recovery root must fail without overwriting the retained archive key.
	altered := maps.Clone(admin.Data)
	altered["sealKey"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	is.NoErr(connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao-admin", Namespace: namespace, Labels: managedLabels(), Data: altered}))
	_, err = ManagementBackupRecipient(ctx, kubeconfigPath, namespace)
	is.True(err != nil && strings.Contains(err.Error(), "refusing to rotate"))
	is.NoErr(connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao-admin", Namespace: namespace, Labels: managedLabels(), Data: admin.Data}))
	unchanged, err := vault.ReadKv(ctx, "secret", managementBackupKeyPath, 0)
	is.NoErr(err)
	is.Equal(unchanged.Version, keyEntry.Version)
	stored, err := vault.ReadKv(ctx, "secret", "test/persistence", 0)
	is.NoErr(err)
	is.Equal(stored.Values, map[string]string{"value": "survives-restart"})
	after, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	is.NoErr(err)
	is.True(maps.Equal(after.Data, admin.Data))
	repeated, err := reconcileComponent(ctx, releases, definition, valuesInput{Settings: settings}, false, false)
	is.NoErr(err)
	is.Equal(repeated.Revision, 1)
	is.True(!repeated.Changed)
}
