package apps

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestClusterSpacesAndBinding verifies organizations start empty, registrations
// create isolated bound spaces, and automatic settings survive a database reopen.
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
	initial, err := manager.GetSpace(ctx, "acme", "local")
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
	assert.True(errors.Is(err, ErrConflict))
	_, _, err = loadDocument[Cluster](ctx, manager.database, "cluster", "acme/staging")
	assert.True(errors.Is(err, ErrNotFound)) // A conflicting initial space cannot leave a partial registration.
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
	assert.Equal(len(spaces), 4)
	for _, space := range spaces {
		assert.True(space.Cluster != "")
	}
}
