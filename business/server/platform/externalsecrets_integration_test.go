//go:build integration

package platform

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"helm.sh/helm/v4/pkg/action"
	helmkube "helm.sh/helm/v4/pkg/kube"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestConvergedExternalSecretsIntegration exercises two real ESO controllers and
// their shared APIs, proving namespace confinement, independent delivery and teardown.
// It requires a disposable cluster without either Borq namespace.
func TestConvergedExternalSecretsIntegration(t *testing.T) {
	path := os.Getenv("BORQ_KUBECONFIG")
	if path == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	// Kubernetes clients require pointers to their REST configuration and request objects.
	kube, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	resources, err := dynamic.NewForConfig(&configuration)
	assert.NoErr(err)
	for _, namespace := range []string{ManagementNamespace, SystemNamespace, "eso-application"} {
		_, err := kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err == nil {
			t.Fatalf("requires a disposable cluster without namespace %s", namespace)
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("inspect disposable cluster: %v", err)
		}
	}
	uninstallEso := func(ctx context.Context, namespace string) error {
		configuration := action.NewConfiguration()
		if err := configuration.Init(&genericclioptions.ConfigFlags{KubeConfig: &path}, namespace, "secrets"); err != nil {
			return err
		}
		uninstall := action.NewUninstall(configuration)
		uninstall.WaitStrategy = helmkube.LegacyStrategy
		uninstall.Timeout = 30 * time.Second
		uninstall.IgnoreNotFound = true
		_, err := uninstall.Run("external-secrets")
		return err
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, namespace := range []string{SystemNamespace, ManagementNamespace} {
			if err := uninstallEso(cleanup, namespace); err != nil {
				t.Errorf("uninstall fixture ESO in %s: %v", namespace, err)
			}
		}
		for _, namespace := range []string{"eso-application", SystemNamespace, ManagementNamespace} {
			if err := kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete fixture namespace %s: %v", namespace, err)
			}
		}
	})
	for _, namespace := range []string{ManagementNamespace, SystemNamespace, "eso-application"} {
		assert.NoErr(prepareNamespace(ctx, connection, namespace))
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: path})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(releases.Close()) })
	assert.NoErr(connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao-admin", Namespace: ManagementNamespace, Data: map[string]string{"token": "converged-eso-fixture"}}))
	for _, object := range []map[string]any{
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "openbao", "namespace": ManagementNamespace}, "spec": map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "eso-vault"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "eso-vault"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "openbao", "image": "quay.io/openbao/openbao:2.7.1", "args": []any{"server", "-dev", "-dev-root-token-id=converged-eso-fixture", "-dev-listen-address=0.0.0.0:8200"}}}}}}},
		{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "openbao", "namespace": ManagementNamespace}, "spec": map[string]any{"selector": map[string]any{"app": "eso-vault"}, "ports": []any{map[string]any{"port": int64(8200), "targetPort": int64(8200)}}}},
	} {
		assert.NoErr(connection.Objects().Apply(ctx, &unstructured.Unstructured{Object: object}))
	}
	assert.NoErr(connection.Deployments().WaitForAvailable(ctx, ManagementNamespace, "openbao"))
	vault, err := OpenManagementVault(ctx, connection, ManagementNamespace)
	assert.NoErr(err)
	assert.NoErr(vault.WaitForActive(ctx, time.Second, time.Minute))
	_, err = vault.WriteKv(ctx, "secret", "borq/platform/isolation", openbao.WriteKvInput{Values: map[string]string{"VALUE": "management-only"}})
	assert.NoErr(err)
	settings, err := normalizeSettings(Settings{Profile: "production", OpenBaoURL: "http://openbao." + ManagementNamespace + ".svc:8200", TelemetryURL: "https://unused.example"})
	assert.NoErr(err)
	var definition component
	for _, candidate := range managementCatalog(settings, false) {
		if candidate.Name == "external-secrets" {
			definition = candidate
		}
	}
	management, err := reconcileComponent(ctx, releases, definition, valuesInput{Settings: settings}, false, false)
	assert.NoErr(err)
	assert.True(management.Changed)
	assert.NoErr(connection.Objects().Apply(ctx, secretStore("borq-management-openbao", ManagementNamespace, settings.OpenBaoURL, "secret", "borq-openbao-admin")))
	assert.NoErr(connection.Objects().Apply(ctx, externalSecret("management-delivery", ManagementNamespace, "borq-management-openbao", "borq/platform/isolation")))
	assert.NoErr(waitForSecret(ctx, connection, ManagementNamespace, "management-delivery", []string{"VALUE"}, time.Minute))
	credentials, err := EnsureClusterCredentials(ctx, CredentialsInput{ManagementKubeconfigPath: path, TargetID: ClusterID("test", "converged")})
	assert.NoErr(err)
	assert.NoErr(connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-openbao", Namespace: SystemNamespace, Data: map[string]string{"token": credentials.VaultToken}}))
	target, err := targetSettings(TargetInput{ID: "converged", Settings: settings})
	assert.NoErr(err)
	application, err := reconcileComponent(ctx, releases, definition, valuesInput{Settings: target, ClusterID: "converged"}, false, false)
	assert.NoErr(err)
	assert.True(application.Changed)
	assert.NoErr(prepareTargetSecrets(ctx, connection, TargetInput{SecretMount: credentials.Mount}, target))
	assert.NoErr(connection.Objects().Apply(ctx, externalSecret("application-delivery", "eso-application", "borq-openbao", "borq/platform/telemetry")))
	assert.NoErr(waitForSecret(ctx, connection, "eso-application", "application-delivery", []string{"TELEMETRY_TOKEN"}, time.Minute))
	conflicts, err := targetComponentConflicts(ctx, connection, releases, SystemNamespace, targetCatalog(false))
	assert.NoErr(err)
	assert.Equal(conflicts["external-secrets"], "")
	// A real authorization decision proves the management service account has no cross-namespace authority.
	for _, namespace := range []string{ManagementNamespace, SystemNamespace, "eso-application"} {
		for _, verb := range []string{"get", "create"} {
			review, err := kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: "system:serviceaccount:" + ManagementNamespace + ":external-secrets-mgmt", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + ManagementNamespace, "system:authenticated"}, ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Resource: "secrets"}}}, metav1.CreateOptions{})
			assert.NoErr(err)
			assert.Equal(review.Status.Allowed, namespace == ManagementNamespace)
		}
	}
	// The management store cannot be addressed from an application's namespace.
	assert.NoErr(connection.Objects().Apply(ctx, externalSecret("forbidden-management", "eso-application", "borq-management-openbao", "borq/platform/isolation")))
	externals := resources.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}).Namespace("eso-application")
	assert.NoErr(wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		events, err := kube.CoreV1().Events("eso-application").List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=forbidden-management,reason=UpdateFailed"})
		if err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if strings.Contains(event.Message, "borq-management-openbao") && strings.Contains(event.Message, "not found") {
				return true, nil
			}
		}
		return false, nil
	}))
	_, err = kube.CoreV1().Secrets("eso-application").Get(ctx, "forbidden-management", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	// Repeat setup must not create additional revisions or adopt shared CRDs.
	for _, values := range []valuesInput{{Settings: settings}, {Settings: target, ClusterID: "converged"}} {
		repeated, err := reconcileComponent(ctx, releases, definition, values, false, false)
		assert.NoErr(err)
		assert.True(!repeated.Changed)
	}
	assert.NoErr(uninstallEso(ctx, ManagementNamespace))
	// Refresh application delivery after management ESO has been removed.
	external, err := externals.Get(ctx, "application-delivery", metav1.GetOptions{})
	assert.NoErr(err)
	external.SetAnnotations(map[string]string{"force-sync": fmt.Sprint(time.Now().UnixNano())})
	_, err = externals.Update(ctx, external, metav1.UpdateOptions{})
	assert.NoErr(err)
	assert.NoErr(kube.CoreV1().Secrets("eso-application").Delete(ctx, "application-delivery", metav1.DeleteOptions{}))
	assert.NoErr(waitForSecret(ctx, connection, "eso-application", "application-delivery", []string{"TELEMETRY_TOKEN"}, time.Minute))
	appSecret, err := connection.Secrets().Get(ctx, "eso-application", "application-delivery")
	assert.NoErr(err)
	assert.Equal(appSecret.Data["TELEMETRY_TOKEN"], credentials.TelemetryToken)
}
