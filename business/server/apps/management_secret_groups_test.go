package apps

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestManagementPocketIdEnvironmentDurability owns accepted secret snapshots:
// latest cannot drift across restart or history pruning, and SQLite never holds values.
// The vault fixture does not simulate a successful Kubernetes installation.
func TestManagementPocketIdEnvironmentDurability(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	fixture.input.PollInterval = time.Millisecond
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := managementOidcManagedDeclaration()
	configuration.Organizations[0].ManagedPocketId.Env = &EnvReference{Name: "identity", Version: "latest"}
	configuration.SecretGroups = []ManagementSecretGroup{{Organization: "team", Name: "identity", Data: map[string]string{"APP_NAME": "Accepted Identity", "SMTP_PASSWORD": "private-bootstrap-smtp-value"}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	// No RustFS or controller is faked: publication succeeds, installation reports failure.
	waitManagementResult(t, manager, operation.ID, "failed")
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	owner := task.Authentication["team"].OrganizationID
	accepted := task.PocketIdEnvironments["team"]
	assert.Equal(accepted.Name, "identity")
	assert.Equal(accepted.Version, 1)
	assert.Equal(accepted.Selection, "latest")
	assert.True(task.Configuration.SecretGroups[0].Data == nil)
	published, err := manager.GetManagementSecret(t.Context(), "team", "identity", "1")
	assert.NoErr(err)
	assert.Equal(published.Data, configuration.SecretGroups[0].Data)
	for version := 2; version <= 13; version++ {
		_, err := manager.PutManagementSecret(t.Context(), "team", "identity", PutSecretInput{Type: "secret-group", Data: map[string]string{"APP_NAME": "Identity " + strconv.Itoa(version)}})
		assert.NoErr(err)
	}
	assert.NoErr(manager.Close())
	database, err := os.ReadFile(fixture.input.DatabasePath)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(database), "private-bootstrap-smtp-value"))
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	persisted, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.Equal(persisted.PocketIdEnvironments["team"], accepted)
	values, err := manager.readPocketIdEnvironment(t.Context(), owner, accepted)
	assert.NoErr(err)
	assert.Equal(values, configuration.SecretGroups[0].Data)
	_, err = manager.GetManagementSecret(t.Context(), "team", "identity", "1")
	assert.True(errors.Is(err, ErrInvalid))
	pinned, err := manager.resolvePocketIdEnvironment(t.Context(), owner, EnvReference{Name: "identity", Version: "1"})
	assert.NoErr(err)
	assert.Equal(pinned.CredentialsRef, accepted.CredentialsRef)
	assert.Equal(pinned.Selection, "1")
	latest, err := manager.resolvePocketIdEnvironment(t.Context(), owner, EnvReference{Name: "identity", Version: "latest"})
	assert.NoErr(err)
	assert.Equal(latest.Version, 13)
	assert.True(latest.CredentialsRef != accepted.CredentialsRef)
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(exported.SecretGroups, []ManagementSecretGroup{{Organization: "team", Name: "identity", Version: "13"}})
	assert.Equal(exported.Organizations[0].ManagedPocketId.Env, &EnvReference{Name: "identity", Version: "latest"})
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "private-bootstrap-smtp-value"))
	// Snapshot payload keys bind both name and version, beyond path ownership.
	for _, changed := range []pocketIdEnvironment{
		{Name: "other", Version: accepted.Version, Selection: "latest", CredentialsRef: accepted.CredentialsRef},
		{Name: accepted.Name, Version: 2, Selection: "latest", CredentialsRef: accepted.CredentialsRef},
	} {
		_, err := manager.readPocketIdEnvironment(t.Context(), owner, changed)
		assert.True(err != nil)
	}
	_, err = manager.readPocketIdEnvironment(t.Context(), strings.Repeat("a", 32), accepted)
	assert.True(errors.Is(err, ErrInvalid))
}

// TestManagementSecretGroupPublicationReplay protects full-replacement publication,
// stale apply CAS, and restart-safe replay after the vault write already succeeded.
func TestManagementSecretGroupPublicationReplay(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer manager.Close()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	owners := map[string]managementAuthentication{"team": {OrganizationID: organization.ID}}
	configuration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team"}}, SecretGroups: []ManagementSecretGroup{{Organization: "team", Name: "identity", Data: map[string]string{"APP_NAME": "Accepted"}}}}
	publications, _, err := manager.stageManagementSecretGroups(t.Context(), &configuration, owners)
	assert.NoErr(err)
	publication := publications["team/identity"]
	assert.NoErr(manager.applyManagementSecretGroup(t.Context(), configuration.SecretGroups[0], owners["team"], publication))
	assert.NoErr(manager.applyManagementSecretGroup(t.Context(), configuration.SecretGroups[0], owners["team"], publication))
	published, err := manager.GetManagementSecret(t.Context(), "team", "identity", "latest")
	assert.NoErr(err)
	assert.Equal(published.Version, 1)
	configuration.SecretGroups[0].Data, configuration.SecretGroups[0].Version = map[string]string{}, ""
	publications, _, err = manager.stageManagementSecretGroups(t.Context(), &configuration, owners)
	assert.NoErr(err)
	_, err = manager.PutManagementSecret(t.Context(), "team", "identity", PutSecretInput{Type: "secret-group", Data: map[string]string{"APP_NAME": "Concurrent"}})
	assert.NoErr(err)
	err = manager.applyManagementSecretGroup(t.Context(), configuration.SecretGroups[0], owners["team"], publications["team/identity"])
	assert.True(errors.Is(err, ErrConflict))
	retained, err := manager.GetManagementSecret(t.Context(), "team", "identity", "latest")
	assert.NoErr(err)
	assert.Equal(retained.Data, map[string]string{"APP_NAME": "Concurrent"})
	// A fresh empty map is a real update; omission is a reference-only no-op.
	configuration.SecretGroups[0].Data, configuration.SecretGroups[0].Version = map[string]string{}, ""
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	empty, err := manager.GetManagementSecret(t.Context(), "team", "identity", "latest")
	assert.NoErr(err)
	assert.Equal(empty.Version, 3)
	assert.Equal(empty.Data, map[string]string{})
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	// Replay the group fragment; the fixture has no storage or addon controllers.
	replay, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{SecretGroups: exported.SecretGroups, Organizations: []ManagementOrganization{{Name: "team"}}})
	assert.NoErr(err)
	waitManagementResult(t, manager, replay.ID, "succeeded")
	repeated, err := manager.GetManagementSecret(t.Context(), "team", "identity", "latest")
	assert.NoErr(err)
	assert.Equal(repeated, empty)
}

// TestManagementSecretGroupValidation owns document cross-references and the
// early rejection of invalid environment-variable and version selections.
func TestManagementSecretGroupValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ManagementConfiguration)
	}{
		{"missing organization", func(configuration *ManagementConfiguration) { configuration.SecretGroups[0].Organization = "missing" }},
		{"duplicate group", func(configuration *ManagementConfiguration) {
			configuration.SecretGroups = append(configuration.SecretGroups, configuration.SecretGroups[0])
		}},
		{"data and version", func(configuration *ManagementConfiguration) { configuration.SecretGroups[0].Version = "latest" }},
		{"invalid variable", func(configuration *ManagementConfiguration) {
			configuration.SecretGroups[0].Data = map[string]string{"not=valid": "private"}
		}},
		{"invalid binding", func(configuration *ManagementConfiguration) {
			configuration.Organizations[0].ManagedPocketId.Env.Version = "01"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := managementOidcManagedDeclaration()
			configuration.Organizations[0].ManagedPocketId.Env = &EnvReference{Name: "identity", Version: "latest"}
			configuration.SecretGroups = []ManagementSecretGroup{{Organization: "team", Name: "identity", Data: map[string]string{}}}
			test.change(&configuration)
			is.New(t).True(errors.Is(validateManagementConfiguration(configuration), ErrInvalid))
		})
	}
}
