package apps

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestPocketIdOrganizationBranding(t *testing.T) {
	for _, test := range []struct {
		name        string
		values      map[string]any
		environment map[string]string
		want        string
	}{
		{name: "default", want: "team"},
		{name: "other settings", environment: map[string]string{"SMTP_HOST": "mail.test"}, want: "team"},
		{name: "explicit", environment: map[string]string{"APP_NAME": "Team Identity"}, want: "Team Identity"},
		{name: "legacy", values: map[string]any{"config": map[string]any{"appName": "Legacy Identity"}}, want: "Legacy Identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			configuration, err := pocketIdConfiguration("team", test.values, test.environment)
			assert.NoErr(err)
			assert.Equal(configuration["APP_NAME"], test.want)
		})
	}
}

// TestPocketIdConfigurationEditing owns full replacement, publication and snapshot
// concurrency, and durable secret storage behind the direct configuration editor.
func TestPocketIdConfigurationEditing(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	fixture.input.PlatformSettings.OpenBaoURL = "http://vault.test"
	fixture.input.PlatformSettings.TelemetryURL = "http://telemetry.test"
	fixture.input.InfrastructureEnabled = true
	fixture.input.ManagementEnabled = true // Keep deployment workers behind real management startup.
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	initial, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(initial.Values, map[string]string{"APP_NAME": "team"})
	assert.Equal(initial.BaseVersion, 0)
	assert.True(initial.Revision != "")
	_, containsKey := initial.Managed["ENCRYPTION_KEY"]
	assert.True(!containsKey)
	_, containsKey = initial.Managed["STATIC_API_KEY"]
	assert.True(!containsKey)
	input := OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "issuer", AdminEmail: "admin@example.test",
		Configuration: map[string]string{"APP_NAME": "Team Identity", "SMTP_PASSWORD": "  private-password\n"}, ConfigurationRevision: initial.Revision}
	status, err := manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	assert.Equal(status.AdminUsername, input.AdminEmail)
	assert.Equal(status.Env.Version, "1")
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "configuration changed"))
	input.IssuerURL = "https://other.test"
	_, err = manager.SetupOrganizationPocketId(t.Context(), "other", input)
	assert.True(errors.Is(err, ErrConflict))
	input.IssuerURL = "https://id.test"

	assert.NoErr(manager.Close())
	database, err := os.ReadFile(fixture.input.DatabasePath)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(database), "private-password"))
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	current, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(current.Values, input.Configuration)
	assert.Equal(current.BaseVersion, 1)
	// Worker progress must not invalidate a configuration form.
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "provisioning", "Starting")
	assert.NoErr(err)
	progress, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(progress.Revision, current.Revision)
	input.ConfigurationRevision, input.ConfigurationBaseVersion = current.Revision, current.BaseVersion
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	unchanged, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(unchanged.Revision, current.Revision)
	assert.Equal(unchanged.BaseVersion, 1)

	// A publication through the separate secret API invalidates the form even
	// before anyone applies it to the managed instance.
	_, err = manager.PutManagementSecret(t.Context(), "team", status.Env.Name, PutSecretInput{Type: "secret-group", Data: map[string]string{"APP_NAME": "Concurrent"}})
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "base version"))
	current, err = manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(current.Values, input.Configuration) // Still the accepted deployment snapshot.
	assert.Equal(current.BaseVersion, 2)
	input.ConfigurationRevision, input.ConfigurationBaseVersion = current.Revision, current.BaseVersion
	input.Configuration = map[string]string{}
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	empty, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(empty.Values, map[string]string{"APP_NAME": "team"})
	assert.Equal(empty.BaseVersion, 3)
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "private-password"))
}

// TestPocketIdConfigurationReplacesLegacySettings prevents removed keys from
// reappearing through legacy chart configuration, while retaining workload values.
func TestPocketIdConfigurationReplacesLegacySettings(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	fixture.input.PlatformSettings.OpenBaoURL = "http://vault.test"
	fixture.input.PlatformSettings.TelemetryURL = "http://telemetry.test"
	fixture.input.InfrastructureEnabled = true
	fixture.input.ManagementEnabled = true
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer manager.Close()
	owner, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	ref, err := manager.stageManagementSecret(t.Context(), "pocketid-values", owner.ID, "values", `{"config":{"appName":"Legacy","smtpPassword":"legacy-secret"},"resources":{"requests":{"cpu":"75m"}}}`)
	assert.NoErr(err)
	addon := ManagementPocketId{IssuerURL: "https://id.test", CertificateIssuer: "issuer", AdminEmail: "admin@example.test", ValuesRef: ref}
	input := managementPocketIdInput(addon, ManagementOidc{BaseURL: "https://borq.test"}, 0)
	_, err = manager.setupOrganizationPocketId(t.Context(), "team", input, &addon, pocketIdEnvironment{})
	assert.NoErr(err)
	current, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(current.Values, map[string]string{"APP_NAME": "Legacy", "SMTP_PASSWORD": "legacy-secret"})
	input.Configuration, input.ConfigurationRevision, input.ConfigurationBaseVersion = map[string]string{"APP_NAME": "Edited"}, current.Revision, current.BaseVersion
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	updated, err := manager.GetOrganizationPocketIdConfiguration(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(updated.Values, input.Configuration)
	stored, _, err := manager.readPocketId(t.Context(), owner.ID)
	assert.NoErr(err)
	values, err := manager.readPocketIdValues(t.Context(), owner.ID, stored.ValuesRef)
	assert.NoErr(err)
	assert.Equal(values, map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "75m"}}})
}
