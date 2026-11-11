//go:build integration

package apps

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestPocketIdRetainedRecoveryIntegration proves destroy and ready-provider recovery
// reopen the real Pocket ID database without recreating its account/client/secret.
// Gateway resources are read back from Kubernetes; no ingress controller, TLS,
// public DNS or backup replication success is claimed by this HTTP-only test.
func TestPocketIdRetainedRecoveryIntegration(t *testing.T) {
	path := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if path == "" {
		t.Skip("isolated BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	// This destructive lifecycle test must never borrow an existing installation's binding.
	_, err = client.RbacV1().ClusterRoleBindings().Get(ctx, "borq", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("an isolated cluster without an existing Borq bootstrap binding is required")
	}
	namespace, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "borq-id-recovery-"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := client.CoreV1().Namespaces().Delete(cleanup, namespace.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Error("remove isolated Pocket ID recovery namespace")
		}
	})
	identity := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner := strings.ReplaceAll(uuid.NewString(), "-", "")
	credentials := map[string]string{"apiKey": rand.Text() + rand.Text(), "encryptionKey": rand.Text() + rand.Text(), "clientSecret": rand.Text() + rand.Text()}
	archive, err := sqliteworkload.NewArchiveKey()
	assert.NoErr(err)
	input := platform.PocketIdInput{ArchiveKey: archive, Connection: connection, Namespace: namespace.Name, InstanceID: identity, IssuerURL: "http://localhost", StorageClass: "local-path", Timeout: 2 * time.Minute,
		EncryptionKey: credentials["encryptionKey"], ApiKey: credentials["apiKey"], Bootstrapping: true,
		// The sidecar runs with explicit fixture credentials. There is no S3 service
		// here: real archive/restore behavior belongs to platform's backup integration.
		BackupCredentials: objectstore.Credentials{AccessKey: "recovery-fixture", SecretKey: strings.Repeat("backup-fixture-", 3)},
	}
	_, err = platform.InstallPocketId(ctx, input)
	assert.NoErr(err)
	stored := organizationPocketIdRecord{ID: identity, OrganizationID: owner, Organization: "recovery", Status: OrganizationPocketId{State: "ready", IssuerURL: input.IssuerURL, BaseURL: "https://borq.example.test", AdminUsername: "recovery-owner", AdminEmail: "recovery-owner@example.test"}, CredentialsStored: true}
	manager := Manager{runtime: &managerRuntime{managementKubeconfigPath: path, platformSettings: platform.Settings{Namespace: namespace.Name}}}
	provider, err := manager.pocketIdClient(stored, credentials["apiKey"])
	assert.NoErr(err)
	admin := pocketid.Admin{ID: stored.adminID(), Username: stored.Status.AdminUsername, Email: stored.Status.AdminEmail}
	callback := stored.Status.BaseURL + "/api/auth/oidc/callback/" + stableId(owner+"/"+input.IssuerURL)
	clientInput := pocketid.OidcClient{ID: identity, Name: "Borq recovery", CallbackURL: callback, Secret: credentials["clientSecret"], LogoutCallbackURL: stored.Status.BaseURL + "/login", LaunchURL: stored.Status.BaseURL + "/"}
	assert.NoErr(provider.EnsureAdmin(ctx, admin))
	assert.NoErr(provider.EnsureClient(ctx, clientInput))
	input.Bootstrapping = false
	_, err = platform.InstallPocketId(ctx, input)
	assert.NoErr(err)
	assert.NoErr(platform.PublishPocketId(ctx, input))
	runtimeClient, err := ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	assert.NoErr(err)
	name := "pocketid-" + identity
	key := ctrlclient.ObjectKey{Namespace: namespace.Name, Name: name}
	var initialGateway gatewayv1.Gateway
	var initialRoute gatewayv1.HTTPRoute
	assert.NoErr(runtimeClient.Get(ctx, key, &initialGateway))
	assert.NoErr(runtimeClient.Get(ctx, key, &initialRoute))
	originalDeployment, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	originalClaim, err := client.CoreV1().PersistentVolumeClaims(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	originalSecret, err := client.CoreV1().Secrets(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	transport, err := rest.HTTPClientFor(&configuration)
	assert.NoErr(err)
	base := strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace.Name + "/services/http:" + name + ":1411/proxy"
	var originalAccount struct {
		ID, Username, Email string
		IsAdmin             bool `json:"isAdmin"`
	}
	pocketIdRecoveryRead(t, ctx, transport, base, "/api/users/"+admin.ID, credentials["apiKey"], &originalAccount)
	var originalSecrets []struct {
		ID     string `json:"id"`
		Active bool   `json:"isActive"`
	}
	pocketIdRecoveryRead(t, ctx, transport, base, "/api/oidc/clients/"+identity+"/secrets", credentials["apiKey"], &originalSecrets)
	assert.Equal(len(originalSecrets), 1)
	assert.True(originalSecrets[0].Active)
	destroyInput := platform.ManagementDestroyInput{KubeconfigPath: path, Namespace: namespace.Name, OperationID: "pocketid-recovery-" + identity, Timeout: 2 * time.Minute}
	destroyPlan, err := platform.PlanManagementDestroy(ctx, destroyInput)
	assert.NoErr(err)
	result, err := platform.DestroyManagementResources(ctx, destroyInput, destroyPlan)
	assert.NoErr(err)
	assert.True(result.Executed)
	_, err = client.AppsV1().Deployments(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	assert.True(apierrors.IsNotFound(runtimeClient.Get(ctx, key, &gatewayv1.Gateway{})))
	assert.True(apierrors.IsNotFound(runtimeClient.Get(ctx, key, &gatewayv1.HTTPRoute{})))
	retained, err := client.CoreV1().PersistentVolumeClaims(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retained.UID, originalClaim.UID)
	input.RequireExistingStorage = true
	_, err = platform.InstallPocketId(ctx, input)
	assert.NoErr(err)
	// Invalid retained credentials must reach VerifyClient and leave all public
	// routes absent. The succeeding recovery below uses the original private key.
	wrong := map[string]string{"apiKey": credentials["apiKey"], "clientSecret": strings.Repeat("wrong-secret-", 4)}
	err = manager.republishReadyPocketId(ctx, stored, input, wrong)
	assert.True(err != nil && strings.Contains(err.Error(), "Could not verify the retained Pocket ID identity"))
	assert.True(apierrors.IsNotFound(runtimeClient.Get(ctx, key, &gatewayv1.Gateway{})))
	assert.True(apierrors.IsNotFound(runtimeClient.Get(ctx, key, &gatewayv1.HTTPRoute{})))
	assert.NoErr(manager.republishReadyPocketId(ctx, stored, input, credentials))
	replacement, err := client.AppsV1().Deployments(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(replacement.UID != originalDeployment.UID)
	var gateway gatewayv1.Gateway
	var route gatewayv1.HTTPRoute
	assert.NoErr(runtimeClient.Get(ctx, key, &gateway))
	assert.True(gateway.UID != initialGateway.UID)
	assert.NoErr(runtimeClient.Get(ctx, key, &route))
	assert.True(route.UID != initialRoute.UID)
	currentClaim, err := client.CoreV1().PersistentVolumeClaims(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(currentClaim.UID, originalClaim.UID)
	currentSecret, err := client.CoreV1().Secrets(namespace.Name).Get(ctx, name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(currentSecret.UID, originalSecret.UID)
	assert.Equal(currentSecret.Data, originalSecret.Data)
	var savedAccount struct {
		ID, Username, Email string
		IsAdmin             bool `json:"isAdmin"`
	}
	pocketIdRecoveryRead(t, ctx, transport, base, "/api/users/"+admin.ID, credentials["apiKey"], &savedAccount)
	assert.Equal(savedAccount, originalAccount)
	assert.Equal(savedAccount.ID, admin.ID)
	assert.Equal(savedAccount.Username, admin.Username)
	assert.True(savedAccount.IsAdmin)
	var savedSecrets []struct {
		ID     string `json:"id"`
		Active bool   `json:"isActive"`
	}
	pocketIdRecoveryRead(t, ctx, transport, base, "/api/oidc/clients/"+identity+"/secrets", credentials["apiKey"], &savedSecrets)
	assert.Equal(savedSecrets, originalSecrets)
	assert.NoErr(provider.VerifyClient(ctx, clientInput))
}

// pocketIdRecoveryRead observes the real database through Kubernetes's private service transport.
func pocketIdRecoveryRead(t *testing.T, ctx context.Context, client *http.Client, base, path, key string, output any) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	is.New(t).NoErr(err)
	request.Header.Set("X-API-KEY", key)
	response, err := client.Do(request)
	is.New(t).NoErr(err)
	defer response.Body.Close()
	is.New(t).Equal(response.StatusCode, http.StatusOK)
	is.New(t).NoErr(json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output))
}
