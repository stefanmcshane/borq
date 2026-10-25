package platform

import (
	"testing"

	"github.com/matryer/is"
)

// TestComponentOverrides verifies public chart coordinates and recursive values without losing Borq defaults.
func TestComponentOverrides(t *testing.T) {
	assert := is.New(t)
	settings, err := normalizeSettings(Settings{Profile: "production", Components: map[string]ComponentOverride{"loki": {Repo: "https://charts.example", Chart: "custom-loki", Version: "3.2.1", Values: map[string]any{"singleBinary": map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "1Gi"}}}}}}})
	assert.NoErr(err)
	for _, definition := range managementCatalog(settings, false) {
		if definition.Name != "loki" {
			continue
		}
		assert.Equal(definition.Repository, "https://charts.example")
		assert.Equal(definition.Chart, "custom-loki")
		assert.Equal(definition.Version, "3.2.1")
		values, err := renderComponentValues(definition, valuesInput{Settings: settings})
		assert.NoErr(err)
		resources := values["singleBinary"].(map[string]any)["resources"].(map[string]any)
		assert.Equal(resources["limits"].(map[string]any)["memory"], "1Gi")
		assert.Equal(resources["requests"].(map[string]any)["cpu"], "50m")
	}
	for _, overrides := range []map[string]ComponentOverride{{"unknown": {}}, {"loki": {Repo: "https://secret:password@charts.example"}}, {"loki": {Version: "latest"}}} {
		assert.True(ValidateComponentOverrides(overrides) != nil)
	}
	targetSettings := Settings{Components: map[string]ComponentOverride{"snapshot-controller": {Repo: "https://charts.example/snapshots", Chart: "custom-snapshot-controller", Version: "5.2.1", Values: map[string]any{"controller": map[string]any{"replicaCount": 2}}}}}
	setup, err := GetTargetSetup(targetSettings)
	assert.NoErr(err)
	assert.Equal(setup.Components[len(setup.Components)-1].Name, "snapshot-controller")
	assert.Equal(setup.Components[len(setup.Components)-1].Version, "5.2.1")
	for _, definition := range overrideComponents(targetCatalog(false), targetSettings.Components) {
		if definition.Name != "snapshot-controller" {
			continue
		}
		assert.Equal(definition.Repository, "https://charts.example/snapshots")
		assert.Equal(definition.Chart, "custom-snapshot-controller")
		values, err := renderComponentValues(definition, valuesInput{Settings: targetSettings})
		assert.NoErr(err)
		assert.Equal(values["installCRDs"], true)
		controller := values["controller"].(map[string]any)
		assert.Equal(controller["enabled"], true)
		assert.Equal(controller["replicaCount"], 2)
	}
}

// TestSharedS3PlatformValues verifies management consumes the same configured S3 bucket through secret references.
func TestSharedS3PlatformValues(t *testing.T) {
	assert := is.New(t)
	settings, err := normalizeSettings(Settings{Profile: "production", S3: S3Settings{Endpoint: "https://s3.example", Region: "region-one", Bucket: "shared", Prefix: "borq", PathStyle: true, CredentialsRef: "borq/management/s3/reference"}})
	assert.NoErr(err)
	values, err := renderComponentValues(component{Name: "loki", Values: "loki.yaml"}, valuesInput{Settings: settings})
	assert.NoErr(err)
	loki := values["loki"].(map[string]any)
	storage := loki["storage"].(map[string]any)
	assert.Equal(storage["type"], "s3")
	assert.Equal(storage["bucketNames"].(map[string]any)["chunks"], "shared")
	s3 := storage["s3"].(map[string]any)
	assert.Equal(s3["endpoint"], "s3.example")
	assert.Equal(s3["s3ForcePathStyle"], true)
	assert.Equal(s3["secretAccessKey"], "${secretKey}")
	binary := values["singleBinary"].(map[string]any)
	env := binary["extraEnvFrom"].([]any)[0].(map[string]any)
	assert.Equal(env["secretRef"].(map[string]any)["name"], "borq-management-s3-449b02f1014c04f915e634904160b5ba")
	settings.S3.CredentialsRef = "borq/management/s3/replacement"
	replacement, err := renderComponentValues(component{Name: "loki", Values: "loki.yaml"}, valuesInput{Settings: settings})
	assert.NoErr(err)
	replacementEnv := replacement["singleBinary"].(map[string]any)["extraEnvFrom"].([]any)[0].(map[string]any)
	assert.Equal(replacementEnv["secretRef"].(map[string]any)["name"], "borq-management-s3-711182a807f455d186c1b74d96509f7d")
}

// TestDevelopmentS3Address keeps advertised storage reachable using the existing management node topology.
func TestDevelopmentS3Address(t *testing.T) {
	assert := is.New(t)
	settings, err := normalizeSettings(Settings{ApiUrl: "http://management.example:7833"})
	assert.NoErr(err)
	assert.Equal(settings.S3Endpoint, "http://management.example:30900")
	values, err := renderComponentValues(component{Name: "rustfs", Values: "rustfs.yaml"}, valuesInput{Settings: settings})
	assert.NoErr(err)
	service := values["service"].(map[string]any)
	assert.Equal(service["type"], "NodePort")
	assert.Equal(service["endpoint"].(map[string]any)["nodePort"], float64(30900))
	loki, err := renderComponentValues(component{Name: "loki", Values: "loki.yaml"}, valuesInput{Settings: settings})
	assert.NoErr(err)
	env := loki["singleBinary"].(map[string]any)["extraEnvFrom"].([]any)[0].(map[string]any)
	assert.Equal(env["secretRef"].(map[string]any)["name"], "borq-rustfs")
}
