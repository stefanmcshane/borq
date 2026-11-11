package apps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationEnrollmentDuringPolicySelection fences the initial account pin
// while management accepts a different account for an unenrolled legacy provider.
func TestOrganizationEnrollmentDuringPolicySelection(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	source, err := manager.organizationPocketIdSource(t.Context(), "team")
	assert.NoErr(err)
	declaration := ManagementConfiguration{
		Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: source.Status.IssuerURL,
			CertificateIssuer: source.Status.CertificateIssuer, AdminUsername: source.Status.AdminUsername, AdminEmail: source.Status.AdminEmail, BaseURL: source.Status.BaseURL, OrganizationAdminEmail: "selected@example.test"}}},
	}
	result := make(chan error, 1)
	started := make(chan struct{})
	func() {
		manager.runtime.mutex.Lock()
		defer manager.runtime.mutex.Unlock()
		go func() {
			close(started)
			_, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
			result <- err
		}()
		<-started
		select {
		case <-result:
			t.Fatal("first enrollment escaped management's pending account selection")
		case <-time.After(100 * time.Millisecond):
		}
		_, writes, err := manager.stageManagementAuthentication(t.Context(), &declaration)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), writes))
	}()
	select {
	case err := <-result:
		assert.NoErr(err)
		login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
		assert.NoErr(err)
		assert.True(login.MemberID != "")
		permissions, err := manager.OrganizationLoginPermissions(t.Context(), login)
		assert.NoErr(err)
		assert.Equal(permissions, []Permission{})
	case <-time.After(5 * time.Second):
		t.Fatal("enrollment did not resume after account selection")
	}
	identity.Email, identity.Subject = "selected@example.test", "selected-account"
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
}

// organizationLoginFixture seeds durable ready state for authorization tests; it does not exercise Kubernetes readiness.
func organizationLoginFixture(t *testing.T) (Manager, NewInput, string, oidc.Identity) {
	t.Helper()
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	t.Cleanup(provider.Close)
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	organization, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", OrganizationPocketIdInput{IssuerURL: issuer, BaseURL: "https://borq.test", AdminEmail: "owner@example.test"})
	assert.NoErr(err)
	pocket, record, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	_, err = manager.putOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: issuer, BaseURL: pocket.Status.BaseURL, ClientID: pocket.ID, ClientSecret: "test-client-secret"}, nil)
	assert.NoErr(err)
	pocket.Status.State, pocket.CredentialsStored = "ready", true
	write, err := documentWrite("organization-pocketid", organization.ID, pocket, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	saved, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	return manager, configuration, saved.Generation, oidc.Identity{Issuer: issuer, Subject: "initial-admin", Email: " OWNER@Example.Test ", EmailVerified: true, ExpiresAt: time.Now().Add(time.Hour)}
}

// TestProvisionedPocketIdAdministratorLogin authorizes the exact preselected
// Pocket ID subject without treating an unverified email as proof of membership.
func TestProvisionedPocketIdAdministratorLogin(t *testing.T) {
	for _, name := range []string{"legacy", "bound policy", "other subject", "other email", "selected alternate administrator", "external provider", "changed client"} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			manager, configuration, generation, identity := organizationLoginFixture(t)
			defer func() { _ = manager.Close() }()
			pocket, err := manager.organizationPocketIdSource(t.Context(), "team")
			assert.NoErr(err)
			identity.Subject, identity.Email, identity.EmailVerified = pocket.adminID(), pocket.Status.AdminEmail, false
			organization := "team"
			if name == "external provider" {
				_, err := manager.OnboardOrganization(t.Context(), "external", OrganizationOnboardingInput{AdminEmail: pocket.Status.AdminEmail,
					Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: identity.Issuer, BaseURL: "https://borq.test", ClientID: "external-client", ClientSecret: "external-secret"}})
				assert.NoErr(err)
				provider, _, err := manager.organizationOidc(t.Context(), "external")
				assert.NoErr(err)
				generation, organization = provider.Generation, "external"
			} else if name != "legacy" {
				policy := organizationLoginPolicy{OrganizationID: pocket.OrganizationID, AdminEmail: pocket.Status.AdminEmail,
					Issuer: pocket.Status.IssuerURL, ClientID: pocket.ID, BaseURL: pocket.Status.BaseURL, PocketInstanceID: pocket.ID}
				if name == "selected alternate administrator" {
					policy.AdminEmail, identity.Email = "selected@example.test", "selected@example.test"
				}
				if name == "changed client" {
					policy.PocketInstanceID = ""
				}
				write, err := documentWrite("organization-login-policy", pocket.OrganizationID, policy, 0)
				assert.NoErr(err)
				assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
				if name == "changed client" {
					settings, err := manager.GetOrganizationOidc(t.Context(), "team")
					assert.NoErr(err)
					_, err = manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: identity.Issuer, BaseURL: "https://borq.test", ClientID: "replacement-client", ClientSecret: "replacement-secret", Revision: settings.Revision})
					assert.NoErr(err)
					provider, _, err := manager.organizationOidc(t.Context(), "team")
					assert.NoErr(err)
					generation = provider.Generation
				}
			}
			if name == "other subject" {
				identity.Subject = "other-user-with-the-same-email"
			}
			if name == "other email" {
				identity.Email = "other@example.test"
			}
			_, _, err = manager.organizationLoginProvider(t.Context(), organization)
			assert.NoErr(err)
			login, err := manager.AuthorizeOrganizationLogin(t.Context(), organization, generation, identity)
			if name == "other email" {
				assert.NoErr(err)
				assert.True(login.MemberID != "")
				permissions, err := manager.OrganizationLoginPermissions(t.Context(), login)
				assert.NoErr(err)
				assert.Equal(permissions, []Permission{})
				return
			}
			if name != "legacy" && name != "bound policy" {
				assert.True(err != nil)
				return
			}
			assert.NoErr(err)
			assert.NoErr(manager.Close())
			manager, err = NewManager(t.Context(), configuration)
			assert.NoErr(err)
			assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
			_, err = manager.AuthorizeOrganizationLogin(t.Context(), organization, generation, identity)
			assert.NoErr(err)
			identity.Subject = "replacement-account"
			replacement, err := manager.AuthorizeOrganizationLogin(t.Context(), organization, generation, identity)
			assert.NoErr(err)
			assert.True(replacement.MemberID != "")
			permissions, err := manager.OrganizationLoginPermissions(t.Context(), replacement)
			assert.NoErr(err)
			assert.Equal(permissions, []Permission{})
		})
	}
}

// TestOrganizationLoginEnrollment owns initial administrator selection and durable identity pins across manager restarts.
func TestOrganizationLoginEnrollment(t *testing.T) {
	assert := is.New(t)
	manager, configuration, generation, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	prepared, err := manager.PrepareOrganizationLogin(t.Context(), "team", "")
	assert.NoErr(err)
	assert.Equal(prepared.Generation, generation)
	for _, test := range []struct {
		name   string
		change func(*oidc.Identity)
	}{
		{"unverified email", func(identity *oidc.Identity) { identity.EmailVerified = false }},
		{"other issuer", func(identity *oidc.Identity) { identity.Issuer = "https://other.test" }},
		{"missing subject", func(identity *oidc.Identity) { identity.Subject = "" }},
		{"expired identity", func(identity *oidc.Identity) { identity.ExpiresAt = time.Now().Add(-time.Minute) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			modified := identity
			test.change(&modified)
			_, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, modified)
			assert.True(err != nil)
		})
	}
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", "stale-generation", identity)
	assert.True(err != nil)
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	identity.EmailVerified = false
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err) // The verified issuer and pinned subject identify an enrolled account.
	reassigned := identity
	reassigned.Subject = "new-owner-of-same-email"
	replacement, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, reassigned)
	assert.NoErr(err)
	assert.True(replacement.MemberID != "")
	permissions, err := manager.OrganizationLoginPermissions(t.Context(), replacement)
	assert.NoErr(err)
	assert.Equal(permissions, []Permission{})
	identity.Email = "administrator-renamed@example.test"
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	login.ExpiresAt = time.Now().Add(-time.Minute)
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
}

// TestOrganizationLoginRevocation checks current provider, managed readiness, ownership and pin on every session validation.
func TestOrganizationLoginRevocation(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	for _, test := range []struct {
		name, collection string
		change           func(map[string]any)
	}{
		{"provider generation", "organization-oidc", func(document map[string]any) { document["generation"] = "new-generation" }},
		{"provider issuer", "organization-oidc", func(document map[string]any) {
			document["settings"].(map[string]any)["issuerUrl"] = "https://other.test"
		}},
		{"provider client", "organization-oidc", func(document map[string]any) { document["settings"].(map[string]any)["clientId"] = "other-client" }},
		{"provider base address", "organization-oidc", func(document map[string]any) {
			document["settings"].(map[string]any)["borqPublicUrl"] = "https://other.test"
		}},
		{"provider ownership", "organization-oidc", func(document map[string]any) { document["organizationId"] = "other-organization" }},
		{"managed readiness", "organization-pocketid", func(document map[string]any) { document["status"].(map[string]any)["state"] = "failed" }},
		{"managed ownership", "organization-pocketid", func(document map[string]any) { document["organization"] = "other" }},
		{"pinned ownership", "organization-login-admin", func(document map[string]any) { document["organizationId"] = "other-organization" }},
		{"pinned issuer", "organization-login-admin", func(document map[string]any) { document["issuer"] = "https://other.test" }},
		{"pinned subject", "organization-login-admin", func(document map[string]any) { document["subject"] = "other-admin" }},
		{"recreated organization", "organization", func(document map[string]any) { document["id"] = "0123456789abcdef0123456789abcdef" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			key := login.OrganizationID
			if test.collection == "organization" {
				key = "team"
			}
			record, err := manager.database.Get(t.Context(), test.collection, key)
			assert.NoErr(err)
			var document map[string]any
			assert.NoErr(json.Unmarshal(record.Value, &document))
			test.change(document)
			write, err := documentWrite(test.collection, key, document, record.Revision)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
			if test.collection != "organization-login-admin" {
				_, err = manager.PrepareOrganizationLogin(t.Context(), "team", "")
				if test.name != "provider generation" {
					assert.True(err != nil)
				}
			}
			candidate, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
			if test.name == "pinned subject" {
				assert.NoErr(err)
				permissions, err := manager.OrganizationLoginPermissions(t.Context(), candidate)
				assert.NoErr(err)
				assert.Equal(permissions, []Permission{})
			} else {
				assert.True(err != nil)
			}
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{{Collection: record.Collection, Key: record.Key, Value: record.Value, ExpectedRevision: record.Revision + 1}}))
			assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
		})
	}
}

// TestOrganizationLoginConcurrentEnrollment admits both provider accounts while granting administrator access to only one.
func TestOrganizationLoginConcurrentEnrollment(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	type result struct {
		login OrganizationLogin
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, subject := range []string{"administrator-one", "administrator-two"} {
		go func() {
			<-start
			candidate := identity
			candidate.Subject = subject
			login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, candidate)
			results <- result{login: login, err: err}
		}()
	}
	close(start)
	accepted, administrators := 0, 0
	for range 2 {
		attempt := <-results
		if attempt.err == nil {
			accepted++
			if attempt.login.MemberID == "" {
				administrators++
			}
			assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), attempt.login))
		}
	}
	assert.Equal(accepted, 2)
	assert.Equal(administrators, 1)
}
