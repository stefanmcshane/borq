package apps

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationOidcObservedOriginChange owns current origin authorization and
// durable network-state precedence over stale startup addresses, without asserting controller readiness.
func TestOrganizationOidcObservedOriginChange(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	provider := managementOidcDiscovery(t)
	first, second := "https://original.tail.ts.net", "https://replacement.tail.ts.net"
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq", Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, ManagementURL: first}}}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	_, err = manager.OnboardOrganization(t.Context(), "external", OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &OrganizationOidcInput{IssuerURL: provider.URL, BaseURL: "https://borq.example.test", ClientID: "client", ClientSecret: "secret"}})
	assert.NoErr(err)
	connection, err := manager.PrepareOrganizationLogin(t.Context(), "external", strings.TrimPrefix(first, "https://"))
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationOidcCallback(t.Context(), "external", connection.CallbackURL))
	write, err := documentWrite("management-network-state", "shared", platform.NetworkState{ManagementURL: second}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	assert.True(errors.Is(manager.ValidateOrganizationOidcCallback(t.Context(), "external", connection.CallbackURL), ErrInvalid))
	_, err = manager.PrepareOrganizationLogin(t.Context(), "external", strings.TrimPrefix(first, "https://"))
	assert.True(errors.Is(err, ErrInvalid))
	current, err := manager.PrepareOrganizationLogin(t.Context(), "external", strings.TrimPrefix(second, "https://"))
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationOidcCallback(t.Context(), "external", current.CallbackURL))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	assert.True(errors.Is(manager.ValidateOrganizationOidcCallback(t.Context(), "external", connection.CallbackURL), ErrInvalid))
	assert.NoErr(manager.ValidateOrganizationOidcCallback(t.Context(), "external", current.CallbackURL))
	// Removing tailnet access revokes its callback even if an old observation remains.
	write, err = documentWrite("management-network-settings", "shared", managementNetworkConfiguration{BorqPublicURL: "https://borq.example.test"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	assert.True(errors.Is(manager.ValidateOrganizationOidcCallback(t.Context(), "external", current.CallbackURL), ErrInvalid))
	saved, err := manager.GetOrganizationOidc(t.Context(), "external")
	assert.NoErr(err)
	assert.Equal(saved.CallbackURLs, []string{saved.CallbackURL})
	host, err := url.Parse(saved.BaseURL)
	assert.NoErr(err)
	_, err = manager.PrepareOrganizationLogin(t.Context(), "external", host.Host)
	assert.NoErr(err)
}
