package apps

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSecretGroupCrud preserves JSON replacement and version semantics while
// guarding stale deletion, producer ownership, and accepted application references.
func TestSecretGroupCrud(t *testing.T) {
	assert := is.New(t)
	manager, _, vault, space := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	zero := 0
	created, err := manager.PutSecret(t.Context(), target, "shared", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "one", "EMPTY": ""}, BaseVersion: &zero})
	assert.NoErr(err)
	assert.Equal(created.Type, "secret-group")
	assert.Equal(created.Version, 1)
	updated, err := manager.PutSecret(t.Context(), target, "shared", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "two"}, BaseVersion: &created.Version})
	assert.NoErr(err)
	assert.Equal(updated.Data, map[string]string{"TOKEN": "two"})
	assert.Equal(updated.Version, 2)
	pinned, err := manager.GetSecret(t.Context(), target, "shared", "1")
	assert.NoErr(err)
	assert.Equal(pinned, created)
	listed, err := manager.ListSecrets(t.Context(), target)
	assert.NoErr(err)
	assert.Equal(listed, []Secret{updated})
	assert.True(errors.Is(manager.DeleteSecret(t.Context(), target, "shared", 1), ErrConflict))
	stored, record, err := loadDocument[spaceRecord](t.Context(), manager.database, "space", spaceKey(target.Organization, target.Space))
	assert.NoErr(err)
	bound, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"web:v1","env":{"name":"shared","version":"2"}}]}`))
	assert.NoErr(err)
	stored.Space.Configuration = bound
	write, err := documentWrite("space", record.Key, stored, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	err = manager.DeleteSecret(t.Context(), target, "shared", 2)
	assert.True(errors.Is(err, ErrConflict))
	stored.Space.Configuration.Apps[0].Env.Version = "0"
	record, err = manager.database.Get(t.Context(), "space", record.Key)
	assert.NoErr(err)
	write, err = documentWrite("space", record.Key, stored, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	managed := environmentGroupRecord{SpaceID: space.ID, Name: "managed", PostgresID: "db", PostgresName: "database"}
	write, err = documentWrite("environment-group", space.ID+"/managed", managed, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	assert.True(errors.Is(manager.DeleteSecret(t.Context(), target, "managed", 0), ErrEnvironmentGroupManaged))
	group, groupStored, err := loadDocument[environmentGroupRecord](t.Context(), manager.database, "environment-group", space.ID+"/shared")
	assert.NoErr(err)
	oldPath := environmentGroupPath(space.ID, "shared")
	legacyPath := legacyEnvironmentGroupPath(space.ID, "shared")
	vault.versions[legacyPath] = vault.versions[oldPath]
	delete(vault.versions, oldPath)
	group.VaultPath = legacyPath
	groupWrite, err := documentWrite("environment-group", space.ID+"/shared", group, groupStored.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{groupWrite}))
	assert.NoErr(manager.DeleteSecret(t.Context(), target, "shared", 2))
	_, err = manager.GetSecret(t.Context(), target, "shared", "latest")
	assert.True(errors.Is(err, ErrNotFound))
	assert.True(vault.versions[legacyPath][0] == nil)
	recreated, err := manager.PutSecret(t.Context(), target, "shared", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "replacement"}, BaseVersion: &zero})
	assert.NoErr(err)
	assert.Equal(recreated.Version, 3)
	_, err = manager.PutSecret(t.Context(), target, "shared", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "stale"}, BaseVersion: &created.Version})
	assert.True(errors.Is(err, ErrConflict))
	assert.True(errors.Is(manager.DeleteSecret(t.Context(), target, "shared", created.Version), ErrConflict))
	legacyGroup, err := documentWrite("environment-group", space.ID+"/ghcr.io", environmentGroupRecord{SpaceID: space.ID, Name: "ghcr.io"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{legacyGroup}))
	vault.versions[environmentGroupPath(space.ID, "ghcr.io")] = []map[string]string{{"TOKEN": "legacy"}}
	first := 1
	edited, err := manager.PutSecret(t.Context(), target, "ghcr.io", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "edited"}, BaseVersion: &first})
	assert.NoErr(err)
	assert.Equal(edited.Version, 2)
	assert.Equal(edited.Data, map[string]string{"TOKEN": "edited"})
	implicit, err := manager.PutSecret(t.Context(), target, "acme-production-deleted", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "unused"}})
	assert.NoErr(err)
	assert.NoErr(manager.DeleteSecret(t.Context(), target, implicit.Name, implicit.Version))
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"deleted","image":"image:v1"}]}`))
	assert.NoErr(err)
	for _, app := range plan.Configuration.Apps {
		if app.Name == "deleted" {
			assert.Equal(app.Env.Version, "0")
		}
	}
	_, err = manager.PutSecret(t.Context(), target, "shared", PutSecretInput{Type: "future", Data: map[string]string{}})
	assert.True(errors.Is(err, ErrInvalid))
}

// TestSecretRegistryCrud owns safe metadata reads, write-only token replacement,
// durable deletion, scope isolation, and revocation without application operations.
func TestSecretRegistryCrud(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "secrets.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	cluster := newManagerCluster(t, managerClusterOptions{})
	space := createManagerEnvironment(t, manager, cluster, "production")
	createManagerEnvironment(t, manager, cluster, "other")
	target := EnvironmentGroupTarget{Organization: "acme", Space: "production"}
	reserved, err := documentWrite("environment-group", space.ID+"/ghcr.io", environmentGroupRecord{SpaceID: space.ID, Name: "ghcr.io"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{reserved}))
	_, err = manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "alice", "token": "private-one"}})
	assert.True(errors.Is(err, ErrConflict))
	staged, err := manager.stageRegistries(ctx, map[string]RegistryAuth{"ghcr.io": {Username: "alice", Token: "private-one"}}, space.ID)
	assert.NoErr(err)
	assert.True(errors.Is(manager.activateSpaceRegistries(ctx, space, staged), ErrConflict))
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{{Collection: "environment-group", Key: space.ID + "/ghcr.io", ExpectedRevision: 1, Delete: true}}))
	zero := 0
	created, err := manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "alice", "token": "private-one"}, BaseVersion: &zero})
	assert.NoErr(err)
	assert.Equal(created.Data, map[string]string{"server": "ghcr.io", "username": "alice"})
	_, err = manager.PublishEnvironmentGroup(ctx, PublishEnvironmentGroupInput{Target: target, Name: "ghcr.io", Values: []Variable{{Name: "TOKEN", Value: "conflict"}}})
	assert.True(errors.Is(err, ErrConflict))
	refs, err := manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	firstRef := refs["ghcr.io"].CredentialsRef
	updated, err := manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"server": "ghcr.io", "username": "bob"}, BaseVersion: &created.Version})
	assert.NoErr(err)
	assert.Equal(updated.Data, map[string]string{"server": "ghcr.io", "username": "bob"})
	listed, err := manager.ListSecrets(ctx, target)
	assert.NoErr(err)
	assert.Equal(listed, []Secret{updated})
	_, err = manager.GetSecret(ctx, target, "ghcr.io", "1")
	assert.True(errors.Is(err, ErrNotFound))
	refs, err = manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	secondRef := refs["ghcr.io"].CredentialsRef
	credentials, err := manager.registryValues(ctx, "ghcr.io", refs["ghcr.io"])
	assert.NoErr(err)
	assert.Equal(credentials.Token, "private-one")
	_, err = manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "lost", "token": "lost"}, BaseVersion: &created.Version})
	assert.True(errors.Is(err, ErrConflict))
	_, err = manager.GetSecret(ctx, EnvironmentGroupTarget{Organization: "acme", Space: "other"}, "ghcr.io", "latest")
	assert.True(errors.Is(err, ErrNotFound))
	assert.True(errors.Is(manager.DeleteSecret(ctx, target, "ghcr.io", created.Version), ErrConflict))
	assert.NoErr(manager.DeleteSecret(ctx, target, "ghcr.io", updated.Version))
	vault, err := platform.ManagementVault(ctx, fixture.kubeconfig, "borq")
	assert.NoErr(err)
	for _, ref := range []string{firstRef, secondRef} {
		_, err := vault.ReadKv(ctx, "secret", ref, 1)
		assert.True(errors.Is(err, openbao.ErrNotFound))
	}
	operations, err := manager.database.List(ctx, "operation")
	assert.NoErr(err)
	assert.Equal(len(operations), 0)
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer reopened.Close()
	refs, err = reopened.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	assert.Equal(len(refs), 0)
	_, err = reopened.GetSecret(ctx, target, "ghcr.io", "latest")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestSecretValidation rejects ambiguous secret types and registry keys before
// credentials can be staged, with errors that never include submitted values.
func TestSecretValidation(t *testing.T) {
	assert := is.New(t)
	manager, _, _, _ := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	negative := -1
	for _, input := range []struct {
		name string
		body PutSecretInput
	}{
		{"shared", PutSecretInput{Type: "future", Data: map[string]string{"TOKEN": "private-value"}}},
		{"shared", PutSecretInput{Type: "secret-group", BaseVersion: &negative}},
		{"docker.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "user", "token": "private-value"}}},
		{"ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"username": "user", "unsupported": "private-value"}}},
		{"shared", PutSecretInput{Type: "secret-group", Data: map[string]string{"invalid=key": "private-value"}}},
	} {
		_, err := manager.PutSecret(t.Context(), target, input.name, input.body)
		assert.True(errors.Is(err, ErrInvalid))
		assert.True(!strings.Contains(err.Error(), "private-value"))
	}
}
