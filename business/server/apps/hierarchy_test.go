package apps

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationManagedAddonsDefault preserves automatic availability while
// retaining an explicit opt-out across idempotent creation and reopening storage.
func TestOrganizationManagedAddonsDefault(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, databasePath, 0)
	created, err := manager.CreateOrganization(ctx, "enabled")
	assert.NoErr(err)
	assert.True(created.ManagedAddons)
	_, err = manager.CreateOrganization(ctx, "disabled")
	assert.NoErr(err)
	disabled, err := manager.SetOrganizationManagedAddons(ctx, "disabled", false)
	assert.NoErr(err)
	assert.True(!disabled.ManagedAddons)
	repeated, err := manager.CreateOrganization(ctx, "disabled")
	assert.NoErr(err)
	assert.Equal(repeated, disabled)
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, databasePath, 0)
	organizations, err := reopened.ListOrganizations(ctx)
	assert.NoErr(err)
	assert.Equal(organizations, []Organization{disabled, created})
}

// TestLinkClusterAllocatesSpace verifies unique names and atomic initial spaces
// survive a restart without moving existing organization-wide space bindings.
func TestLinkClusterAllocatesSpace(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, 0)
	_, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	_, err = manager.LinkCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: "missing"})
	assert.True(err != nil)
	spaces, err := manager.ListSpaces(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(len(spaces), 0)
	clusters, err := manager.ListClusters(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(len(clusters), 0)
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager.runtime.platformSettings.Network.Cloudflare = &platform.CloudflareSettings{}
	first, err := manager.LinkCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	assert.True(regexp.MustCompile(`^local-[a-f0-9]{5}$`).MatchString(first.Name))
	initial, err := manager.GetSpace(ctx, "acme", "default")
	assert.NoErr(err)
	assert.Equal(initial.Cluster, first.Name)
	assert.Equal(initial.CertificateIssuer, "letsencrypt-cloudflare")
	second, err := manager.LinkCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	assert.True(regexp.MustCompile(`^local-[a-f0-9]{5}$`).MatchString(second.Name))
	assert.True(first.Name != second.Name)
	assert.True(first.ID != second.ID)
	later, err := manager.GetSpace(ctx, "acme", "default-"+second.Name)
	assert.NoErr(err)
	assert.Equal(later.Cluster, second.Name)
	assert.True(later.Namespace != initial.Namespace)
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "default", Cluster: second.Name})
	assert.True(errors.Is(err, ErrConflict))
	long, err := manager.LinkCluster(ctx, ClusterInput{Organization: "acme", Name: strings.Repeat("a", 63), KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	assert.True(len("default-"+long.Name) <= 63)
	_, err = manager.CreateOrganization(ctx, "another")
	assert.NoErr(err)
	other, err := manager.LinkCluster(ctx, ClusterInput{Organization: "another", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	assert.Equal(cluster.namespaceAttempts(), 0)
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, 0)
	for name, expected := range map[string]Space{"default": initial, later.Name: later} {
		saved, err := reopened.GetSpace(ctx, "acme", name)
		assert.NoErr(err)
		assert.Equal(saved, expected)
	}
	saved, err := reopened.GetSpace(ctx, "another", "default")
	assert.NoErr(err)
	assert.Equal(saved.Cluster, other.Name)
	assert.True(saved.Namespace != initial.Namespace)
}

// TestClusterSpacesAndBinding verifies registrations leave space creation explicit,
// while isolated namespace bindings survive a database reopen.
func TestClusterSpacesAndBinding(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, 0)
	organizations, err := manager.ListOrganizations(ctx)
	assert.NoErr(err)
	assert.Equal(organizations, []Organization{})
	_, err = manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	spaces, err := manager.ListSpaces(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(spaces, []Space{})
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "unbound"})
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.GetSpace(ctx, "acme", "unbound")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.ListClusters(ctx, "missing")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "missing", Name: "space", Cluster: "local"})
	assert.True(errors.Is(err, ErrNotFound))
	cluster := newManagerCluster(t, managerClusterOptions{})
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: "missing", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.True(errors.Is(err, ErrNotFound))
	for _, name := range []string{"local", "remote"} {
		_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: name, KubeconfigPath: cluster.kubeconfig})
		assert.NoErr(err)
	}
	spaces, err = manager.ListSpaces(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(spaces, []Space{})
	_, err = manager.GetSpace(ctx, "acme", "local")
	assert.True(errors.Is(err, ErrNotFound))
	initial, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "local", Cluster: "local"})
	assert.NoErr(err)
	assert.Equal(initial.Cluster, "local")
	assert.True(strings.HasPrefix(initial.Namespace, "borq-"))
	assert.Equal(initial.CertificateIssuer, "borq")
	bound, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "staging", Cluster: "local"})
	assert.NoErr(err)
	assert.Equal(bound.Cluster, "local")
	assert.True(bound.Namespace != initial.Namespace)
	assert.Equal(bound.CertificateIssuer, "borq")
	repeated, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "staging", Cluster: "local"})
	assert.NoErr(err)
	assert.Equal(repeated, bound)
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "staging", Cluster: "remote"})
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "staging", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	_, _, err = loadDocument[Cluster](ctx, manager.database, "cluster", "acme/staging")
	assert.NoErr(err)
	preserved, err := manager.GetSpace(ctx, "acme", "staging")
	assert.NoErr(err)
	assert.Equal(preserved, bound)
	legacy := spaceRecord{Space: Space{ID: "legacy-id", Organization: "acme", Name: "legacy", Namespace: bound.Namespace, CertificateIssuer: "existing-issuer", Configuration: Configuration{Apps: []App{}}}}
	write, err := documentWrite("space", "acme/legacy", legacy, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "legacy", Cluster: "local"})
	assert.True(errors.Is(err, ErrConflict)) // Legacy namespaces still cannot overlap on one cluster.
	legacyBound, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "legacy", Cluster: "remote"})
	assert.NoErr(err)
	assert.Equal(legacyBound.ID, legacy.Space.ID)
	assert.Equal(legacyBound.Namespace, bound.Namespace)
	assert.Equal(legacyBound.CertificateIssuer, "existing-issuer")
	_, err = manager.CreateOrganization(ctx, "another")
	assert.NoErr(err)
	_, err = manager.CreateSpace(ctx, SpaceInput{Organization: "another", Name: "default", Cluster: "local"})
	assert.True(errors.Is(err, ErrNotFound))
	assert.Equal(cluster.namespaceAttempts(), 0)
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, 0)
	saved, err := reopened.GetSpace(ctx, "acme", "staging")
	assert.NoErr(err)
	assert.Equal(saved, bound)
	_, err = reopened.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "local", KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	savedInitial, err := reopened.GetSpace(ctx, "acme", "local")
	assert.NoErr(err)
	assert.Equal(savedInitial, initial)
	spaces, err = reopened.ListSpaces(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(len(spaces), 3)
	for _, space := range spaces {
		assert.True(space.Cluster != "")
	}
}
