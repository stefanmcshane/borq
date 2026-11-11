package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/foundations/logger"
	"sigs.k8s.io/yaml"
)

// TestConfigurationTemplateCommands prints API-valid bodies without profiles, files, credentials or network calls.
func TestConfigurationTemplateCommands(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	assert.NoErr(os.MkdirAll(filepath.Join(directory, "borq"), 0700))
	assert.NoErr(os.WriteFile(filepath.Join(directory, "borq", "config.yaml"), []byte("invalid: ["), 0600))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	specification, err := appapi.GetSwagger()
	assert.NoErr(err)
	cases := map[string]string{
		"management apply":   "ManagementConfiguration",
		"org create":         "OrganizationOnboardingInput",
		"org members update": "OrganizationMemberInput",
		"org roles create":   "OrganizationRoleInput", "org roles update": "OrganizationRoleInput",
		"cluster link": "ClusterLinkFile", "space create": "SpaceInput", "plan": "Patch", "apply": "SpaceConfiguration",
		"secrets put": "PutSecretInput", "management secrets put": "PutSecretInput",
		"environment-group create": "PublishEnvironmentGroupInput", "environment-group clone": "CloneEnvironmentGroupInput",
		"am postgres create": "PostgresInput", "am postgres apply": "SpaceConfiguration", "am postgres backup restore": "PostgresRestoreInput",
		"addons managed postgres create": "PostgresInput", "addons managed postgres apply": "SpaceConfiguration", "addons managed postgres backup restore": "PostgresRestoreInput",
		"ac install": "CustomChartInput", "ac apply": "CustomChartInput", "ac defaults": "ChartInput",
		"addons custom install": "CustomChartInput", "addons custom apply": "CustomChartInput", "addons custom defaults": "ChartInput",
	}
	for path, schema := range cases {
		t.Run(path, func(t *testing.T) {
			assert := is.New(t)
			command := NewRootCommand(context.Background(), logger.Logger{}, Config{})
			var output, diagnostics bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&diagnostics)
			arguments := append(strings.Fields(path), "--template=full", "--template-format", "json", "--api-url", server.URL)
			command.SetArgs(arguments)
			assert.NoErr(command.Execute())
			var value map[string]any
			assert.NoErr(json.Unmarshal(output.Bytes(), &value))
			assert.NoErr(specification.Components.Schemas[schema].Value.VisitJSON(value, openapi3.VisitAsRequest()))
			for name, property := range specification.Components.Schemas[schema].Value.Properties {
				if !property.Value.ReadOnly {
					_, present := value[name]
					assert.True(present)
				}
			}
			if schema == "ManagementConfiguration" {
				backups := value["backups"].(map[string]any)
				_, hasEncryptionKey := backups["encryptionKey"]
				_, hasRecipient := backups["recipient"]
				assert.True(!hasEncryptionKey)
				assert.True(!hasRecipient)
				for _, field := range []string{"backups", "offsite"} {
					settings := value[field].(map[string]any)
					_, hasStatus := settings["status"]
					assert.True(!hasStatus)
				}
			}
			assert.Equal(diagnostics.String(), "")
		})
	}
	assert.Equal(requests.Load(), int64(0))
}

// TestConfigurationTemplateExamples exposes OIDC overrides as directly consumable organization files and retains normal command validation.
func TestConfigurationTemplateExamples(t *testing.T) {
	assert := is.New(t)
	output, _, err := runApplicationCommand(t, "org", "create", "--template=oidc")
	assert.NoErr(err)
	var input appapi.OrganizationOnboardingInput
	assert.NoErr(yaml.UnmarshalStrict([]byte(output), &input))
	assert.True(input.Oidc != nil && input.AdminEmail != nil)
	assert.True(input.PocketID == nil && input.PocketIDOrganization == nil)
	assert.True(strings.Contains(output, "borqPublicUrl:"))
	assert.True(!strings.Contains(output, "baseUrl:"))
	assert.Equal(input.Oidc.IssuerURL, "https://id.example.com")
	assert.True(strings.Contains(output, "embedded Borq API"))
	output, _, err = runApplicationCommand(t, "template", "PutOrganization", "--example", "oidc", "--format", "json")
	assert.NoErr(err)
	var value map[string]any
	assert.NoErr(json.Unmarshal([]byte(output), &value))
	assert.True(value["oidc"] != nil)
	output, _, err = runApplicationCommand(t, "template")
	assert.NoErr(err)
	assert.True(strings.Contains(output, "ApplyManagementConfiguration"))
	_, _, err = runApplicationCommand(t, "org", "create", "--template=unknown")
	assert.True(err != nil && strings.Contains(err.Error(), "available examples"))
	_, _, err = runApplicationCommand(t, "apply", "--template-format", "json")
	assert.True(err != nil && strings.Contains(err.Error(), "requires --template"))
	_, _, err = runApplicationCommand(t, "org", "create")
	assert.True(err != nil)
	_, _, err = runApplicationCommand(t, "secrets", "put", "example", "--organization", "team", "--space", "dev")
	assert.True(err != nil && strings.Contains(err.Error(), "file"))
}

// TestManagementTemplateDefault exposes explicit provider credentials and one
// external OIDC declaration without requiring a running server.
func TestManagementTemplateDefault(t *testing.T) {
	assert := is.New(t)
	output, _, err := runApplicationCommand(t, "management", "apply", "--template")
	assert.NoErr(err)
	var configuration appapi.ManagementConfiguration
	assert.NoErr(yaml.UnmarshalStrict([]byte(output), &configuration))
	assert.True(configuration.BorqPublicURL != nil)
	assert.True(configuration.Network != nil && configuration.Network.Tailscale != nil && configuration.Network.Cloudflare != nil)
	assert.Equal(*configuration.Network.Tailscale.ClientID, "replace-with-tailscale-oauth-client-id")
	assert.Equal(*configuration.Network.Tailscale.ClientSecret, "replace-with-tailscale-oauth-client-secret")
	assert.Equal(*configuration.Network.Cloudflare.APIToken, "replace-with-cloudflare-api-token")
	organizations := *configuration.Organizations
	assert.Equal(len(organizations), 1)
	assert.True(organizations[0].ManagedPocketID == nil && organizations[0].Oidc != nil)
	assert.Equal(organizations[0].Oidc.AdminEmail, "admin@example.com")
	assert.Equal(*organizations[0].Oidc.IssuerURL, "https://accounts.google.com")
	bootstrapOutput, _, err := runApplicationCommand(t, "management", "bootstrap", "--template")
	assert.NoErr(err)
	assert.Equal(bootstrapOutput, output)
}
