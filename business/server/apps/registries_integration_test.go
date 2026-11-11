//go:build integration

package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestRegistryProjectionIntegration proves saving credentials projects real ESO
// secrets before an app exists, without redeploying existing apps on replacement.
// It also owns immutable rotation, cross-space denial, and management projection.
func TestRegistryProjectionIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	kubernetesClient, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	managementNamespace := platform.ManagementNamespace
	var vault *openbao.Client // The SDK client owns shared authenticated transport state.
	for _, candidate := range []string{platform.ManagementNamespace, "borq"} {
		vault, err = platform.ManagementVault(ctx, kubeconfig, candidate)
		if err == nil {
			managementNamespace = candidate
			break
		}
	}
	assert.NoErr(err)
	admin, err := connection.Secrets().Get(ctx, managementNamespace, "borq-openbao-admin")
	assert.NoErr(err)
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "registry.sqlite"), ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{Namespace: managementNamespace}, PollInterval: 100 * time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	firstID, err := newId()
	assert.NoErr(err)
	secondID, err := newId()
	assert.NoErr(err)
	namespaceOne, namespaceTwo := "borq-registry-test-"+firstID[:12]+"-one", "borq-registry-test-"+firstID[:12]+"-two"
	cluster := Cluster{KubeconfigPath: kubeconfig}
	client, err := newClusterClient(cluster)
	assert.NoErr(err)
	refs := []string{}
	scopes := []string{firstID, secondID}
	namespaces := []string{namespaceOne, namespaceTwo}
	managementManifests := []map[string]any{}
	var managementStore *unstructured.Unstructured // The object API tracks mutable metadata through pointers.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, ref := range refs {
			assert.NoErr(vault.DeleteKvMetadata(cleanup, "secret", ref))
		}
		for _, scope := range scopes {
			tokenRef := "borq/registry-access/" + scope
			entry, err := vault.ReadKv(cleanup, "secret", tokenRef, 0)
			assert.NoErr(err)
			assert.NoErr(vault.DeleteKvMetadata(cleanup, "secret", tokenRef))
			proxy := kubernetesClient.CoreV1().RESTClient()
			body, err := json.Marshal(map[string]string{"token": entry.Values["token"]})
			assert.NoErr(err)
			assert.NoErr(proxy.Post().Namespace(managementNamespace).Resource("services").Name("http:openbao:8200").SubResource("proxy").Suffix("v1/auth/token/revoke").SetHeader("X-Vault-Token", admin.Data["token"]).Body(body).Do(cleanup).Error())
			assert.NoErr(proxy.Delete().Namespace(managementNamespace).Resource("services").Name("http:openbao:8200").SubResource("proxy").Suffix("v1/sys/policies/acl/borq-registry-"+scope).SetHeader("X-Vault-Token", admin.Data["token"]).Do(cleanup).Error())
		}
		for _, manifest := range managementManifests {
			assert.NoErr(connection.Objects().Delete(cleanup, &unstructured.Unstructured{Object: manifest}))
			name, _, _ := unstructured.NestedString(manifest, "spec", "target", "name")
			if err := kubernetesClient.CoreV1().Secrets(managementNamespace).Delete(cleanup, name, metav1.DeleteOptions{}); !apierrors.IsNotFound(err) {
				assert.NoErr(err)
			}
		}
		if managementStore != nil {
			assert.NoErr(connection.Objects().Delete(cleanup, managementStore))
		}
		for _, namespace := range namespaces {
			assert.NoErr(kubernetesClient.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}))
		}
	})
	for _, namespace := range []string{namespaceOne, namespaceTwo} {
		_, err := kubernetesClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
		assert.NoErr(err)
	}
	firstName, firstRef := "", ""
	for index, value := range []struct{ namespace, scope, token string }{
		{namespaceOne, firstID, "first-private-token"},
		{namespaceTwo, secondID, "second-private-token"},
		{namespaceOne, firstID, "rotated-private-token"},
	} {
		registries, err := manager.stageRegistries(ctx, map[string]RegistryAuth{"ghcr.io": {Username: "test-user", Token: value.token}}, value.scope)
		assert.NoErr(err)
		refs = append(refs, registries["ghcr.io"].CredentialsRef)
		previousToken := ""
		if index == 2 {
			previous, err := connection.Secrets().Get(ctx, namespaceOne, "borq-registry-openbao")
			assert.NoErr(err)
			previousToken = previous.Data["token"]
			body, err := json.Marshal(map[string]string{"token": previousToken})
			assert.NoErr(err)
			assert.NoErr(kubernetesClient.CoreV1().RESTClient().Post().Namespace(managementNamespace).Resource("services").Name("http:openbao:8200").SubResource("proxy").Suffix("v1/auth/token/revoke").SetHeader("X-Vault-Token", admin.Data["token"]).Body(body).Do(ctx).Error())
		}
		assert.NoErr(manager.prepareSpaceRegistries(ctx, cluster, Space{ID: value.scope, Namespace: value.namespace}, registries))
		if index == 2 {
			replaced, err := connection.Secrets().Get(ctx, namespaceOne, "borq-registry-openbao")
			assert.NoErr(err)
			assert.True(replaced.Data["token"] != previousToken)
		}
		manifests := registryExternalSecrets(registries, value.namespace, value.scope, "borq-registry-openbao")
		assert.NoErr(client.Apply(ctx, manifests, value.scope))
		name := registrySecretName("ghcr.io", registries["ghcr.io"])
		secret := waitRegistryProjection(t, ctx, kubernetesClient, value.namespace, name)
		assert.Equal(secret.Type, corev1.SecretTypeDockerConfigJson)
		assert.True(secret.Immutable != nil && *secret.Immutable)
		var docker struct {
			Auths map[string]struct{ Auth string }
		}
		assert.NoErr(json.Unmarshal(secret.Data[".dockerconfigjson"], &docker))
		auth, err := base64.StdEncoding.DecodeString(docker.Auths["ghcr.io"].Auth)
		assert.NoErr(err)
		assert.Equal(string(auth), "test-user:"+value.token)
		if index == 0 {
			firstName, firstRef = name, registries["ghcr.io"].CredentialsRef
		}
		if index == 1 {
			_, err := kubernetesClient.CoreV1().Secrets(namespaceTwo).Get(ctx, firstName, metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
		}
		if index == 2 {
			assert.True(name != firstName)
			_, err := kubernetesClient.CoreV1().Secrets(namespaceOne).Get(ctx, firstName, metav1.GetOptions{})
			assert.NoErr(err)
		}
	}
	// A namespace writer cannot project another space's registry document using its own store.
	foreign := registryExternalSecrets(map[string]RegistryAuth{"ghcr.io": {CredentialsRef: firstRef}}, namespaceTwo, secondID, "borq-registry-openbao")[0]
	foreignObject := &unstructured.Unstructured{Object: foreign}
	assert.NoErr(connection.Objects().Apply(ctx, foreignObject))
	for {
		object, err := connection.Objects().Get(ctx, foreignObject)
		assert.NoErr(err)
		conditions, _, err := unstructured.NestedSlice(object.(*unstructured.Unstructured).Object, "status", "conditions")
		assert.NoErr(err)
		denied := false
		for _, item := range conditions {
			condition := item.(map[string]any)
			if condition["type"] == "Ready" && condition["status"] == "False" && condition["reason"] == "SecretSyncedError" {
				denied = true
			}
		}
		if denied {
			break
		}
		if err := manager.waitForPoll(ctx); err != nil {
			t.Fatal("ESO did not report denied cross-space registry access")
		}
	}
	_, err = kubernetesClient.CoreV1().Secrets(namespaceTwo).Get(ctx, firstName, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	tokenSecret, err := connection.Secrets().Get(ctx, namespaceTwo, "borq-registry-openbao")
	assert.NoErr(err)
	for _, check := range []struct {
		method, path string
		status       int
	}{
		{"GET", "secret/data/" + refs[1], 200},
		{"GET", "secret/data/" + firstRef, 403},
		{"PUT", "secret/data/" + refs[1], 403},
		{"LIST", "secret/metadata/borq/registry-auth/" + secondID, 403},
	} {
		method := check.method
		if method == "LIST" {
			method = "GET"
		}
		request := kubernetesClient.CoreV1().RESTClient().Verb(method).Namespace(managementNamespace).Resource("services").Name("http:openbao:8200").SubResource("proxy").Suffix("v1/"+check.path).SetHeader("X-Vault-Token", tokenSecret.Data["token"])
		if check.method == "LIST" {
			request.Param("list", "true")
		}
		if check.method == "PUT" {
			request.Body([]byte(`{"data":{"values":{}}}`))
		}
		code := 0
		result := request.Do(ctx)
		result.StatusCode(&code)
		assert.Equal(code, check.status)
	}
	// Exercise the manager apply path with a public image; no real private GHCR account is needed.
	organization := "registry-test-" + firstID[:12]
	_, err = manager.CreateOrganization(ctx, organization)
	assert.NoErr(err)
	_, err = manager.SetOrganizationManagedAddons(ctx, organization, false)
	assert.NoErr(err)
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: organization, Name: "apps", KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	space, err := manager.CreateSpace(ctx, SpaceInput{Organization: organization, Name: "test", Cluster: "apps"})
	assert.NoErr(err)
	namespaces = append(namespaces, space.Namespace)
	scopes = append(scopes, space.ID)
	saveCredentials := func(token string) map[string]RegistryAuth {
		t.Helper()
		operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: organization, Spaces: []ManagementSpace{{Name: "test", SpaceConfiguration: SpaceConfiguration{Registries: map[string]RegistryAuth{"ghcr.io": {Username: "app-user", Token: token}}}}}}}})
		assert.NoErr(err)
		waitManagementResult(t, manager, operation.ID, "succeeded")
		registries, err := manager.readRegistries(ctx, space.ID)
		assert.NoErr(err)
		refs = append(refs, registries["ghcr.io"].CredentialsRef)
		projection := registryExternalSecrets(registries, space.Namespace, space.ID, "borq-registry-openbao")[0]
		_, err = connection.Objects().Get(ctx, &unstructured.Unstructured{Object: projection})
		assert.NoErr(err)
		return registries
	}
	registries := saveCredentials("app-private-token")
	_ = waitRegistryProjection(t, ctx, kubernetesClient, space.Namespace, registrySecretName("ghcr.io", registries["ghcr.io"]))
	deployments, err := kubernetesClient.AppsV1().Deployments(space.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.Equal(len(deployments.Items), 0)
	plan, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patchFromJson(t, `{"apps":[{"name":"pause","image":"registry.k8s.io/pause:3.10","port":0,"env":{"version":"0"}},{"name":"other","image":"registry.k8s.io/pause:3.10","port":0,"env":{"version":"0"}}]}`)})
	assert.NoErr(err)
	operation, err := manager.Apply(ctx, plan.ID)
	assert.NoErr(err)
	assert.Equal(operation.Phase, "applications")
	for {
		current, err := manager.GetOperation(ctx, operation.ID)
		assert.NoErr(err)
		if current.Status == "succeeded" {
			break
		}
		if operationFinished(current.Status) {
			t.Fatalf("registry-enabled application apply: %s", current.Message)
		}
		if err := manager.waitForPoll(ctx); err != nil {
			t.Fatal("registry-enabled application did not become ready")
		}
	}
	_ = waitRegistryProjection(t, ctx, kubernetesClient, space.Namespace, registrySecretName("ghcr.io", registries["ghcr.io"]))
	before, err := kubernetesClient.AppsV1().Deployments(space.Namespace).Get(ctx, "pause", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(before.Spec.Template.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: registrySecretName("ghcr.io", registries["ghcr.io"])}})
	otherBefore, err := kubernetesClient.AppsV1().Deployments(space.Namespace).Get(ctx, "other", metav1.GetOptions{})
	assert.NoErr(err)
	registries = saveCredentials("replacement-app-token")
	_ = waitRegistryProjection(t, ctx, kubernetesClient, space.Namespace, registrySecretName("ghcr.io", registries["ghcr.io"]))
	after, err := kubernetesClient.AppsV1().Deployments(space.Namespace).Get(ctx, "pause", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(after.UID, before.UID)
	assert.Equal(after.Generation, before.Generation)
	assert.Equal(after.Spec.Template, before.Spec.Template)
	appOperations, err := manager.database.List(ctx, "operation")
	assert.NoErr(err)
	assert.Equal(len(appOperations), 1)
	// Review the same app settings: only generated manifests change when the
	// selected app adopts the space's replacement pull secret.
	updated, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patchFromJson(t, `{"apps":[{"name":"pause","image":"registry.k8s.io/pause:3.10","port":0,"env":{"version":"0"}}]}`)})
	assert.NoErr(err)
	assert.Equal(updated.Configuration, plan.Configuration)
	assert.Equal(updated.ConfigurationDiff, "")
	changedDeployment := false
	for _, difference := range updated.ResourceDiffs {
		if difference.Kind == "Deployment" && difference.Name == "pause" {
			changedDeployment = difference.Action == "update"
		}
		if difference.Kind == "Deployment" && difference.Name == "other" {
			assert.Equal(difference.Action, "unchanged")
		}
	}
	assert.True(changedDeployment)
	for _, manifest := range updated.Manifests {
		if manifest["kind"] == "Deployment" && manifestText(manifest, "metadata", "name") == "pause" {
			pullSecrets, _, err := unstructured.NestedSlice(manifest, "spec", "template", "spec", "imagePullSecrets")
			assert.NoErr(err)
			assert.Equal(pullSecrets, []any{map[string]any{"name": registrySecretName("ghcr.io", registries["ghcr.io"])}})
		}
	}
	version, err := manager.Apply(ctx, updated.ID)
	assert.NoErr(err)
	assert.True(version.ID != operation.ID)
	waitManagerOperation(t, manager, version.ID, "succeeded")
	history, err := manager.ListAppDeployments(ctx, organization, "test", "pause")
	assert.NoErr(err)
	assert.Equal(len(history), 2)
	assert.Equal(history[0].Operation.PlanID, updated.ID)
	assert.Equal(history[0].Configuration, history[1].Configuration)
	otherAfter, err := kubernetesClient.AppsV1().Deployments(space.Namespace).Get(ctx, "other", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(otherAfter.Generation, otherBefore.Generation)
	assert.Equal(otherAfter.Spec.Template, otherBefore.Spec.Template)

	target := EnvironmentGroupTarget{Organization: organization, Space: "test"}
	safe, err := manager.GetSecret(ctx, target, "ghcr.io", "latest")
	assert.NoErr(err)
	assert.Equal(safe.Data, map[string]string{"server": "ghcr.io", "username": "app-user"})
	pending, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patchFromJson(t, `{"apps":[{"name":"pause","replicas":1}]}`)})
	assert.NoErr(err)
	assert.NoErr(manager.DeleteSecret(ctx, target, "ghcr.io", safe.Version))
	for _, name := range []string{before.Spec.Template.Spec.ImagePullSecrets[0].Name, registrySecretName("ghcr.io", registries["ghcr.io"])} {
		_, err := kubernetesClient.CoreV1().Secrets(space.Namespace).Get(ctx, name, metav1.GetOptions{})
		assert.True(apierrors.IsNotFound(err))
	}
	_, err = manager.Apply(ctx, pending.ID)
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patchFromJson(t, `{"apps":[{"name":"pause","replicas":1}]}`)})
	assert.NoErr(err)
	zero := 0
	recreated, err := manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "new-user", "token": "new-token"}, BaseVersion: &zero})
	assert.NoErr(err)
	assert.Equal(recreated.Data, map[string]string{"server": "ghcr.io", "username": "new-user"})
	currentRefs, err := manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	refs = append(refs, currentRefs["ghcr.io"].CredentialsRef)
	_ = waitRegistryProjection(t, ctx, kubernetesClient, space.Namespace, registrySecretName("ghcr.io", currentRefs["ghcr.io"]))
	unchanged, err := kubernetesClient.AppsV1().Deployments(space.Namespace).Get(ctx, "other", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(unchanged.Spec.Template, otherBefore.Spec.Template)
	// A named generic registry uses the URL in Docker config and its own identity
	// for lifecycle management; it never changes the other saved registry secret.
	named, err := manager.PutSecret(ctx, target, "private-mirror", PutSecretInput{Type: "registry", Data: map[string]string{"server": "https://registry.example.invalid:5000/team/", "username": "mirror-user", "password": "mirror-password"}})
	assert.NoErr(err)
	namedRefs, err := manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	refs = append(refs, namedRefs[named.Name].CredentialsRef)
	namedSecret := waitRegistryProjection(t, ctx, kubernetesClient, space.Namespace, registrySecretName(named.Name, namedRefs[named.Name]))
	var docker dockerRegistryConfig
	assert.NoErr(json.Unmarshal(namedSecret.Data[corev1.DockerConfigJsonKey], &docker))
	assert.Equal(len(docker.Auths), 1)
	assert.Equal(docker.Auths["registry.example.invalid:5000/team/"].Auth, base64.StdEncoding.EncodeToString([]byte("mirror-user:mirror-password")))
	namedPlan, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patchFromJson(t, `{"apps":[{"name":"pause","replicas":1}]}`)})
	assert.NoErr(err)
	namedManifests, _ := splitRegistryManifests(namedPlan.Manifests)
	assert.NoErr(manager.verifyRegistryManifests(ctx, space.ID, namedManifests))
	assert.NoErr(manager.DeleteSecret(ctx, target, named.Name, named.Version))
	_, err = kubernetesClient.CoreV1().Secrets(space.Namespace).Get(ctx, namedSecret.Name, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	_, err = kubernetesClient.CoreV1().Secrets(space.Namespace).Get(ctx, registrySecretName("ghcr.io", currentRefs["ghcr.io"]), metav1.GetOptions{})
	assert.NoErr(err)
	// Test the management projection in its controller's real namespace without changing Borq's Deployment.
	registries, err = manager.stageRegistries(ctx, map[string]RegistryAuth{"ghcr.io": {Username: "management-user", Token: "management-private-token"}}, "management")
	assert.NoErr(err)
	refs = append(refs, registries["ghcr.io"].CredentialsRef)
	managementStore, err = registryStore(ctx, connection, managementNamespace, true, managementNamespace)
	assert.NoErr(err)
	managementStore.SetName("borq-registry-test-" + firstID[:12])
	assert.NoErr(connection.Objects().Apply(ctx, managementStore))
	managementManifests = registryExternalSecrets(registries, managementNamespace, "management", managementStore.GetName())
	assert.NoErr(client.Apply(ctx, managementManifests, "management"))
	secret := waitRegistryProjection(t, ctx, kubernetesClient, managementNamespace, registrySecretName("ghcr.io", registries["ghcr.io"]))
	assert.Equal(secret.Type, corev1.SecretTypeDockerConfigJson)
}

func waitRegistryProjection(t *testing.T, ctx context.Context, client kubeclient.Interface, namespace, name string) corev1.Secret {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return *secret
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("ESO did not create the registry Secret")
		case <-ticker.C:
		}
	}
}
