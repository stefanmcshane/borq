package platform

import (
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestComponentCatalog protects resolved Helm input, scope membership, and separation from editable overrides.
func TestComponentCatalog(t *testing.T) {
	assert := is.New(t)
	settings := Settings{ApiUrl: "https://management.example", Namespace: "infrastructure", Components: map[string]ComponentOverride{
		"alloy": {Repo: "https://charts.example", Chart: "collector", Version: "2.3.4", Values: map[string]any{"controller": map[string]any{"type": "deployment"}}},
	}}
	management, err := GetComponentCatalog(ComponentCatalogInput{Scope: ComponentScopeManagement, Settings: settings})
	assert.NoErr(err)
	assert.Equal(management.Profile, "dev")
	assert.Equal(management.Namespace, "infrastructure")
	assert.Equal(management.ClusterID, "")
	assert.Equal(management.Components[0].Name, "openbao")
	assert.Equal(management.Components[0].Version, "0.30.3")
	assert.Equal(management.Components[0].Values["server"].(map[string]any)["service"].(map[string]any)["nodePort"], float64(30200))
	for _, component := range management.Components {
		assert.True(component.Description != "")
		assert.True(len(component.Values) > 0)
		assert.True(component.Name != "alloy")
		_, injected := component.Values["namespace"]
		assert.True(!injected)
	}
	target, err := GetComponentCatalog(ComponentCatalogInput{Scope: ComponentScopeTarget, Settings: settings, ClusterID: "registered-target"})
	assert.NoErr(err)
	assert.Equal(target.ClusterID, "registered-target")
	var alloy ComponentConfiguration
	for _, component := range target.Components {
		if component.Name == "alloy" {
			alloy = component
		}
		assert.True(component.Name != "openbao")
	}
	assert.Equal(alloy.Repository, "https://charts.example")
	assert.Equal(alloy.Chart, "collector")
	assert.Equal(alloy.Version, "2.3.4")
	assert.Equal(alloy.Override, settings.Components["alloy"])
	assert.Equal(alloy.Values["controller"].(map[string]any)["type"], "deployment")
	content := alloy.Values["alloy"].(map[string]any)["configMap"].(map[string]any)["content"].(string)
	assert.True(strings.Contains(content, "http://management.example:31834/api/v1/ingest/registered-target/loki/api/v1/push"))
	assert.True(strings.Contains(content, "http://management.example:31834/api/v1/ingest/registered-target/api/v1/write"))
	assert.True(strings.Contains(content, `borq_cluster = "registered-target"`))
	postgres, err := GetComponentCatalog(ComponentCatalogInput{Scope: ComponentScopePostgres, Settings: Settings{Profile: "production"}})
	assert.NoErr(err)
	assert.Equal(len(postgres.Components), 2)
	assert.Equal(postgres.Components[0].Name, "cloudnative-pg")
	assert.Equal(postgres.Components[1].Name, "plugin-barman-cloud")
	production, err := GetComponentCatalog(ComponentCatalogInput{Scope: ComponentScopeManagement, Settings: Settings{Profile: "production"}})
	assert.NoErr(err)
	for _, component := range production.Components {
		assert.True(component.Name != "rustfs")
	}
	for _, test := range []struct {
		input ComponentCatalogInput
		guard string
	}{
		{ComponentCatalogInput{Scope: "unknown"}, "unsupported component scope"},
		{ComponentCatalogInput{Scope: ComponentScopeTarget, Settings: settings}, "target cluster identity is required"},
		{ComponentCatalogInput{Scope: ComponentScopeTarget, ClusterID: "registered-target"}, "requires a reachable management OpenBao URL"},
		{ComponentCatalogInput{Scope: ComponentScopeTarget, ClusterID: "registered-target", Settings: Settings{OpenBaoURL: "https://vault.example"}}, "requires authenticated management telemetry URLs"},
		{ComponentCatalogInput{Scope: ComponentScopeManagement, Settings: Settings{Profile: "unknown"}}, "unsupported platform profile"},
	} {
		_, err := GetComponentCatalog(test.input)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), test.guard))
	}
}
