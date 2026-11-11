//go:build integration

package apps

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// TestManagementPocketIdIntegration owns the management YAML -> queue -> real
// provider -> vaulted OIDC -> SQLite reopen boundary. Platform tests separately
// own certificate issuance, network isolation and archive recovery.
// Network settings saved after startup must also reach the published route.
func TestManagementPocketIdIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	t.Cleanup(cancel)
	namespace := platform.ManagementNamespace
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	is.NoErr(err)
	restConfig := connection.RestConfig()
	cluster, err := ctrlclient.New(&restConfig, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	is.NoErr(err)
	transport, err := rest.HTTPClientFor(&restConfig)
	is.NoErr(err)
	organization := "identity-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	s3Endpoint := managementPocketIdRustfsForward(t, ctx, kubeconfig)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "management.sqlite"), ManagementKubeconfigPath: kubeconfig,
		PlatformSettings: platform.Settings{Namespace: namespace, Profile: "dev", StorageClass: "local-path", S3Endpoint: s3Endpoint}, PollInterval: 100 * time.Millisecond}
	manager, err := NewManager(ctx, input)
	is.NoErr(err)
	is.True(manager.runtime.platformSettings.Network.Cloudflare == nil)
	networkWrite, err := documentWrite("management-network-settings", "shared", managementNetworkConfiguration{
		Network: platform.NetworkSettings{Cloudflare: &platform.CloudflareSettings{Exposure: "tunnel", WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "admin@example.test"}},
	}, 0)
	is.NoErr(err)
	is.NoErr(manager.database.Commit(ctx, []sqlite.Write{networkWrite}))
	// The proxy and reopened manager share the active connection through an atomic pointer.
	var active atomic.Pointer[Manager]
	active.Store(&manager)
	t.Cleanup(func() {
		current := active.Load()
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		oidc, _, _ := current.organizationOidc(cleanup, organization)
		stored, _, _ := current.readPocketId(cleanup, oidc.OrganizationID)
		_ = current.Close()
		managementPocketIdCleanup(t, cleanup, cluster, kubeconfig, namespace, s3Endpoint, oidc.OrganizationID, stored)
	})
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := active.Load()
		oidc, _, err := current.organizationOidc(request.Context(), organization)
		if err != nil {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		stored, _, err := current.readPocketId(request.Context(), oidc.OrganizationID)
		if err != nil {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		target, _ := url.Parse(strings.TrimRight(restConfig.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:pocketid-" + stored.ID + ":1411/proxy")
		forward := httputil.NewSingleHostReverseProxy(target)
		forward.Transport = transport.Transport
		forward.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "provider unavailable", http.StatusBadGateway)
		}
		forward.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.Close)
	issuer := strings.Replace(proxy.URL, "127.0.0.1", "localhost", 1)
	privateValue := rand.Text()
	declaration := fmt.Sprintf(`secretGroups:
  - organization: %s
    name: identity-settings
    data:
      APP_NAME: Bootstrap Identity
      SMTP_PASSWORD: %s
borqPublicUrl: http://localhost:7833
organizations:
  - name: %s
    managedPocketId:
      issuerUrl: %s
      adminEmail: admin@example.test
      env:
        name: identity-settings
      values:
        resources:
          requests:
            cpu: 75m
`, organization, privateValue, organization, issuer)
	apply := func(source string) Operation {
		var configuration ManagementConfiguration
		is.NoErr(yaml.UnmarshalStrict([]byte(source), &configuration))
		operation, err := active.Load().ApplyManagementConfiguration(ctx, configuration)
		is.NoErr(err)
		managementPocketIdWait(t, ctx, *active.Load(), operation.ID)
		return operation
	}
	appName := func() string {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/api/application-configuration", nil)
		is.NoErr(err)
		response, err := http.DefaultClient.Do(request)
		is.NoErr(err)
		defer response.Body.Close()
		is.Equal(response.StatusCode, http.StatusOK)
		var configuration []struct{ Key, Value string }
		is.NoErr(json.NewDecoder(response.Body).Decode(&configuration))
		for _, variable := range configuration {
			if variable.Key == "appName" {
				return variable.Value
			}
		}
		return ""
	}
	apply(declaration)
	is.Equal(appName(), "Bootstrap Identity")
	settings, err := manager.GetOrganizationOidc(ctx, organization)
	is.NoErr(err)
	is.Equal(settings.IssuerURL, issuer)
	is.True(settings.ClientSecretConfigured)
	oidc, _, err := manager.organizationOidc(ctx, organization)
	is.NoErr(err)
	stored, _, err := manager.readPocketId(ctx, oidc.OrganizationID)
	is.NoErr(err)
	t.Logf("isolated management identity organization=%s instance=%s", oidc.OrganizationID, stored.ID)
	is.Equal(settings.ClientID, stored.ID)
	is.Equal(stored.Status.State, "ready")
	is.Equal(stored.Status.AdminUsername, "admin@example.test")
	is.True(stored.BackupConfigured)
	is.True(stored.ConfigurationApplied)
	is.Equal(stored.Environment.Version, 1)
	is.Equal(stored.AppliedEnvironmentRef, stored.Environment.CredentialsRef)
	is.Equal(stored.AppliedValuesRef, stored.ValuesRef)
	is.True(stored.ValuesRef != "")
	prepared, err := manager.PrepareOrganizationOidcTest(ctx, organization, "")
	is.NoErr(err)
	authorize, err := url.Parse(prepared.Client.AuthorizationURL("state", "nonce", "verifier"))
	is.NoErr(err)
	is.Equal(authorize.Query().Get("client_id"), stored.ID)
	is.Equal(authorize.Query().Get("redirect_uri"), settings.CallbackURL)
	values, err := manager.readPocketIdEnvironment(ctx, oidc.OrganizationID, stored.Environment)
	is.NoErr(err)
	is.True(values["SMTP_PASSWORD"] == privateValue)
	for _, collection := range []string{"management-task", "management-operation", "organization-pocketid", "organization-oidc"} {
		records, err := manager.database.List(ctx, collection)
		is.NoErr(err)
		for _, record := range records {
			is.True(!strings.Contains(string(record.Value), privateValue))
		}
	}
	key := ctrlclient.ObjectKey{Namespace: namespace, Name: "pocketid-" + stored.ID}
	var route gatewayv1.HTTPRoute
	is.NoErr(cluster.Get(ctx, key, &route))
	is.Equal(len(route.Spec.Rules), 1)
	is.Equal(route.Spec.Rules[0].Filters, []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Forwarded-Proto", Value: "https"}}}}})
	var deployment appsv1.Deployment
	is.NoErr(cluster.Get(ctx, key, &deployment))
	before := deployment.Generation
	apply(declaration)
	is.NoErr(cluster.Get(ctx, key, &deployment))
	is.Equal(deployment.Generation, before)
	repeated, _, err := manager.readPocketId(ctx, oidc.OrganizationID)
	is.NoErr(err)
	is.Equal(repeated.ValuesRef, stored.ValuesRef)
	is.Equal(repeated.Environment, stored.Environment)

	// An established provider can have other users. Re-running initial-admin
	// provisioning would reject this account and make the following update fail.
	secrets, err := manager.pocketIdCredentials(ctx, stored)
	is.NoErr(err)
	userID := uuid.NewString()
	user, err := json.Marshal(map[string]any{"id": userID, "username": "established-user", "email": "user@example.test", "isAdmin": false})
	is.NoErr(err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/api/users", bytes.NewReader(user))
	is.NoErr(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-KEY", secrets["apiKey"])
	response, err := http.DefaultClient.Do(request)
	is.NoErr(err)
	response.Body.Close()
	is.True(response.StatusCode == http.StatusCreated || response.StatusCode == http.StatusOK)
	// Publishing configuration never silently changes a running identity provider.
	publication, err := manager.PutManagementSecret(ctx, organization, "identity-settings", PutSecretInput{Type: "secret-group", Data: map[string]string{"APP_NAME": "Updated Identity", "SMTP_PASSWORD": privateValue}})
	is.NoErr(err)
	is.Equal(publication.Version, 2)
	is.Equal(appName(), "Bootstrap Identity")
	apply(strings.Replace(declaration, "Bootstrap Identity", "Updated Identity", 1))
	is.Equal(appName(), "Updated Identity")
	updated, _, err := manager.readPocketId(ctx, oidc.OrganizationID)
	is.NoErr(err)
	is.Equal(updated.ValuesRef, stored.ValuesRef)
	is.True(updated.Environment.CredentialsRef != stored.Environment.CredentialsRef)
	is.Equal(updated.Environment.Version, 2)
	is.Equal(updated.AppliedEnvironmentRef, updated.Environment.CredentialsRef)
	is.Equal(updated.AppliedValuesRef, updated.ValuesRef)
	is.Equal(updated.ID, stored.ID)
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/api/users/"+userID, nil)
	is.NoErr(err)
	request.Header.Set("X-API-KEY", secrets["apiKey"])
	response, err = http.DefaultClient.Do(request)
	is.NoErr(err)
	response.Body.Close()
	is.Equal(response.StatusCode, http.StatusOK)
	// The UI uses the same managed publication and reconciliation path after setup.
	editable, err := manager.GetOrganizationPocketIdConfiguration(ctx, organization)
	is.NoErr(err)
	is.True(editable.Values["SMTP_PASSWORD"] == privateValue)
	editable.Values["APP_NAME"] = "Edited Identity"
	_, err = manager.SetupOrganizationPocketId(ctx, organization, OrganizationPocketIdInput{IssuerURL: issuer, BaseURL: stored.Status.BaseURL,
		AdminEmail: stored.Status.AdminEmail, OidcRevision: settings.Revision, Configuration: editable.Values,
		ConfigurationRevision: editable.Revision, ConfigurationBaseVersion: editable.BaseVersion})
	is.NoErr(err)
	for {
		updated, _, err = manager.readPocketId(ctx, oidc.OrganizationID)
		is.NoErr(err)
		if updated.Environment.Version == 3 && updated.AppliedEnvironmentRef == updated.Environment.CredentialsRef {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("configuration update did not reach the running provider")
		case <-time.After(100 * time.Millisecond):
		}
	}
	is.Equal(appName(), "Edited Identity")
	is.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	is.NoErr(err)
	active.Store(&reopened)
	retained, _, err := reopened.readPocketId(ctx, oidc.OrganizationID)
	is.NoErr(err)
	is.Equal(retained.ID, stored.ID)
	is.Equal(retained.ValuesRef, updated.ValuesRef)
	is.Equal(retained.Environment, updated.Environment)
	is.Equal(retained.Status.State, "ready")
	_, err = reopened.PrepareOrganizationOidcTest(ctx, organization, "")
	is.NoErr(err)
	exported, err := reopened.GetManagementConfiguration(ctx)
	is.NoErr(err)
	is.Equal(exported.Organizations[0].ManagedPocketId.ValuesRef, updated.ValuesRef)
	is.True(exported.Organizations[0].ManagedPocketId.Values == nil)
	is.Equal(exported.Organizations[0].ManagedPocketId.Env, &EnvReference{Name: "identity-settings", Version: "latest"})
	is.Equal(exported.SecretGroups, []ManagementSecretGroup{{Organization: organization, Name: "identity-settings", Version: "3"}})
}

func managementPocketIdWait(t *testing.T, ctx context.Context, manager Manager, id string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		operation, err := manager.GetManagementOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if operationFinished(operation.Status) {
			if operation.Status != "succeeded" {
				t.Fatalf("management apply %s: %s", operation.Status, operation.Message)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("management Pocket ID apply did not complete")
		case <-ticker.C:
		}
	}
}

func managementPocketIdRustfsForward(t *testing.T, ctx context.Context, kubeconfig string) string {
	t.Helper()
	is := is.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	is.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	is.NoErr(listener.Close())
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", platform.ManagementNamespace, "port-forward", "service/rustfs-svc", fmt.Sprintf("%d:9000", port), "--address=127.0.0.1")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	is.NoErr(command.Start())
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint + "/health/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("management RustFS port-forward did not become ready")
	return ""
}

func managementPocketIdCleanup(t *testing.T, ctx context.Context, cluster ctrlclient.Client, kubeconfig, namespace, endpoint, organizationID string, stored organizationPocketIdRecord) {
	t.Helper()
	if organizationID == "" {
		return
	}
	vault, err := platform.ManagementVault(ctx, kubeconfig, namespace)
	if err != nil {
		t.Error("could not open vault for isolated identity test cleanup")
		return
	}
	if stored.ID != "" {
		name := "pocketid-" + stored.ID
		objects := []ctrlclient.Object{&gatewayv1.HTTPRoute{}, &gatewayv1.Gateway{}, &networkingv1.Ingress{}, &corev1.Service{}, &appsv1.Deployment{}, &corev1.PersistentVolumeClaim{}, &corev1.Secret{}, &corev1.ConfigMap{}}
		for _, object := range objects {
			for _, suffix := range []string{"", "-backup", "-config"} {
				live := object.DeepCopyObject().(ctrlclient.Object)
				if err := cluster.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name + suffix}, live); apierrors.IsNotFound(err) {
					continue
				} else if err != nil {
					t.Error("could not inspect isolated Pocket ID test resource")
					continue
				}
				if live.GetLabels()["borq.io/pocket-id"] != stored.ID || live.GetLabels()["app.kubernetes.io/managed-by"] != "borq" {
					t.Error("refusing to clean a Pocket ID test resource with different ownership")
					continue
				}
				if err := cluster.Delete(ctx, live, ctrlclient.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
					t.Error("could not delete isolated Pocket ID test resource")
				}
			}
		}
		for {
			var pods corev1.PodList
			if err := cluster.List(ctx, &pods, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{"borq.io/pocket-id": stored.ID}); err != nil {
				t.Error("could not wait for isolated Pocket ID test pods")
				return
			}
			if len(pods.Items) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Error("isolated Pocket ID test pods did not stop")
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		managementPocketIdDeleteArchive(t, ctx, vault, endpoint, stored)
	}
	for _, purpose := range []string{"organization-pocketid", "organization-pocketid-values", "organization-oidc", "organization-management-secret-group", "organization-pocketid-environment", "management-secret-groups"} {
		managementPocketIdDeleteVault(t, ctx, vault, "borq/"+purpose+"/"+organizationID)
	}
}

func managementPocketIdDeleteVault(t *testing.T, ctx context.Context, vault *openbao.Client, path string) {
	t.Helper()
	keys, err := vault.ListKvKeys(ctx, "secret", path)
	if err != nil {
		t.Error("could not list isolated identity test vault receipts")
		return
	}
	for _, key := range keys {
		if strings.HasSuffix(key, "/") {
			managementPocketIdDeleteVault(t, ctx, vault, path+"/"+strings.TrimSuffix(key, "/"))
		} else if err := vault.DeleteKvMetadata(ctx, "secret", path+"/"+key); err != nil {
			t.Error("could not remove isolated identity test vault receipt")
		}
	}
}

func managementPocketIdDeleteArchive(t *testing.T, ctx context.Context, vault *openbao.Client, endpoint string, stored organizationPocketIdRecord) {
	t.Helper()
	root, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil {
		t.Error("could not read storage credentials for test cleanup")
		return
	}
	keys := aws.Credentials{AccessKeyID: root.Values["RUSTFS_ACCESS_KEY"], SecretAccessKey: root.Values["RUSTFS_SECRET_KEY"]}
	client := s3.New(s3.Options{BaseEndpoint: &endpoint, Region: "us-east-1", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(keys.AccessKeyID, keys.SecretAccessKey, ""), RetryMaxAttempts: 2})
	bucket := "borq-pocketid-" + stored.ID
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			if !stored.BackupConfigured {
				return
			}
			t.Error("could not list isolated Pocket ID test archive")
			return
		}
		for _, object := range page.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: object.Key}); err != nil {
				t.Error("could not delete isolated Pocket ID archive object")
				return
			}
		}
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); err != nil {
		t.Error("could not delete isolated Pocket ID archive bucket")
	}
	backup, err := vault.ReadKv(ctx, "secret", stored.credentialsPath()+"/backup", 1)
	accessKey := backup.Values["accessKey"]
	if err != nil {
		// The account ID is deterministic even if a prior interrupted cleanup
		// already removed its vault receipt. It remains scoped to this exact bucket.
		mac := hmac.New(sha256.New, []byte(keys.SecretAccessKey))
		_, _ = mac.Write([]byte("borq/storage/access/" + bucket + "/sqlite"))
		accessKey = "bq" + hex.EncodeToString(mac.Sum(nil))[:30]
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint+"/rustfs/admin/v3/delete-service-account?accessKey="+url.QueryEscape(accessKey), nil)
	if err != nil {
		t.Error("could not build isolated backup account cleanup")
		return
	}
	digest := sha256.Sum256(nil)
	hash := hex.EncodeToString(digest[:])
	request.Header.Set("X-Amz-Content-Sha256", hash)
	if err := v4.NewSigner().SignHTTP(ctx, keys, request, hash, "s3", "us-east-1", time.Now()); err != nil {
		t.Error("could not sign isolated backup account cleanup")
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Error("could not remove isolated backup account")
		return
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		t.Errorf("isolated backup account cleanup returned status %d", response.StatusCode)
	}
}
