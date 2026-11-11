package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationOidcPersistence owns durable provider isolation, vault-only secrets, and configuration generations.
func TestOrganizationOidcPersistence(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	defer provider.Close()
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	for _, name := range []string{"first", "second"} {
		_, err := manager.CreateOrganization(t.Context(), name)
		assert.NoErr(err)
	}
	// Persist the pre-ID format to exercise identity migration through the save path.
	legacy, legacyRecord, err := loadDocument[Organization](t.Context(), manager.database, "organization", "second")
	assert.NoErr(err)
	legacy.ID = ""
	legacyWrite, err := documentWrite("organization", "second", legacy, legacyRecord.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{legacyWrite}))
	settings := OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: provider.URL, ClientID: "client-one", ClientSecret: "private-oidc-secret", BaseURL: "https://borq.example.test"}
	saved, err := manager.PutOrganizationOidc(t.Context(), "first", settings)
	assert.NoErr(err)
	assert.True(saved.ClientSecretConfigured)
	assert.Equal(saved.Revision, int64(1))
	assert.Equal(saved.LastTestedAt, "")
	assert.Equal(saved.LogoutCallbackURL, "https://borq.example.test/login")
	second, err := manager.PutOrganizationOidc(t.Context(), "second", settings)
	assert.NoErr(err)
	assert.True(second.CallbackURL != saved.CallbackURL)
	connection, err := manager.PrepareOrganizationOidcTest(t.Context(), "first", "")
	assert.NoErr(err)
	assert.Equal(connection.CallbackURL, saved.CallbackURL)
	assert.True(errors.Is(manager.CompleteOrganizationOidcTest(t.Context(), "second", connection.Generation), ErrConflict))
	assert.NoErr(manager.CompleteOrganizationOidcTest(t.Context(), "first", connection.Generation))
	records, err := manager.database.List(t.Context(), "organization-oidc")
	assert.NoErr(err)
	assert.Equal(len(records), 2)
	for _, record := range records {
		assert.True(!strings.Contains(string(record.Value), settings.ClientSecret))
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	saved, err = manager.GetOrganizationOidc(t.Context(), "first")
	assert.NoErr(err)
	assert.Equal(saved.IssuerURL, provider.URL)
	assert.Equal(saved.LogoutCallbackURL, "https://borq.example.test/login")
	assert.True(saved.LastTestedAt != "")
	_, err = manager.PrepareOrganizationOidcTest(t.Context(), "first", "")
	assert.NoErr(err)
	settings.Revision, settings.ClientSecret = saved.Revision, ""
	settings.BaseURL = "https://new-borq.example.test"
	updated, err := manager.PutOrganizationOidc(t.Context(), "first", settings)
	assert.NoErr(err)
	assert.True(strings.HasPrefix(updated.CallbackURL, settings.BaseURL+"/api/auth/oidc/callback/"))
	assert.Equal(updated.LastTestedAt, "")
	assert.Equal(updated.LogoutCallbackURL, "https://new-borq.example.test/login")
	assert.True(errors.Is(manager.CompleteOrganizationOidcTest(t.Context(), "first", connection.Generation), ErrConflict))
	_, err = manager.PutOrganizationOidc(t.Context(), "first", settings)
	assert.True(errors.Is(err, ErrConflict))
	settings.Revision = updated.Revision
	settings.ClientID = "different-client"
	_, err = manager.PutOrganizationOidc(t.Context(), "first", settings)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(OrganizationOidcRequestMessage(err), "client secret"))
	settings.ClientID = "client-one"
	settings.ClientSecret = "replacement-secret"
	fixture.mutex.Lock()
	fixture.vaultDenied = true
	fixture.mutex.Unlock()
	_, err = manager.PutOrganizationOidc(t.Context(), "first", settings)
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), settings.ClientSecret))
	retained, err := manager.GetOrganizationOidc(t.Context(), "first")
	assert.NoErr(err)
	assert.Equal(retained, updated)
	untouched, err := manager.GetOrganizationOidc(t.Context(), "second")
	assert.NoErr(err)
	assert.Equal(untouched, second)
}

// TestOrganizationOidcValidation rejects invalid setup before contacting secret storage or unknown providers.
func TestOrganizationOidcValidation(t *testing.T) {
	assert := is.New(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.GetOrganizationOidc(t.Context(), "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	empty, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(empty, OrganizationOidc{})
	for _, input := range []OrganizationOidcInput{
		{IssuerURL: "http://provider.test", ClientID: "client", ClientSecret: "secret", BaseURL: "https://borq.test"},
		{IssuerURL: "https://provider.test", ClientID: "client", ClientSecret: "secret", BaseURL: "https://borq.test/wrong-path"},
		{IssuerURL: "https://provider.test", ClientID: "client", BaseURL: "https://borq.test"},
	} {
		_, err := manager.PutOrganizationOidc(t.Context(), "team", input)
		assert.True(errors.Is(err, ErrInvalid))
		assert.True(OrganizationOidcRequestMessage(err) != "")
	}
}
