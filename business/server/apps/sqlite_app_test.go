package apps

import (
	"strings"
	"testing"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestSqliteDefaultsAndMerge owns opt-in defaults, backup suspension and single-writer validation.
func TestSqliteDefaultsAndMerge(t *testing.T) {
	assert := is.New(t)
	enabled, disabled, image := true, false, "example/app:v1"
	updates := []AppPatch{{Name: "web", Image: &image, Sqlite: &SqlitePatch{Enabled: &enabled}}}
	configuration, err := Merge(Configuration{}, Patch{Apps: &updates})
	assert.NoErr(err)
	assert.Equal(configuration.Apps[0].Sqlite, SqliteSettings{Enabled: true, Backups: true})
	retained, err := Merge(configuration, Patch{})
	assert.NoErr(err)
	assert.Equal(retained, configuration)
	updates = []AppPatch{{Name: "web", Sqlite: &SqlitePatch{Backups: &disabled}}}
	paused, err := Merge(configuration, Patch{Apps: &updates})
	assert.NoErr(err)
	assert.Equal(paused.Apps[0].Sqlite, SqliteSettings{Enabled: true})
	updates = []AppPatch{{Name: "web", Sqlite: &SqlitePatch{Enabled: &disabled}}}
	_, err = Merge(configuration, Patch{Apps: &updates})
	assert.True(err != nil && strings.Contains(err.Error(), "storage cannot be removed"))
	replicas := 2
	updates = []AppPatch{{Name: "web", Replicas: &replicas}}
	_, err = Merge(configuration, Patch{Apps: &updates})
	assert.True(err != nil && strings.Contains(err.Error(), "at most one replica"))
	replicas, command := 0, "migrate"
	updates = []AppPatch{{Name: "web", Replicas: &replicas, Predeploy: &PredeployPatch{Command: &command}}}
	_, err = Merge(configuration, Patch{Apps: &updates})
	assert.True(err != nil && strings.Contains(err.Error(), "predeploy tasks with backups require one"))
}

// TestRenderSqlite owns the concrete persistent mount, exported filename and backup lifecycle manifests.
func TestRenderSqlite(t *testing.T) {
	assert := is.New(t)
	patch := Patch{}
	assert.NoErr(yaml.UnmarshalStrict([]byte(`apps:
  - name: web
    image: example/app:v1
    sqlite:
      enabled: true
    predeploy:
      command: migrate
`), &patch))
	configuration, err := Merge(Configuration{}, patch)
	assert.NoErr(err)
	storage := ManagementS3{Endpoint: "https://storage.management.example", Region: "us-east-1", Bucket: "application-cluster", Prefix: "archives", PathStyle: true}
	rendered, err := RenderWithEnv(configuration, "application-space", "space-id", "plan-id", "borq", nil, ResourceScope{}, storage)
	assert.NoErr(err)
	deployment := decodeResource[appsv1.Deployment](t, findManifest(t, rendered, "Deployment", "web"))
	assert.Equal(deployment.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	pod := deployment.Spec.Template.Spec
	assert.Equal(pod.Containers[0].Env, []corev1.EnvVar{{Name: "SQLITE_DATABASE_PATH", Value: "/var/lib/borq/sqlite/app.sqlite"}})
	assert.Equal(pod.Containers[0].VolumeMounts, []corev1.VolumeMount{{Name: "borq-sqlite", MountPath: "/var/lib/borq/sqlite"}})
	pods := []corev1.PodSpec{pod, decodeResource[batchv1.CronJob](t, findManifest(t, rendered, "CronJob", "web-sqlite-check")).Spec.JobTemplate.Spec.Template.Spec}
	assert.Equal(len(pod.InitContainers), 3)
	assert.Equal(pod.InitContainers[0].Name, "sqlite-restore")
	assert.True(strings.Contains(pod.InitContainers[0].Args[0], "litestream restore"))
	assert.Equal(pod.InitContainers[1].Name, "sqlite-prepare")
	assert.Equal(pod.InitContainers[2].Name, "litestream")
	assert.Equal(*pod.InitContainers[2].RestartPolicy, corev1.ContainerRestartPolicyAlways)
	assert.Equal(pod.InitContainers[2].EnvFrom[0].SecretRef.Name, "web-litestream")
	assert.Equal(pod.InitContainers[0].EnvFrom[0].SecretRef.Name, "web-litestream")
	assert.Equal(pods[1].InitContainers[0].EnvFrom[0].SecretRef.Name, "web-litestream")
	claim := decodeResource[corev1.PersistentVolumeClaim](t, findManifest(t, rendered, "PersistentVolumeClaim", "web-sqlite"))
	assert.Equal(claim.Spec.Resources.Requests.Storage().String(), "1Gi")
	assert.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce})
	configurationMap := decodeResource[corev1.ConfigMap](t, findManifest(t, rendered, "ConfigMap", "web-litestream"))
	assert.True(strings.Contains(configurationMap.Data["litestream.yml"], "https://storage.management.example"))
	assert.True(strings.Contains(configurationMap.Data["litestream.yml"], "archives/sqlite/space-id/web"))
	assert.True(strings.Contains(configurationMap.Data["litestream.yml"], "${LITESTREAM_AGE_IDENTITY}"))
	assert.True(strings.Contains(configurationMap.Data["litestream.yml"], "${LITESTREAM_AGE_RECIPIENT}"))
	for _, manifest := range rendered.Manifests {
		if manifest["kind"] != "Job" {
			continue
		}
		job := decodeResource[batchv1.Job](t, manifest)
		pods = append(pods, job.Spec.Template.Spec)
		assert.Equal(job.Spec.Template.Spec.Containers[0].Env, pod.Containers[0].Env)
		assert.Equal(job.Spec.Template.Spec.Volumes, pod.Volumes)
		assert.Equal(len(job.Spec.Template.Spec.InitContainers), 2)
	}
	for _, spec := range pods {
		for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
			for _, container := range containers {
				assert.Equal(container.ImagePullPolicy, corev1.PullAlways)
			}
		}
	}
}

// TestInspectSqlite owns live readback and rejects filesystem changes the application model cannot preserve.
func TestInspectSqlite(t *testing.T) {
	assert := is.New(t)
	application := defaultApp("web", nil)
	application.Image = "example/app:v1"
	application.Sqlite = SqliteSettings{Enabled: true, Backups: true}
	configuration, err := Merge(Configuration{Apps: []App{application}}, Patch{})
	assert.NoErr(err)
	rendered, err := RenderWithEnv(configuration, "space", "target", "plan", "borq", nil, ResourceScope{}, ManagementS3{Endpoint: "https://storage.example", Region: "us-east-1", Bucket: "apps", PathStyle: true})
	assert.NoErr(err)
	actual, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(actual, configuration)
	deployment := findManifest(t, rendered, "Deployment", "web")
	pod := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := pod["containers"].([]any)[0].(map[string]any)
	container["env"].([]any)[0].(map[string]any)["value"] = "/another/database.sqlite"
	_, err = Inspect(rendered.Manifests)
	assert.True(err != nil && strings.Contains(err.Error(), "SQLITE_DATABASE_PATH"))
	container["env"].([]any)[0].(map[string]any)["value"] = "/var/lib/borq/sqlite/app.sqlite"
	backup := findManifest(t, rendered, "ConfigMap", "web-litestream")["data"].(map[string]any)
	var backupConfig map[string]any
	assert.NoErr(yaml.Unmarshal([]byte(backup["litestream.yml"].(string)), &backupConfig))
	replica := backupConfig["dbs"].([]any)[0].(map[string]any)["replicas"].([]any)[0].(map[string]any)
	replica["sync-interval"] = "5s"
	encoded, err := yaml.Marshal(backupConfig)
	assert.NoErr(err)
	backup["litestream.yml"] = string(encoded)
	_, err = Inspect(rendered.Manifests)
	assert.True(err != nil && strings.Contains(err.Error(), "backup destination or policy"))
}
