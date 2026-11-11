package apps

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// pocketIdQueueFixture keeps workers behind the real management-startup barrier.
// It models vault transport only; real installation is owned by the platform integration test.
func pocketIdQueueFixture(t *testing.T) (Manager, NewInput) {
	t.Helper()
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig,
		ManagementEnabled: true, InfrastructureEnabled: true,
		PlatformSettings: platform.Settings{Namespace: "borq", Profile: "dev", OpenBaoURL: "http://vault.test", TelemetryURL: "http://borq.test"}}
	manager, err := NewManager(t.Context(), input)
	is.New(t).NoErr(err)
	return manager, input
}

// TestPocketIdSetupPersistence proves queue durability, per-organization ownership and retry identity.
func TestPocketIdSetupPersistence(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	input := OrganizationPocketIdInput{IssuerURL: "https://id.example.test", BaseURL: "https://borq.example.test", CertificateIssuer: "cloudflare", AdminUsername: "owner", AdminEmail: "owner@example.test"}
	queued, err := manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	assert.Equal(queued.State, "queued")
	first, _, err := loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "other", input)
	assert.True(errors.Is(err, ErrConflict))
	other, err := manager.GetOrganizationPocketId(t.Context(), "other")
	assert.NoErr(err)
	assert.Equal(other.State, "")
	changed := input
	changed.IssuerURL = "https://different.test"
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", changed)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "already has"))
	_, err = manager.PocketIdAdminEnrollment(t.Context(), "team")
	assert.True(errors.Is(err, ErrConflict))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	read, err := manager.GetOrganizationPocketId(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(read, queued)
	// A persisted failure can be retried without allocating another provider or reserving another host.
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "External dependency unavailable")
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	retried, _, err := loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	assert.Equal(retried.ID, first.ID)
	assert.Equal(retried.Status.State, "queued")
}

// TestPocketIdCredentialPersistence protects encryption keys across retries and refuses missing saved credentials.
func TestPocketIdCredentialPersistence(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "issuer", AdminUsername: "owner", AdminEmail: "owner@example.test"})
	assert.NoErr(err)
	stored, _, err := loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	secrets, err := manager.pocketIdCredentials(t.Context(), stored)
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	stored, record, err := loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	assert.True(stored.CredentialsStored)
	for _, secret := range secrets {
		assert.True(len(secret) >= 32)
		assert.True(!strings.Contains(string(record.Value), secret))
	}
	reused, err := manager.pocketIdCredentials(t.Context(), stored)
	assert.NoErr(err)
	assert.Equal(reused, secrets)
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", stored.credentialsPath()))
	missingArchiveKey := maps.Clone(secrets)
	delete(missingArchiveKey, "ageIdentity")
	_, err = vault.WriteKv(t.Context(), "secret", stored.credentialsPath(), openbao.WriteKvInput{Values: missingArchiveKey, CheckVersion: 0})
	assert.NoErr(err)
	_, err = manager.pocketIdCredentials(t.Context(), stored)
	assert.True(err != nil && strings.Contains(err.Error(), "original vault entry"))
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", stored.credentialsPath()))
	_, err = manager.pocketIdCredentials(t.Context(), stored)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "will not be replaced"))
}

// TestPocketIdOidcCompletion owns atomic settings publication and stale-revision protection, not controller readiness.
func TestPocketIdOidcCompletion(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	defer provider.Close()
	// localhost is a valid ingress hostname; the protocol fixture listens on its loopback address.
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	input := OrganizationPocketIdInput{IssuerURL: issuer, BaseURL: "https://borq.test", AdminUsername: "owner", AdminEmail: "owner@example.test"}
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	stored, _, err := loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	external, err := manager.putOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: issuer, BaseURL: "https://borq.test", ClientID: "external", ClientSecret: "external-secret"}, nil)
	assert.NoErr(err)
	credentials, err := manager.pocketIdCredentials(t.Context(), stored)
	assert.NoErr(err)
	err = manager.completePocketIdSetup(t.Context(), stored, credentials["clientSecret"])
	assert.True(errors.Is(err, ErrConflict))
	unchanged, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(unchanged, external)
	progress, err := manager.GetOrganizationPocketId(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(progress.State, "queued")
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "OIDC settings changed")
	assert.NoErr(err)
	input.OidcRevision = external.Revision
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", input)
	assert.NoErr(err)
	stored, _, err = loadDocument[organizationPocketIdRecord](t.Context(), manager.database, "organization-pocketid", organization.ID)
	assert.NoErr(err)
	assert.NoErr(manager.completePocketIdSetup(t.Context(), stored, credentials["clientSecret"]))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	saved, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(saved.ClientID, stored.ID)
	assert.Equal(saved.IssuerURL, issuer)
	assert.True(saved.ClientSecretConfigured)
	assert.Equal(saved.LastTestedAt, "")
	completed, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.True(completed.ClientLinksConfigured)
	connection, err := manager.PrepareOrganizationOidcTest(t.Context(), "team", "")
	assert.NoErr(err)
	assert.Equal(connection.CallbackURL, saved.CallbackURL)
	progress, err = manager.GetOrganizationPocketId(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(progress.State, "ready")
	t.Run("backup upgrade failure preserves the saved provider", func(t *testing.T) {
		assert := is.New(t)
		legacy, record, err := manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		legacy.ClientLinksConfigured = false
		write, err := documentWrite("organization-pocketid", organization.ID, legacy, record.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
		// This completed provider predates the backup receipt. The fixture has no
		// Pocket ID service or RustFS credentials: a link failure must still reach
		// the independent backup guard without reopening administrator setup.
		manager.provisionPendingPocketId(t.Context(), map[string]bool{})
		current, _, err := manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		assert.Equal(current.Status.State, "ready")
		assert.True(!current.BackupConfigured)
		assert.True(!current.ClientLinksConfigured)
		assert.True(strings.Contains(current.Status.Message, "SQLite backup setup"))
		unchanged, err := manager.GetOrganizationOidc(t.Context(), "team")
		assert.NoErr(err)
		assert.Equal(unchanged, saved)
	})
	t.Run("ready restart reconciles retained runtime without resetting OIDC", func(t *testing.T) {
		assert := is.New(t)
		ready, record, err := manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		ready.BackupConfigured, ready.ConfigurationApplied, ready.ClientLinksConfigured = true, true, true
		ready.AppliedValuesRef, ready.AppliedEnvironmentRef = ready.ValuesRef, ready.Environment.CredentialsRef
		ready.Status.Message = "Ready before restart"
		write, err := documentWrite("organization-pocketid", organization.ID, ready, record.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
		// This fixture deliberately has no retained backup credentials. A ready record must
		// reach the real recovery boundary instead of skipping its lost runtime.
		manager.provisionPendingPocketId(t.Context(), map[string]bool{})
		current, _, err := manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		assert.Equal(current.Status.State, "ready")
		assert.Equal(current.Status.Message, "Pocket ID backup credentials are missing. Restore management secret storage before retrying.")
		unchanged, err := manager.GetOrganizationOidc(t.Context(), "team")
		assert.NoErr(err)
		assert.Equal(unchanged, saved)
	})
}

// TestPocketIdCorruptOwnership rejects swapped records and malformed IDs before any enrollment or worker side effects.
func TestPocketIdCorruptOwnership(t *testing.T) {
	assert := is.New(t)
	manager, _ := pocketIdQueueFixture(t)
	defer manager.Close()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	other, err := manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "issuer", AdminUsername: "owner", AdminEmail: "owner@example.test"})
	assert.NoErr(err)
	original, record, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	for _, test := range []struct {
		name   string
		change func(*organizationPocketIdRecord)
	}{
		{"short id", func(stored *organizationPocketIdRecord) { stored.ID = "short" }},
		{"swapped org", func(stored *organizationPocketIdRecord) {
			stored.OrganizationID = other.ID
			stored.Organization = other.Name
		}},
		{"wrong name", func(stored *organizationPocketIdRecord) { stored.Organization = other.Name }},
		{"unreceipted ready", func(stored *organizationPocketIdRecord) { stored.Status.State = "ready" }},
		{"insecure redirect", func(stored *organizationPocketIdRecord) { stored.Status.IssuerURL = "http://foreign.test" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			modified := original
			test.change(&modified)
			write, err := documentWrite("organization-pocketid", organization.ID, modified, record.Revision)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			record.Revision++
			_, err = manager.GetOrganizationPocketId(t.Context(), "team")
			assert.True(err != nil && strings.Contains(err.Error(), "state is invalid"))
			_, err = manager.PocketIdAdminEnrollment(t.Context(), "team")
			assert.True(err != nil && strings.Contains(err.Error(), "state is invalid"))
			manager.provisionPendingPocketId(t.Context(), map[string]bool{})
			records, err := manager.database.List(t.Context(), "organization-pocketid")
			assert.NoErr(err)
			assert.Equal(len(records), 1)
			assert.Equal(records[0].Revision, record.Revision)
		})
	}
}

// TestPocketIdSetupValidation reaches each input guard before accepting durable installation work.
func TestPocketIdSetupValidation(t *testing.T) {
	assert := is.New(t)
	valid := OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "issuer", AdminUsername: "owner", AdminEmail: "owner@example.test"}
	for _, test := range []struct {
		name, expected string
		change         func(*OrganizationPocketIdInput)
	}{
		{"insecure", "HTTPS", func(input *OrganizationPocketIdInput) { input.IssuerURL = "http://id.test" }},
		{"ip", "DNS hostname", func(input *OrganizationPocketIdInput) { input.IssuerURL = "http://127.0.0.1" }},
		{"shared host", "own address", func(input *OrganizationPocketIdInput) { input.IssuerURL = "https://borq.test" }},
		{"path", "without a path", func(input *OrganizationPocketIdInput) { input.IssuerURL += "/id" }},
		{"certificate", "certificate issuer", func(input *OrganizationPocketIdInput) { input.CertificateIssuer = "" }},
		{"issuer port", "valid port", func(input *OrganizationPocketIdInput) { input.IssuerURL = "https://id.test:70000" }},
		{"borq port", "valid port", func(input *OrganizationPocketIdInput) { input.BaseURL = "https://borq.test:0" }},
		{"username", "username", func(input *OrganizationPocketIdInput) { input.AdminUsername = "../user" }},
		{"configuration without revision", "Reload", func(input *OrganizationPocketIdInput) { input.Configuration = map[string]string{} }},
		{"configuration and env", "not both", func(input *OrganizationPocketIdInput) {
			input.Configuration = map[string]string{}
			input.Env = &EnvReference{Name: "group"}
		}},
		{"managed configuration", "managed setting", func(input *OrganizationPocketIdInput) {
			input.Configuration = map[string]string{"ENCRYPTION_KEY": "secret"}
			input.ConfigurationRevision = "revision"
		}},
		{"email", "email", func(input *OrganizationPocketIdInput) { input.AdminEmail = "User <user@example.test>" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			input := valid
			test.change(&input)
			err := validatePocketIdInput(input)
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), test.expected))
		})
	}
	assert.NoErr(validatePocketIdInput(valid))
	for _, email := range []string{"owner@example.test", "owner+tag@example.test", strings.Repeat("a", 40) + "@example.test"} {
		input := valid
		input.AdminUsername, input.AdminEmail = "", email
		assert.NoErr(validatePocketIdInput(input))
		username := pocketIdAdminUsername("", email)
		assert.True(validPocketIdUsername(username))
		if email == "owner@example.test" {
			assert.Equal(username, email)
		}
		assert.Equal(pocketIdAdminUsername("legacy-owner", email), "legacy-owner")
	}
	assert.True(pocketIdAdminUsername("", "owner+one@example.test") != pocketIdAdminUsername("", "owner+two@example.test"))
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", valid)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "management cluster"))
}

// TestPocketIdOffsiteSources preserves every configured provider's archive source
// across a real database reopen, including providers whose later reconciliation failed.
func TestPocketIdOffsiteSources(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	configuration.PlatformSettings.S3Endpoint = "https://management-rustfs.example.test"
	manager.runtime.platformSettings.S3Endpoint = configuration.PlatformSettings.S3Endpoint
	expected := []ManagementS3{}
	for _, name := range []string{"first", "second", "pending"} {
		organization, err := manager.CreateOrganization(t.Context(), name)
		assert.NoErr(err)
		_, err = manager.SetupOrganizationPocketId(t.Context(), name, OrganizationPocketIdInput{
			IssuerURL: "https://" + name + ".example.test", BaseURL: "https://borq.example.test", CertificateIssuer: "issuer", AdminUsername: "owner", AdminEmail: "owner@example.test",
		})
		assert.NoErr(err)
		stored, record, err := manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		if name == "pending" {
			continue
		}
		// This fixture records backup provisioning metadata; controller lifecycle is tested on Kubernetes.
		stored.BackupConfigured = true
		stored.Status.State = "failed"
		write, err := documentWrite("organization-pocketid", organization.ID, stored, record.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
		expected = append(expected, ManagementS3{Endpoint: configuration.PlatformSettings.S3Endpoint, Region: "us-east-1", Bucket: "borq-pocketid-" + stored.ID, PathStyle: true, CredentialsRef: "borq/platform/rustfs"})
	}
	assert.NoErr(manager.Close())
	var err error
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	sources, err := manager.sqliteBackupSources(t.Context())
	assert.NoErr(err)
	assert.Equal(len(sources), 2)
	for _, want := range expected {
		found := false
		for _, source := range sources {
			found = found || source == want
		}
		assert.True(found)
	}
}
