package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRegistryCredentialsScopeAndPersistence verifies saving credentials creates
// projection resources before any app exists, persists only references across
// reopening, and rejects reuse by another space.
func TestRegistryCredentialsScopeAndPersistence(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagementStorageFixture(t)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	input := NewInput{DatabasePath: path, ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	cluster := newManagerCluster(t, managerClusterOptions{})
	createManagerEnvironment(t, manager, cluster, "production")
	registries := map[string]RegistryAuth{"ghcr.io": {Username: "github-user", Token: "private-registry-token"}}
	operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "acme", Spaces: []ManagementSpace{{Name: "production", SpaceConfiguration: SpaceConfiguration{Registries: registries}}}}}})
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	configuration, err := manager.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	auth := configuration.Organizations[0].Spaces[0].Registries["ghcr.io"]
	assert.Equal(auth.Username, "")
	assert.Equal(auth.Token, "")
	assert.True(strings.HasPrefix(auth.CredentialsRef, "borq/registry-auth/"+initialSpace("acme", "production").ID+"/"))
	space, err := manager.GetSpace(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(len(space.Configuration.Apps), 0)
	appOperations, err := manager.database.List(ctx, "operation")
	assert.NoErr(err)
	assert.Equal(len(appOperations), 0)
	cluster.mutex.Lock()
	projection := cloneManagerResource(cluster.resources["/apis/external-secrets.io/v1/namespaces/"+space.Namespace+"/externalsecrets/"+registrySecretName("ghcr.io", auth)])
	store := cloneManagerResource(cluster.resources["/apis/external-secrets.io/v1/namespaces/"+space.Namespace+"/secretstores/borq-registry-openbao"])
	cluster.mutex.Unlock()
	assert.Equal(manifestText(projection, "spec", "secretStoreRef", "name"), "borq-registry-openbao")
	assert.Equal(manifestText(store, "spec", "provider", "vault", "auth", "tokenSecretRef", "name"), "borq-registry-openbao")
	assert.True(projection["status"] == nil)
	assert.True(store["status"] == nil)
	vault, err := platform.ManagementVault(ctx, fixture.kubeconfig, "borq")
	assert.NoErr(err)
	entry, err := vault.ReadKv(ctx, "secret", auth.CredentialsRef, 1)
	assert.NoErr(err)
	var docker struct {
		Auths map[string]struct{ Auth string }
	}
	assert.NoErr(json.Unmarshal([]byte(entry.Values[".dockerconfigjson"]), &docker))
	decoded, err := base64.StdEncoding.DecodeString(docker.Auths["ghcr.io"].Auth)
	assert.NoErr(err)
	assert.Equal(string(decoded), "github-user:private-registry-token")
	for _, kind := range []string{"registry-auth", "management-task", "management-operation"} {
		records, err := manager.database.List(ctx, kind)
		assert.NoErr(err)
		for _, record := range records {
			assert.True(!strings.Contains(string(record.Value), "private-registry-token"))
			assert.True(!strings.Contains(string(record.Value), docker.Auths["ghcr.io"].Auth))
		}
	}
	_, err = manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "acme", Spaces: []ManagementSpace{{Name: "other", SpaceConfiguration: SpaceConfiguration{Registries: map[string]RegistryAuth{"ghcr.io": auth}}}}}}})
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "belong to this scope"))
	_, err = manager.stageRegistries(ctx, map[string]RegistryAuth{"ghcr.io": auth}, "management")
	assert.True(errors.Is(err, ErrInvalid))
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer reopened.Close()
	persisted, err := reopened.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	assert.Equal(persisted.Organizations[0].Spaces[0].Registries["ghcr.io"], auth)
	operation, err = reopened.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "acme", Spaces: []ManagementSpace{{Name: "production"}}}}})
	assert.NoErr(err)
	waitManagementResult(t, reopened, operation.ID, "succeeded")
	retained, err := reopened.readRegistries(ctx, initialSpace("acme", "production").ID)
	assert.NoErr(err)
	assert.Equal(retained["ghcr.io"], auth)
}

// TestRegistrySaveResumesLaterSpace preserves the meaning of management action
// checkpoints written before registry-only saves stopped redeploying apps.
func TestRegistrySaveResumesLaterSpace(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "resume.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	cluster := newManagerCluster(t, managerClusterOptions{})
	createManagerEnvironment(t, manager, cluster, "production")
	configuration, err := manager.stageManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "acme", Spaces: []ManagementSpace{
		{Name: "production", SpaceConfiguration: SpaceConfiguration{Registries: map[string]RegistryAuth{"ghcr.io": {Username: "user", Token: "first-token"}}}},
		{Name: "secondary", Cluster: "local", SpaceConfiguration: SpaceConfiguration{Registries: map[string]RegistryAuth{"ghcr.io": {Username: "user", Token: "second-token"}}}},
	}}}})
	assert.NoErr(err)
	now := time.Now().UTC()
	operation := Operation{ID: "resume-registry-save", Status: "running", Phase: "apps", StartedAt: now, UpdatedAt: now}
	// Legacy order: organization, first space, its credentials, its apps, second space.
	task := managementTask{Configuration: configuration, Next: 4, Deadline: now.Add(time.Minute)}
	operationWrite, err := documentWrite("management-operation", operation.ID, operation, 0)
	assert.NoErr(err)
	taskWrite, err := documentWrite("management-task", operation.ID, task, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{operationWrite, taskWrite}))
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer reopened.Close()
	waitManagementResult(t, reopened, operation.ID, "succeeded")
	space, err := reopened.GetSpace(ctx, "acme", "secondary")
	assert.NoErr(err)
	registries, err := reopened.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	assert.Equal(registries, configuration.Organizations[0].Spaces[1].Registries)
	operations, err := reopened.database.List(ctx, "operation")
	assert.NoErr(err)
	assert.Equal(len(operations), 0)
}

// TestRegistryAuthValidation rejects missing servers, partial credentials,
// path traversal, and attempts to combine secrets with a reference before vault access.
func TestRegistryAuthValidation(t *testing.T) {
	for _, registries := range []map[string]RegistryAuth{
		{"docker.io": {Username: "user", Token: "token"}},
		{"ghcr.io": {Username: "user"}},
		{"ghcr.io": {Token: "token"}},
		{"ghcr.io": {Username: "user", Token: "\n"}},
		{"ghcr.io": {Username: "user:other", Token: "token"}},
		{"ghcr.io": {CredentialsRef: "borq/registry-auth/management/../../other"}},
		{"ghcr.io": {Username: "user", Token: "token", CredentialsRef: "borq/registry-auth/management/" + strings.Repeat("a", 32)}},
	} {
		assert := is.New(t)
		assert.True(errors.Is(validateManagementConfiguration(ManagementConfiguration{Registries: registries}), ErrInvalid))
	}
}

// TestRegistryRendering covers private images in deployments and predeploy jobs,
// namespace-local ESO bindings, credential rotation, and application readback.
func TestRegistryRendering(t *testing.T) {
	assert := is.New(t)
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"ghcr.io/example/web:v1","port":8080,"sqlite":{"enabled":true},"predeploy":{"command":"./migrate"}}]}`))
	assert.NoErr(err)
	rendered, err := RenderWithEnv(configuration, "space-one", "target-one", "plan-one", "borq", nil, ResourceScope{}, ManagementS3{Endpoint: "https://storage.example", Region: "region", Bucket: "backups", PathStyle: true})
	assert.NoErr(err)
	registries := map[string]RegistryAuth{"ghcr.io": {CredentialsRef: "borq/registry-auth/target-one/" + strings.Repeat("a", 32)}}
	addRegistryPullSecrets(rendered.Manifests, registries)
	addRegistryPullSecrets(rendered.Manifests, registries)
	rendered.Manifests = append(rendered.Manifests, registryExternalSecrets(registries, "space-one", "target-one", "borq-registry-openbao")...)
	deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "web"))
	job := decodeResource[batchv1.Job](t, predeployManifest(t, rendered))
	assert.Equal(len(deployment.Spec.Template.Spec.ImagePullSecrets), 1)
	assert.Equal(job.Spec.Template.Spec.ImagePullSecrets, deployment.Spec.Template.Spec.ImagePullSecrets)
	cron := decodeResource[batchv1.CronJob](t, findManifest(t, rendered, "CronJob", "web-sqlite-check"))
	assert.Equal(cron.Spec.JobTemplate.Spec.Template.Spec.ImagePullSecrets, deployment.Spec.Template.Spec.ImagePullSecrets)
	secret := rendered.Manifests[len(rendered.Manifests)-1]
	name, _, err := unstructured.NestedString(secret, "spec", "target", "name")
	assert.NoErr(err)
	assert.Equal(deployment.Spec.Template.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: name}})
	secretType, _, err := unstructured.NestedString(secret, "spec", "target", "template", "type")
	assert.NoErr(err)
	assert.Equal(secretType, "kubernetes.io/dockerconfigjson")
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
	rotated := map[string]RegistryAuth{"ghcr.io": {CredentialsRef: "borq/registry-auth/target-one/" + strings.Repeat("b", 32)}}
	addRegistryPullSecrets(rendered.Manifests, rotated)
	updated := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "web"))
	assert.Equal(len(updated.Spec.Template.Spec.ImagePullSecrets), 1)
	assert.True(updated.Spec.Template.Spec.ImagePullSecrets[0].Name != name)
}
