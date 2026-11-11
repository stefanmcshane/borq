package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

// TestSpacePocketIdPersistence proves queue and credential durability through the real SQLite reopen path.
// Kubernetes lifecycle and volume persistence belong to the platform integration test.
func TestSpacePocketIdPersistence(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	cluster := newManagerCluster(t, managerClusterOptions{})
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	reconcile := false
	_, err = manager.RegisterCluster(t.Context(), ClusterInput{Organization: "team", Name: "deployment", KubeconfigPath: cluster.kubeconfig, Reconcile: &reconcile})
	assert.NoErr(err)
	target, err := manager.CreateSpace(t.Context(), SpaceInput{Organization: "team", Name: "test", Cluster: "deployment"})
	assert.NoErr(err)
	queued, err := manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{IssuerURL: "https://id.example.test", AdminEmail: "owner@example.test"}, false)
	assert.NoErr(err)
	assert.Equal(queued.State, "pending")
	assert.Equal(queued.Specification.AdminUsername, "admin")
	stored, _, err := manager.readSpacePocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	secrets, err := manager.spacePocketIdCredentials(t.Context(), stored)
	assert.NoErr(err)
	assert.Equal(stored.OrganizationID, organization.ID)
	operation, err := manager.GetPocketIdOperation(t.Context(), queued.Operation)
	assert.NoErr(err)
	assert.Equal(operation.SpaceID, target.ID)
	for _, collection := range []string{"space-pocketid", "pocketid-operation", "pocketid-task"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		for _, record := range records {
			assert.True(!strings.Contains(string(record.Value), secrets["encryptionKey"]))
			assert.True(!strings.Contains(string(record.Value), secrets["apiKey"]))
			assert.True(!strings.Contains(string(record.Value), secrets["ageIdentity"]))
			assert.True(!strings.Contains(string(record.Value), secrets["ageRecipient"]))
		}
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	read, err := manager.GetPocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	assert.Equal(read, queued)
	stored, _, err = manager.readSpacePocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	reopenedSecrets, err := manager.spacePocketIdCredentials(t.Context(), stored)
	assert.NoErr(err)
	assert.Equal(reopenedSecrets, secrets)
	_, err = manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{}, true)
	assert.True(errors.Is(err, ErrConflict)) // A saved active operation fences concurrent rollovers.
	// Complete only the queue bookkeeping here; no controller outcome is fabricated.
	task, _, err := loadDocument[spacePocketIdTask](t.Context(), manager.database, "pocketid-task", operation.ID)
	assert.NoErr(err)
	assert.NoErr(manager.finishSpacePocketId(operation, task, errors.New("deployment unavailable")))
	changed := PocketIdInput{IssuerURL: "https://another.example.test"}
	_, err = manager.PutPocketId(t.Context(), "team", "test", "identity", changed, true)
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{Storage: "2Gi"}, true)
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{Image: strings.Replace(queued.Specification.Image, ":v2.19.1", ":v2.18.0", 1)}, true)
	assert.True(errors.Is(err, ErrInvalid))
	retried, err := manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{}, true)
	assert.NoErr(err)
	assert.Equal(retried.ID, queued.ID)
	assert.Equal(retried.Specification, queued.Specification)
	current, _, err := manager.readSpacePocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	assert.Equal(current.CredentialsRef, stored.CredentialsRef)
	// Export includes the Space addon and excludes the vaulted key material.
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assert.True(strings.Contains(string(encoded), `"pocketid":[`))
	assert.True(!strings.Contains(string(encoded), secrets["encryptionKey"]))
	assert.True(!strings.Contains(string(encoded), secrets["apiKey"]))
	_, err = manager.CreateSpace(t.Context(), SpaceInput{Organization: "team", Name: "other", Cluster: "deployment"})
	assert.NoErr(err)
	_, err = manager.GetPocketId(t.Context(), "team", "other", "identity")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.PutPocketId(t.Context(), "team", "other", "other", queued.Specification, false)
	assert.True(errors.Is(err, ErrConflict)) // The shared hostname reservation reaches the transaction guard.
	retryOperation, err := manager.GetPocketIdOperation(t.Context(), retried.Operation)
	assert.NoErr(err)
	retryTask, _, err := loadDocument[spacePocketIdTask](t.Context(), manager.database, "pocketid-task", retryOperation.ID)
	assert.NoErr(err)
	assert.NoErr(manager.finishSpacePocketId(retryOperation, retryTask, errors.New("deployment still unavailable")))
	removal, err := manager.DeletePocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	removalOperation, err := manager.GetPocketIdOperation(t.Context(), removal.Operation)
	assert.NoErr(err)
	removalTask, _, err := loadDocument[spacePocketIdTask](t.Context(), manager.database, "pocketid-task", removalOperation.ID)
	assert.NoErr(err)
	assert.NoErr(manager.finishSpacePocketId(removalOperation, removalTask, nil))
	_, err = manager.GetPocketId(t.Context(), "team", "test", "identity")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{}, false)
	assert.True(errors.Is(err, ErrConflict))
	resumed, err := manager.PutPocketId(t.Context(), "team", "test", "identity", PocketIdInput{}, true)
	assert.NoErr(err)
	assert.Equal(resumed.ID, queued.ID)
	retained, _, err := manager.readSpacePocketId(t.Context(), "team", "test", "identity")
	assert.NoErr(err)
	assert.Equal(retained.CredentialsRef, stored.CredentialsRef)
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", retained.CredentialsRef))
	delete(secrets, "ageIdentity")
	malformed, err := json.Marshal(secrets)
	assert.NoErr(err)
	_, err = vault.WriteKv(t.Context(), "secret", retained.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"credentials": string(malformed)}, CheckVersion: 0})
	assert.NoErr(err)
	_, err = manager.spacePocketIdCredentials(t.Context(), retained)
	assert.True(err != nil && strings.Contains(err.Error(), "original vault entry"))
}

// TestSpacePocketIdValidation rejects unsafe origins, mutable image tags, and invalid upstream administrator names.
func TestSpacePocketIdValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		input PocketIdInput
	}{
		{"http", PocketIdInput{IssuerURL: "http://id.example.test"}},
		{"path", PocketIdInput{IssuerURL: "https://id.example.test/path"}},
		{"port", PocketIdInput{IssuerURL: "https://id.example.test:8443"}},
		{"empty port", PocketIdInput{IssuerURL: "https://id.example.test:"}},
		{"ip", PocketIdInput{IssuerURL: "https://192.0.2.1"}},
		{"mutable image", PocketIdInput{Image: "ghcr.io/pocket-id/pocket-id:v2"}},
		{"tag only", PocketIdInput{Image: "ghcr.io/pocket-id/pocket-id:v2.19.1"}},
		{"username", PocketIdInput{AdminUsername: "invalid name"}},
		{"email", PocketIdInput{AdminEmail: "Owner <owner@example.test>"}},
		{"storage", PocketIdInput{Storage: "-1Gi"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := mergeSpacePocketId(PocketIdInput{IssuerURL: "https://id.example.test", AdminEmail: "owner@example.test"}, test.input)
			_, err := normalizeSpacePocketId(input)
			is.New(t).True(errors.Is(err, ErrInvalid))
		})
	}
}
