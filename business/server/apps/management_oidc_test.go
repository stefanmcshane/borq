package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementPocketIdOrganizationAdministrator keeps the selected Borq account
// separate from Pocket ID administration through acceptance, completion and restart.
// Completion is an application-state transition; this fixture does not install a provider.
func TestManagementPocketIdOrganizationAdministrator(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	provider := managementOidcDiscovery(t)
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	declaration := managementOidcManagedDeclaration()
	declaration.Organizations[0].ManagedPocketId.IssuerURL = issuer
	declaration.Organizations[0].ManagedPocketId.CertificateIssuer = ""
	declaration.Organizations[0].ManagedPocketId.OrganizationAdminEmail = "organization-admin@example.test"
	operation, err := manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	accepted := task.Authentication["team"]
	policy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", accepted.OrganizationID)
	assert.NoErr(err)
	assert.Equal(policy.AdminEmail, "organization-admin@example.test")
	assert.Equal(policy.PocketInstanceID, "")
	// Stop the management worker before driving this isolated completion transition.
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", managementPocketIdInput(*task.Configuration.Organizations[0].ManagedPocketId, task.Configuration.Organizations[0].oidcInput(), accepted.Revision))
	assert.NoErr(err)
	pocket, _, err := manager.readPocketId(t.Context(), accepted.OrganizationID)
	assert.NoErr(err)
	assert.Equal(pocket.Status.AdminEmail, "admin@example.test")
	credentials, err := manager.pocketIdCredentials(t.Context(), pocket)
	assert.NoErr(err)
	assert.NoErr(manager.completePocketIdSetup(t.Context(), pocket, credentials["clientSecret"]))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	policy, _, err = loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", accepted.OrganizationID)
	assert.NoErr(err)
	assert.Equal(policy.AdminEmail, "organization-admin@example.test")
	assert.Equal(policy.PocketInstanceID, pocket.ID)
	assert.Equal(policy.ClientID, pocket.ID)
	prepared, err := manager.PrepareOrganizationLogin(t.Context(), "team", "")
	assert.NoErr(err)
	identity := oidc.Identity{Issuer: issuer, Subject: "provider-management-account", Email: "admin@example.test", EmailVerified: true, ExpiresAt: time.Now().Add(time.Hour)}
	providerAccount, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", prepared.Generation, identity)
	assert.NoErr(err)
	assert.True(providerAccount.MemberID != "")
	permissions, err := manager.OrganizationLoginPermissions(t.Context(), providerAccount)
	assert.NoErr(err)
	assert.Equal(permissions, []Permission{})
	identity.Subject, identity.Email = "organization-account", "organization-admin@example.test"
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", prepared.Generation, identity)
	assert.NoErr(err)
	declaration.Organizations[0].ManagedPocketId.OrganizationAdminEmail = "replacement@example.test"
	_, _, err = manager.stageManagementAuthentication(t.Context(), &declaration)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "initial administrator policy"))
	policy, policyRecord, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", accepted.OrganizationID)
	assert.NoErr(err)
	policy.PocketInstanceID, err = newId()
	assert.NoErr(err)
	write, err := documentWrite("organization-login-policy", accepted.OrganizationID, policy, policyRecord.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
}

// TestManagementPocketIdLegacyAdministrator binds an existing ready provider at
// acceptance, because its setup completion will not run again after the upgrade.
func TestManagementPocketIdLegacyAdministrator(t *testing.T) {
	assert := is.New(t)
	manager, configuration, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	source, err := manager.organizationPocketIdSource(t.Context(), "team")
	assert.NoErr(err)
	declaration := ManagementConfiguration{
		Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: source.Status.IssuerURL,
			CertificateIssuer: source.Status.CertificateIssuer, AdminUsername: source.Status.AdminUsername, AdminEmail: source.Status.AdminEmail, BaseURL: source.Status.BaseURL, OrganizationAdminEmail: "selected-org-admin@example.test"}}},
	}
	_, writes, err := manager.stageManagementAuthentication(t.Context(), &declaration)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), writes))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	policy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", source.OrganizationID)
	assert.NoErr(err)
	identity.Email, identity.Subject = "selected-org-admin@example.test", "selected-account"
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	current, record, err := manager.readPocketId(t.Context(), source.OrganizationID)
	assert.NoErr(err)
	current.Status.State = "failed"
	write, err := documentWrite("organization-pocketid", source.OrganizationID, current, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
	assert.Equal(policy.PocketInstanceID, source.ID)
	assert.Equal(policy.ClientID, source.ID)
	t.Run("enrolled account is preserved", func(t *testing.T) {
		assert := is.New(t)
		manager, _, generation, identity := organizationLoginFixture(t)
		defer manager.Close()
		login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
		assert.NoErr(err)
		source, err := manager.organizationPocketIdSource(t.Context(), "team")
		assert.NoErr(err)
		declaration := ManagementConfiguration{
			Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: source.Status.IssuerURL,
				CertificateIssuer: source.Status.CertificateIssuer, AdminUsername: source.Status.AdminUsername, AdminEmail: source.Status.AdminEmail, BaseURL: source.Status.BaseURL, OrganizationAdminEmail: "replacement@example.test"}}},
		}
		_, _, err = manager.stageManagementAuthentication(t.Context(), &declaration)
		assert.True(errors.Is(err, ErrConflict))
		assert.True(strings.Contains(err.Error(), "enrolled administrator"))
		assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	})
}

// TestManagementOidcResumeAndReplay owns durable authentication acceptance and
// exported declaration replay; the HTTP fixtures provide discovery and vault storage only.
func TestManagementOidcResumeAndReplay(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	fixture.listGate, fixture.gateAt = make(chan struct{}), 1
	provider := managementOidcDiscovery(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := ManagementConfiguration{
		S3:            &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "local", Bucket: "backups", PathStyle: true, AccessKey: "oidc-resume-access", SecretKey: "oidc-resume-s3-secret"},
		Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", AdminSubject: "selected-account", IssuerURL: provider.URL, ClientID: "team-client", ClientSecret: "oidc-resume-client-secret", BaseURL: "https://borq.example.test/"}}},
	}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementOidcGate(t, fixture)
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	organizations, err := manager.ListOrganizations(t.Context())
	assert.NoErr(err)
	assert.Equal(len(organizations), 1)
	assert.Equal(task.Authentication["team"], managementAuthentication{OrganizationID: organizations[0].ID, PolicyRevision: 1})
	staged := task.Configuration.Organizations[0].Oidc
	assert.Equal(staged.ClientSecret, "")
	assert.True(strings.HasPrefix(staged.ClientSecretRef, "borq/organization-oidc/"+organizations[0].ID+"/"))
	assert.NoErr(manager.Close())
	database, err := os.ReadFile(input.DatabasePath)
	assert.NoErr(err)
	for _, secret := range []string{configuration.S3.AccessKey, configuration.S3.SecretKey, configuration.Organizations[0].Oidc.ClientSecret} {
		assert.True(!strings.Contains(string(database), secret))
	}
	close(fixture.listGate)
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	stored, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(stored.OrganizationID, organizations[0].ID)
	identity := oidc.Identity{Issuer: provider.URL, Subject: "selected-account", EmailVerified: false, ExpiresAt: time.Now().Add(time.Hour)}
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	secret, err := manager.organizationOidcSecret(t.Context(), stored)
	assert.NoErr(err)
	assert.True(secret == configuration.Organizations[0].Oidc.ClientSecret)
	connection, err := manager.PrepareOrganizationOidcTest(t.Context(), "team", "")
	assert.NoErr(err)
	assert.NoErr(manager.CompleteOrganizationOidcTest(t.Context(), "team", connection.Generation))
	tested, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.True(tested.LastTestedAt != "")
	assert.Equal(tested.BaseURL, "https://borq.example.test")
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.Equal(len(exported.Organizations), 1)
	assert.Equal(exported.Organizations[0].Oidc.ClientSecret, "")
	assert.Equal(exported.Organizations[0].Oidc.AdminSubject, identity.Subject)
	assert.Equal(exported.Organizations[0].Oidc.ClientSecretRef, stored.CredentialsRef)
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), secret))
	permissions, err := manager.GetOrganizationPermissions(t.Context(), "team")
	assert.NoErr(err)
	root := WithPrincipal(t.Context(), Principal{Admin: true})
	_, err = manager.PutOrganizationMember(root, "team", "initial-administrator", OrganizationMemberInput{RoleIDs: []string{}, Disabled: true, Revision: permissions.Revision})
	assert.NoErr(err)
	replay, err := manager.ApplyManagementConfiguration(t.Context(), exported)
	assert.NoErr(err)
	waitManagementResult(t, manager, replay.ID, "succeeded")
	after, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(after, tested)
	current, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(current.Generation, connection.Generation)
	assert.Equal(current.CredentialsRef, stored.CredentialsRef)
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", current.Generation, identity)
	assert.True(err != nil)
	permissions, err = manager.GetOrganizationPermissions(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(len(permissions.Permissions), 0)
	exported.Organizations[0].Oidc.AdminSubject = "replacement-account"
	_, err = manager.ApplyManagementConfiguration(t.Context(), exported)
	assert.True(errors.Is(err, ErrConflict))
	exported.Organizations[0].Oidc.AdminSubject = identity.Subject
	exported.Organizations[0].Oidc.AdminEmail = "owner@example.test"
	exported.Organizations[0].Oidc.IssuerURL = "https://different-issuer.example.test"
	_, err = manager.ApplyManagementConfiguration(t.Context(), exported)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(OrganizationOidcRequestMessage(err), "saved issuer"))
	for _, collection := range []string{"management-task", "management-operation", "organization-oidc"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		assert.True(len(records) > 0)
		for _, record := range records {
			assert.True(!strings.Contains(string(record.Value), secret))
		}
	}
}

// TestManagementOidcAcceptanceRevision proves an accepted, delayed apply cannot
// overwrite a provider subsequently saved through Admin.
func TestManagementOidcAcceptanceRevision(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	fixture.listGate, fixture.gateAt = make(chan struct{}), 1
	provider := managementOidcDiscovery(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	initial, err := manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: provider.URL, ClientID: "initial-client", ClientSecret: "initial-client-secret", BaseURL: "https://borq.example.test"})
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{
		S3:            &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "local", Bucket: "backups", PathStyle: true, AccessKey: "acceptance-access", SecretKey: "acceptance-s3-secret"},
		Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", IssuerURL: provider.URL, ClientID: "declaration-client", ClientSecret: "declaration-client-secret", BaseURL: initial.BaseURL}}},
	})
	assert.NoErr(err)
	waitManagementOidcGate(t, fixture)
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.Equal(task.Authentication["team"], managementAuthentication{OrganizationID: organization.ID, Revision: initial.Revision, PolicyRevision: 1})
	updated, err := manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: provider.URL, ClientID: "admin-client", ClientSecret: "admin-client-secret", BaseURL: initial.BaseURL, Revision: initial.Revision})
	assert.NoErr(err)
	close(fixture.listGate)
	waitManagementResult(t, manager, operation.ID, "failed")
	failed, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.True(strings.Contains(failed.Message, "OIDC settings changed after management configuration was accepted"))
	retained, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(retained, updated)
	stored, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	secret, err := manager.organizationOidcSecret(t.Context(), stored)
	assert.NoErr(err)
	assert.True(secret == "admin-client-secret")
}

// TestManagementOidcReferencesRejectBeforeVault proves both authentication input
// paths enforce exact ownership before making even a vault credential request.
func TestManagementOidcReferencesRejectBeforeVault(t *testing.T) {
	assert := is.New(t)
	var contacts atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		contacts.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer endpoint.Close()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(kubeconfig, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: vault\n  cluster:\n    server: %s\ncontexts:\n- name: vault\n  context:\n    cluster: vault\n    user: vault\ncurrent-context: vault\nusers:\n- name: vault\n  user: {}\n", endpoint.URL)), 0600))
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}})
	assert.NoErr(err)
	defer manager.Close()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	other, err := manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	for _, purpose := range []string{"oidc", "pocketid-values"} {
		prefix := "borq/organization-" + purpose + "/" + organization.ID + "/"
		otherPurpose := "oidc"
		if purpose == "oidc" {
			otherPurpose = "pocketid-values"
		}
		for _, test := range []struct{ name, ref string }{
			{"other organization", "borq/organization-" + purpose + "/" + other.ID + "/" + strings.Repeat("a", 32)},
			{"other purpose", "borq/organization-" + otherPurpose + "/" + organization.ID + "/" + strings.Repeat("a", 32)},
			{"short suffix", prefix + strings.Repeat("a", 31)},
			{"uppercase suffix", prefix + strings.Repeat("A", 32)},
			{"path traversal", prefix + "../" + strings.Repeat("a", 32)},
			{"escaped path", prefix + "%2e%2e%2f" + strings.Repeat("a", 32)},
			{"query suffix", prefix + strings.Repeat("a", 32) + "?version=2"},
		} {
			t.Run(purpose+"/"+test.name, func(t *testing.T) {
				assert := is.New(t)
				configuration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "admin@example.test", IssuerURL: "https://external.example.test", ClientID: "client", BaseURL: "https://borq.example.test", ClientSecretRef: test.ref}}}}
				if purpose == "pocketid-values" {
					configuration = managementOidcManagedDeclaration()
					configuration.Organizations[0].ManagedPocketId.ValuesRef = test.ref
				}
				_, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
				assert.True(errors.Is(err, ErrInvalid))
				assert.True(strings.Contains(err.Error(), "does not belong to this organization and purpose"))
				assert.Equal(contacts.Load(), int64(0))
			})
		}
	}
	operations, err := manager.database.List(t.Context(), "management-operation")
	assert.NoErr(err)
	assert.Equal(len(operations), 0)
	_, err = manager.readManagementSecret(t.Context(), "oidc", organization.ID, "borq/organization-oidc/"+organization.ID+"/"+strings.Repeat("a", 32), "clientSecret")
	assert.True(err != nil && !errors.Is(err, ErrInvalid))
	assert.True(contacts.Load() > 0)
}

// TestManagementOidcManagedValuesStaging verifies Helm values containing secrets
// become vault references in the accepted task before any provisioning begins.
func TestManagementOidcManagedValuesStaging(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]any
	}{
		{"private configuration", map[string]any{"config": map[string]any{"smtpPassword": "private-mail-password"}}},
		{"explicit empty values", map[string]any{}},
		{"omitted values", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementStorageFixture(t)
			fixture.listGate, fixture.gateAt = make(chan struct{}), 1
			manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond})
			assert.NoErr(err)
			defer manager.Close()
			configuration := managementOidcManagedDeclaration()
			configuration.S3 = &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "local", Bucket: "backups", PathStyle: true, AccessKey: "values-access", SecretKey: "values-s3-secret"}
			configuration.Organizations[0].ManagedPocketId.Values = test.values
			operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
			assert.NoErr(err)
			waitManagementOidcGate(t, fixture)
			record, err := manager.database.Get(t.Context(), "management-task", operation.ID)
			assert.NoErr(err)
			assert.True(!strings.Contains(string(record.Value), "private-mail-password"))
			var task managementTask
			assert.NoErr(json.Unmarshal(record.Value, &task))
			addon := task.Configuration.Organizations[0].ManagedPocketId
			assert.True(addon.Values == nil)
			if test.values != nil {
				assert.True(addon.ValuesRef != "")
			} else {
				assert.Equal(addon.ValuesRef, "")
			}
			values, err := manager.readPocketIdValues(t.Context(), task.Authentication["team"].OrganizationID, addon.ValuesRef)
			assert.NoErr(err)
			assert.Equal(values, test.values)
			assert.Equal(task.Configuration.Organizations[0].oidcInput().IssuerURL, addon.IssuerURL)
			assert.Equal(task.Configuration.Organizations[0].oidcInput().ClientID, "")
			instances, err := manager.database.List(t.Context(), "organization-pocketid")
			assert.NoErr(err)
			assert.Equal(len(instances), 0)
		})
	}
}

// TestManagementOidcFailedCertificateRepair owns declarative retry acceptance and
// identity preservation. The startup barrier keeps real controller work out of this test.
func TestManagementOidcFailedCertificateRepair(t *testing.T) {
	assert := is.New(t)
	manager, _ := pocketIdQueueFixture(t)
	defer manager.Close()
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	configuration := managementOidcManagedDeclaration()
	addon := configuration.Organizations[0].ManagedPocketId
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", managementPocketIdInput(*addon, configuration.Organizations[0].oidcInput(), 0))
	assert.NoErr(err)
	original, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	credentials, err := manager.pocketIdCredentials(t.Context(), original)
	assert.NoErr(err)
	configuration.Organizations[0].ManagedPocketId.CertificateIssuer = "repaired-ca"
	_, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "certificate issuer changes require a failed setup"))
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "Certificate issuer unavailable")
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	var repaired organizationPocketIdRecord
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		repaired, _, err = manager.readPocketId(t.Context(), organization.ID)
		assert.NoErr(err)
		if repaired.Status.State == "queued" && repaired.Status.CertificateIssuer == "repaired-ca" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assert.Equal(repaired.Status.State, "queued")
	assert.Equal(repaired.Status.CertificateIssuer, "repaired-ca")
	assert.Equal(repaired.ID, original.ID)
	assert.Equal(repaired.OrganizationID, original.OrganizationID)
	assert.Equal(repaired.Status.IssuerURL, original.Status.IssuerURL)
	assert.True(repaired.CredentialsStored)
	reused, err := manager.pocketIdCredentials(t.Context(), repaired)
	assert.NoErr(err)
	for _, key := range []string{"encryptionKey", "apiKey", "clientSecret"} {
		assert.True(len(credentials[key]) >= 32)
		assert.True(reused[key] == credentials[key])
	}
	pending, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.Equal(pending.Phase, "pocketid")
	assert.True(!operationFinished(pending.Status))
}

// TestManagementOidcDeclarations owns cross-resource identity declarations and the
// ordering that makes components and Pocket ID precede provider settings and workloads.
func TestManagementOidcDeclarations(t *testing.T) {
	assert := is.New(t)
	configuration := managementOidcManagedDeclaration()
	configuration.Components = map[string]platform.ComponentOverride{"traefik": {}}
	configuration.Organizations[0].Clusters = []ClusterInput{{Name: "converged"}}
	configuration.Organizations[0].Spaces = []ManagementSpace{{Name: "apps", Cluster: "converged"}}
	configuration.Organizations = append([]ManagementOrganization{{Name: "external", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", IssuerURL: "https://external.example.test/tenant", ClientID: "external-client", ClientSecret: "external-secret", BaseURL: "https://borq.example.test"}}}, configuration.Organizations...)
	assert.NoErr(validateManagementConfiguration(configuration))
	var order []string
	for _, action := range managementActions(configuration) {
		order = append(order, action.kind+":"+action.organization.Name)
	}
	assert.Equal(order, []string{"components:", "organization:external", "oidc:external", "organization:team", "pocketid:team", "oidc:team", "cluster:team", "space:team"})
	sharedName := managementOidcManagedDeclaration()
	second := *sharedName.Organizations[0].ManagedPocketId
	second.IssuerURL = "https://other-id.example.test"
	sharedName.Organizations = append(sharedName.Organizations, ManagementOrganization{Name: "other", ManagedPocketId: &second})
	assert.NoErr(validateManagementConfiguration(sharedName))
	for _, test := range []struct {
		name, message string
		change        func(*ManagementConfiguration)
	}{
		{"both providers", "oidc or managedPocketId, not both", func(c *ManagementConfiguration) { c.Organizations[0].Oidc = &ManagementOidc{} }},
		{"duplicate organization", "duplicate organization", func(c *ManagementConfiguration) {
			c.Organizations = append(c.Organizations, c.Organizations[0])
		}},
		{"ambiguous values", "values or valuesRef", func(c *ManagementConfiguration) {
			c.Organizations[0].ManagedPocketId.Values = map[string]any{}
			c.Organizations[0].ManagedPocketId.ValuesRef = "reference"
		}},
		{"invalid administrator account", "exact Account ID", func(c *ManagementConfiguration) {
			c.Organizations[0].ManagedPocketId = nil
			c.Organizations[0].Oidc = &ManagementOidc{AdminSubject: "not\nan-account"}
		}},
		{"ambiguous credentials", "clientSecret or clientSecretRef", func(c *ManagementConfiguration) {
			c.Organizations[0].ManagedPocketId = nil
			c.Organizations[0].Oidc = &ManagementOidc{ClientSecret: "secret", ClientSecretRef: "reference"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			configuration := managementOidcManagedDeclaration()
			test.change(&configuration)
			err := validateManagementConfiguration(configuration)
			assert.True(errors.Is(err, ErrInvalid))
			assert.True(strings.Contains(err.Error(), test.message))
		})
	}
}

func managementOidcManagedDeclaration() ManagementConfiguration {
	return ManagementConfiguration{
		Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: "https://id.example.test", CertificateIssuer: "public-ca", AdminUsername: "administrator", AdminEmail: "admin@example.test", BaseURL: "https://borq.example.test"}}},
	}
}

func managementOidcDiscovery(t *testing.T) *httptest.Server {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	t.Cleanup(provider.Close)
	return provider
}

func waitManagementOidcGate(t *testing.T, fixture *managementStorageFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fixture.mutex.Lock()
		blocked := fixture.listCount >= fixture.gateAt
		fixture.mutex.Unlock()
		if blocked {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("management operation did not reach its S3 gate")
}

// TestManagementOidcAdministratorApplyFailure owns atomic provider/account
// publication and retry after discovery fails in the durable management worker.
func TestManagementOidcAdministratorApplyFailure(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	var fail atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if fail.Load() {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	defer provider.Close()
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := ManagementConfiguration{
		S3:            &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "local", Bucket: "backups", PathStyle: true, AccessKey: "access", SecretKey: "secret"},
		Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", AdminSubject: "selected-account", IssuerURL: provider.URL, ClientID: "team-client", ClientSecret: "provider-secret", BaseURL: "https://borq.example.test"}}},
	}
	fail.Store(true)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "failed")
	organizations, err := manager.ListOrganizations(t.Context())
	assert.NoErr(err)
	assert.Equal(len(organizations), 1)
	_, _, err = loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", organizations[0].ID)
	assert.True(errors.Is(err, ErrNotFound))
	settings, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(settings.Revision, int64(0))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	fail.Store(false)
	configuration.Organizations[0].Oidc.AdminEmail = "corrected-owner@example.test"
	operation, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	stored, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	identity := oidc.Identity{Issuer: provider.URL, Subject: "selected-account", EmailVerified: false, ExpiresAt: time.Now().Add(time.Hour)}
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	settings, err = manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(settings.AdminEmail, "corrected-owner@example.test")
	pinned, _, err := loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", stored.OrganizationID)
	assert.NoErr(err)
	assert.Equal(pinned.Email, settings.AdminEmail)
	// Failed discovery must leave the active administrator selection unchanged.
	configuration.Organizations[0].Oidc.AdminEmail = "unapplied-owner@example.test"
	configuration.Organizations[0].Oidc.AdminSubject = ""
	configuration.Organizations[0].Oidc.BaseURL = "https://new-borq.example.test"
	fail.Store(true)
	operation, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "failed")
	settings, err = manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(settings.AdminEmail, "corrected-owner@example.test")
	identity.Subject, identity.Email, identity.EmailVerified = "unapplied-account", "unapplied-owner@example.test", true
	unapplied, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	grants, err := manager.OrganizationLoginPermissions(t.Context(), unapplied)
	assert.NoErr(err)
	assert.Equal(len(grants), 0)
}

// TestManagementOidcInitialAdministratorRole owns the management declaration's
// administrator selection: verified selected accounts gain organization access,
// earlier administrators retain it, and a consumed selection cannot restore revocations.
func TestManagementOidcInitialAdministratorRole(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	provider := managementOidcDiscovery(t)
	managerInput := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(t.Context(), managerInput)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := ManagementConfiguration{
		S3:            &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "local", Bucket: "backups", PathStyle: true, AccessKey: "access", SecretKey: "secret"},
		Organizations: []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", IssuerURL: provider.URL, ClientID: "team-client", ClientSecret: "provider-secret", BaseURL: "https://borq.example.test"}}},
	}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	stored, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	identity := oidc.Identity{Issuer: provider.URL, Subject: "first-administrator", Email: "owner@example.test", EmailVerified: true, ExpiresAt: time.Now().Add(time.Hour)}
	administrator, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	assert.Equal(administrator.MemberID, "")
	grants, err := manager.OrganizationLoginPermissions(t.Context(), administrator)
	assert.NoErr(err)
	assert.Equal(grants, []Permission{"*"})
	principal := WithPrincipal(t.Context(), Principal{Login: administrator, Permissions: []Permission{"*"}})
	assert.NoErr(manager.Authorize(principal, PermissionUserManage, "team"))
	assert.NoErr(manager.Authorize(principal, PermissionOrganizationConfigure, "team"))
	assert.True(errors.Is(manager.Authorize(principal, PermissionUserManage, "other"), ErrForbidden))
	assert.True(errors.Is(manager.Authorize(principal, PermissionManagementConfigure, "team"), ErrForbidden))
	identity.Subject, identity.Email = "another-provider-account", "member@example.test"
	member, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	assert.True(member.MemberID != "")
	grants, err = manager.OrganizationLoginPermissions(t.Context(), member)
	assert.NoErr(err)
	assert.Equal(len(grants), 0)
	configuration.Organizations[0].Oidc.AdminEmail = ""
	_, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.True(errors.Is(err, ErrInvalid))
	configuration.Organizations[0].Oidc.AdminEmail = "member@example.test"
	operation, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	identity.EmailVerified = false
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.True(err != nil)
	identity.EmailVerified = true
	member, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	grants, err = manager.OrganizationLoginPermissions(t.Context(), member)
	assert.NoErr(err)
	assert.Equal(grants, []Permission{"*"})
	grants, err = manager.OrganizationLoginPermissions(t.Context(), administrator)
	assert.NoErr(err)
	assert.Equal(grants, []Permission{"*"})
	root := WithPrincipal(t.Context(), Principal{Admin: true})
	members, err := manager.ListOrganizationMembers(root, "team")
	assert.NoErr(err)
	for _, account := range members {
		if account.ID == "initial-administrator" {
			assert.Equal(account.Email, "owner@example.test")
		}
		if account.ID == member.MemberID {
			_, err = manager.PutOrganizationMember(root, "team", account.ID, OrganizationMemberInput{RoleIDs: []string{}, Revision: account.Revision})
			assert.NoErr(err)
		}
	}
	// Read through a real reopen before replaying settings and authenticating again.
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), managerInput)
	assert.NoErr(err)
	configuration.Organizations[0].Oidc.AdminEmail = " MEMBER@example.test "
	operation, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	member, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	grants, err = manager.OrganizationLoginPermissions(t.Context(), member)
	assert.NoErr(err)
	assert.Equal(len(grants), 0)
	identity.Subject = "different-account-reusing-email"
	reused, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	grants, err = manager.OrganizationLoginPermissions(t.Context(), reused)
	assert.NoErr(err)
	assert.Equal(len(grants), 0)
	grants, err = manager.OrganizationLoginPermissions(t.Context(), administrator)
	assert.NoErr(err)
	assert.Equal(grants, []Permission{"*"})

	identity.Subject, identity.Email = "disabled-selected-account", "disabled@example.test"
	disabled, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.NoErr(err)
	members, err = manager.ListOrganizationMembers(root, "team")
	assert.NoErr(err)
	for _, account := range members {
		if account.ID == disabled.MemberID {
			_, err = manager.PutOrganizationMember(root, "team", account.ID, OrganizationMemberInput{RoleIDs: []string{}, Disabled: true, Revision: account.Revision})
			assert.NoErr(err)
		}
	}
	configuration.Organizations[0].Oidc.AdminEmail = identity.Email
	operation, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", stored.Generation, identity)
	assert.True(err != nil)
	grants, err = manager.OrganizationLoginPermissions(t.Context(), administrator)
	assert.NoErr(err)
	assert.Equal(grants, []Permission{"*"})

}
