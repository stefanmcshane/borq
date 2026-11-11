package apps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementWithoutOidc leaves new organizations under administrator-token
// access until identity setup is explicitly declared, including after restart.
func TestManagementWithoutOidc(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	assert.NoErr(manager.Close())
	configuration.PlatformSettings.ClusterName = "management"
	configuration.PlatformSettings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "cert-admin@example.test"}
	manager, err := NewManager(t.Context(), configuration)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	declaration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team"}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	organization, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
	assert.NoErr(err)
	assert.Equal(organization.AuthenticationState, "")
	for _, collection := range []string{"organization-pocketid", "organization-login-policy", "organization-oidc", "organization-pocketid-host"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		assert.Equal(len(records), 0)
	}
	available, err := manager.ListOrganizationSignInNames(t.Context())
	assert.NoErr(err)
	assert.Equal(len(available), 0)
	operation, err = manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
}

// TestManagementImplicitPocketIdFailure owns aggregate waiting and durable
// retries for a provider queued by a previous explicit organization setup.
func TestManagementImplicitPocketIdFailure(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	manager.runtime.platformSettings.ClusterName = "management"
	manager.runtime.platformSettings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "owner@example.test"}
	_, err := manager.OnboardOrganization(t.Context(), "team", OrganizationOnboardingInput{PocketId: &OrganizationPocketIdInput{IssuerURL: "https://id-team.example.test", BaseURL: "https://management.example.test", AdminEmail: "owner@example.test", CertificateIssuer: "letsencrypt-cloudflare"}})
	assert.NoErr(err)
	declaration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team"}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	waitImplicitManagementGate(t, manager, operation.ID)
	organization, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
	assert.NoErr(err)
	pocket, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	policy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.NoErr(err)
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "dependency unavailable")
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "failed")
	failed, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.True(strings.Contains(failed.Message, "reapply management configuration"))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	operation, err = manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	waitImplicitManagementGate(t, manager, operation.ID)
	retried, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(retried.ID, pocket.ID)
	assert.Equal(retried.Status.State, "queued")
	retainedPolicy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.NoErr(err)
	assert.Equal(retainedPolicy, policy)
	// A resumed task must observe a new failure, rather than continually requeue it.
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "dependency unavailable again")
	assert.NoErr(err)
	assert.NoErr(manager.resumeManagementOperations(t.Context()))
	waitManagementResult(t, manager, operation.ID, "failed")
	retained, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(retained.Status.State, "failed")
}

// TestManagementImplicitPocketIdCompletion checks saved authentication readiness,
// not Kubernetes controller success, before aggregate apply can report success.
func TestManagementImplicitPocketIdCompletion(t *testing.T) {
	assert := is.New(t)
	manager, _ := pocketIdQueueFixture(t)
	defer manager.Close()
	provider := managementOidcDiscovery(t)
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	organization, err := manager.OnboardOrganization(t.Context(), "team", OrganizationOnboardingInput{PocketId: &OrganizationPocketIdInput{IssuerURL: issuer, BaseURL: "https://borq.test", AdminEmail: "owner@example.test"}})
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team"}}})
	assert.NoErr(err)
	waitImplicitManagementGate(t, manager, operation.ID)
	pocket, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	credentials, err := manager.pocketIdCredentials(t.Context(), pocket)
	assert.NoErr(err)
	assert.NoErr(manager.completePocketIdSetup(t.Context(), pocket, credentials["clientSecret"]))
	pocket, record, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	pocket.BackupConfigured, pocket.ConfigurationApplied = true, true
	write, err := documentWrite("organization-pocketid", organization.ID, pocket, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	waitManagementResult(t, manager, operation.ID, "succeeded")
	available, err := manager.ListOrganizationSignInNames(t.Context())
	assert.NoErr(err)
	assert.Equal(available, []string{"team"})
}

// TestManagementImplicitSharedPocketId waits for the reuse client's own setup,
// without reconfiguring the source instance or treating its readiness as success.
func TestManagementImplicitSharedPocketId(t *testing.T) {
	assert := is.New(t)
	manager, _, _, _ := organizationLoginFixture(t)
	defer manager.Close()
	organization, err := manager.OnboardOrganization(t.Context(), "shared", OrganizationOnboardingInput{PocketIdOrganization: "team"})
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "shared"}}})
	assert.NoErr(err)
	waitImplicitManagementGate(t, manager, operation.ID)
	client, record, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	client.State = "failed"
	write, err := documentWrite("organization-pocketid-client", organization.ID, client, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	waitManagementResult(t, manager, operation.ID, "failed")
}

// TestManagementImplicitPocketIdProviderFence refuses retries after another
// provider was saved, and refuses legacy success while onboarding is pending.
func TestManagementImplicitPocketIdProviderFence(t *testing.T) {
	assert := is.New(t)
	manager, _ := pocketIdQueueFixture(t)
	defer manager.Close()
	provider := managementOidcDiscovery(t)
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	organization, err := manager.OnboardOrganization(t.Context(), "team", OrganizationOnboardingInput{PocketId: &OrganizationPocketIdInput{IssuerURL: issuer, BaseURL: "https://borq.test", AdminEmail: "owner@example.test"}})
	assert.NoErr(err)
	_, err = manager.pocketIdProgress(t.Context(), organization.ID, "failed", "dependency unavailable")
	assert.NoErr(err)
	_, err = manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: issuer, BaseURL: "https://borq.test", ClientID: "another-client", ClientSecret: "private-secret"})
	assert.NoErr(err)
	declaration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "team"}}}
	_, err = manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "OIDC settings changed"))
	pocket, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(pocket.Status.State, "failed")
	err = manager.waitImplicitManagementPocketId(t.Context(), "team", managementAuthentication{OrganizationID: organization.ID})
	assert.True(errors.Is(err, ErrComponentsNotReady))
	err = manager.waitImplicitManagementPocketId(t.Context(), "team", managementAuthentication{})
	assert.True(errors.Is(err, ErrComponentsNotReady))
}

func waitImplicitManagementGate(t *testing.T, manager Manager, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		operation, err := manager.GetManagementOperation(ctx, id)
		is.New(t).NoErr(err)
		if operationFinished(operation.Status) {
			t.Fatalf("management apply finished before its identity provider: %s: %s", operation.Status, operation.Message)
		}
		if operation.Phase == "oidc" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("management apply did not wait for identity provider setup")
}
