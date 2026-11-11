package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestOrganizationOidcAdministratorEnrollment owns explicit first-account selection,
// unverified-email rejection, immutable binding, revocation and durable restart behavior.
func TestOrganizationOidcAdministratorEnrollment(t *testing.T) {
	assert := is.New(t)
	manager, configuration, _, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	_, err := manager.OnboardOrganization(t.Context(), "external", OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: identity.Issuer, BaseURL: "https://borq.test", ClientID: "external-client", ClientSecret: "external-secret"}})
	assert.NoErr(err)
	provider, _, err := manager.organizationOidc(t.Context(), "external")
	assert.NoErr(err)
	identity.Email, identity.EmailVerified = "owner@example.test", false
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "external", provider.Generation, identity)
	assert.True(err != nil)
	input := OrganizationOidcAdministratorInput{Subject: identity.Subject, Revision: provider.Settings.Revision}
	_, err = manager.EnrollOrganizationOidcAdministrator(t.Context(), "external", input)
	assert.True(errors.Is(err, ErrUnauthenticated))
	organizationAdmin := WithPrincipal(t.Context(), Principal{Permissions: []Permission{"*"}, Login: OrganizationLogin{Organization: "external"}})
	_, err = manager.EnrollOrganizationOidcAdministrator(organizationAdmin, "external", input)
	assert.True(errors.Is(err, ErrForbidden))
	root := WithPrincipal(t.Context(), Principal{Admin: true})
	stale := input
	stale.Revision++
	_, err = manager.EnrollOrganizationOidcAdministrator(root, "external", stale)
	assert.True(errors.Is(err, ErrConflict))
	for _, subject := range []string{"", " leading-space", "trailing-space ", "line\nbreak"} {
		invalid := input
		invalid.Subject = subject
		_, err = manager.EnrollOrganizationOidcAdministrator(root, "external", invalid)
		assert.True(errors.Is(err, ErrInvalid))
	}
	permissions, err := manager.EnrollOrganizationOidcAdministrator(root, "external", input)
	assert.NoErr(err)
	assert.Equal(permissions.Subject, identity.Subject)
	assert.Equal(permissions.Permissions, []Permission{"*"})
	again, err := manager.EnrollOrganizationOidcAdministrator(root, "external", input)
	assert.NoErr(err)
	assert.Equal(again.Revision, permissions.Revision)
	other := input
	other.Subject = "another-account-with-same-email"
	_, err = manager.EnrollOrganizationOidcAdministrator(root, "external", other)
	assert.True(errors.Is(err, ErrConflict))
	impostor := identity
	impostor.Subject = other.Subject
	unassigned, err := manager.AuthorizeOrganizationLogin(t.Context(), "external", provider.Generation, impostor)
	assert.NoErr(err)
	assert.True(unassigned.MemberID != "")
	grants, err := manager.OrganizationLoginPermissions(t.Context(), unassigned)
	assert.NoErr(err)
	assert.Equal(len(grants), 0)
	identity.Email = ""
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "external", provider.Generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "external", provider.Generation, identity)
	assert.NoErr(err)
	_, err = manager.PutOrganizationMember(root, "external", "initial-administrator", OrganizationMemberInput{RoleIDs: []string{}, Disabled: true, Revision: permissions.Revision})
	assert.NoErr(err)
	_, err = manager.EnrollOrganizationOidcAdministrator(root, "external", input)
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "external", provider.Generation, identity)
	assert.True(err != nil)
	current, err := manager.GetOrganizationPermissions(root, "external")
	assert.NoErr(err)
	assert.Equal(len(current.Permissions), 0)
	assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
}

// TestOrganizationOidcAdministratorMemberCollision prevents promotion of an
// established member through the initial-administrator recovery command.
func TestOrganizationOidcAdministratorMemberCollision(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	root := WithPrincipal(t.Context(), Principal{Admin: true})
	identity.Subject, identity.Email = "member", "member@example.test"
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	provider, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.EnrollOrganizationOidcAdministrator(root, "team", OrganizationOidcAdministratorInput{Subject: identity.Subject, Revision: provider.Revision})
	assert.True(errors.Is(err, ErrConflict))
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
}

// TestOrganizationOidcAdministratorDuringProviderUpdate exercises an asynchronous
// management update already in discovery while a stale enrollment tries to pin.
func TestOrganizationOidcAdministratorDuringProviderUpdate(t *testing.T) {
	assert := is.New(t)
	manager, _, _, _ := organizationLoginFixture(t)
	defer manager.Close()
	var block atomic.Bool
	discovered, release := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if block.Load() {
			select {
			case discovered <- struct{}{}:
			case <-request.Context().Done():
				return
			}
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		issuer := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"})
	}))
	defer func() {
		closeOnce.Do(func() { close(release) })
		provider.Close()
	}()
	organization, err := manager.OnboardOrganization(t.Context(), "external", OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: provider.URL, BaseURL: "https://borq.test", ClientID: "external-client", ClientSecret: "external-secret"}})
	assert.NoErr(err)
	settings, err := manager.GetOrganizationOidc(t.Context(), "external")
	assert.NoErr(err)
	block.Store(true)
	updated := make(chan error, 1)
	go func() {
		updated <- manager.applyManagementOidc(t.Context(), ManagementOrganization{Name: "external", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", IssuerURL: provider.URL, BaseURL: "https://new-borq.test", ClientID: "external-client"}}, managementAuthentication{OrganizationID: organization.ID, Revision: settings.Revision})
	}()
	select {
	case <-discovered:
	case <-time.After(5 * time.Second):
		t.Fatal("background provider update did not reach discovery")
	}
	enrolled := make(chan error, 1)
	go func() {
		_, err := manager.EnrollOrganizationOidcAdministrator(WithPrincipal(t.Context(), Principal{Admin: true}), "external", OrganizationOidcAdministratorInput{Subject: "selected-account", Revision: settings.Revision})
		enrolled <- err
	}()
	select {
	case err := <-enrolled:
		t.Fatalf("enrollment escaped the in-flight provider update: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	closeOnce.Do(func() { close(release) })
	select {
	case err := <-updated:
		assert.NoErr(err)
	case <-time.After(5 * time.Second):
		t.Fatal("provider update did not finish")
	}
	select {
	case err := <-enrolled:
		assert.True(errors.Is(err, ErrConflict))
	case <-time.After(5 * time.Second):
		t.Fatal("stale enrollment did not finish")
	}
	_, _, err = loadDocument[organizationLoginAdmin](t.Context(), manager.database, "organization-login-admin", organization.ID)
	assert.True(errors.Is(err, ErrNotFound))
}
