//go:build integration

package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestInitialManagementSecretIntegration owns immutable input snapshots and
// terminal cleanup through the real Kubernetes API, without fabricating controllers.
func TestInitialManagementSecretIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_INITIAL_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_INITIAL_MANAGEMENT_KUBECONFIG must select an explicit disposable test cluster")
	}
	assert := is.New(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "management.sqlite"), ManagementKubeconfigPath: kubeconfig,
		ManagementEnabled: true, ManagementBootstrapOnly: true, InfrastructureEnabled: true,
		PlatformSettings: platform.Settings{Namespace: "borq-initial-placeholder", ClusterName: "test-management", Profile: "dev"}}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	client, _, err := manager.managementInputClient()
	assert.NoErr(err)
	namespace, err := client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "borq-initial-", Labels: map[string]string{"app.kubernetes.io/managed-by": "borq-integration"}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(ctx, namespace.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &namespace.UID}})
	})
	input.PlatformSettings.Namespace = namespace.Name
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	// Model an interruption after acceptance, before the worker begins setup.
	manager.runtime.cancel()
	configuration := ManagementConfiguration{BorqPublicURL: "https://borq.example.test", Network: &platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}},
		Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "admin@example.test", IssuerURL: "https://id.example.test", ClientID: "client", ClientSecret: "private-initial-oidc-secret", BaseURL: "https://borq.example.test"}}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	task, record, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(record.Value), "private-initial-oidc-secret"))
	assert.True(task.InputRef != "" && task.InputUID != "")
	secret, err := client.CoreV1().Secrets(namespace.Name).Get(t.Context(), task.InputRef, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(string(secret.UID), task.InputUID)
	assert.True(secret.Immutable != nil && *secret.Immutable)
	assert.True(strings.Contains(string(secret.Data["configuration.json"]), "private-initial-oidc-secret"))
	modified := secret.DeepCopy()
	modified.Data["configuration.json"] = []byte(`{}`)
	_, err = client.CoreV1().Secrets(namespace.Name).Update(t.Context(), modified, metav1.UpdateOptions{})
	assert.True(apierrors.IsInvalid(err))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	deadline := time.Now().Add(20 * time.Second)
	finished := false
	for time.Now().Before(deadline) {
		current, err := manager.GetManagementOperation(t.Context(), operation.ID)
		assert.NoErr(err)
		if operationFinished(current.Status) {
			assert.Equal(current.Status, "failed")
			assert.True(strings.Contains(current.Message, "Tailscale requires explicit"))
			finished = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(finished)
	deleted := false
	for time.Now().Before(deadline) {
		_, err = client.CoreV1().Secrets(namespace.Name).Get(t.Context(), task.InputRef, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			deleted = true
			break
		}
		assert.NoErr(err)
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(deleted)
	_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
	assert.True(errors.Is(err, ErrNotFound))
}
