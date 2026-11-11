package apps

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

// TestNamedRegistrySecrets separates name and server, isolates generations of
// credentials sharing a server, and preserves old untagged GHCR references.
func TestNamedRegistrySecrets(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	fixture := newManagementStorageFixture(t)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "registry.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	cluster := newManagerCluster(t, managerClusterOptions{})
	space := createManagerEnvironment(t, manager, cluster, "production")
	target := EnvironmentGroupTarget{Organization: "acme", Space: "production"}
	first, err := manager.PutSecret(ctx, target, "github-packages", PutSecretInput{Type: "registry", Data: map[string]string{"server": "https://GHCR.IO/", "username": "alice", "password": "first-password"}})
	assert.NoErr(err)
	assert.Equal(first.Name, "github-packages")
	assert.Equal(first.Data, map[string]string{"server": "ghcr.io", "username": "alice"})
	current, err := manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	firstAuth := current[first.Name]
	peer, err := manager.PutSecret(ctx, target, "github-builds", PutSecretInput{Type: "registry", Data: map[string]string{"server": "ghcr.io", "username": "builder", "password": "peer-password"}})
	assert.NoErr(err)
	current, err = manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	peerAuth := current[peer.Name]
	_, err = manager.stageRegistries(ctx, map[string]RegistryAuth{"other-name": firstAuth}, space.ID)
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.PutSecret(ctx, target, first.Name, PutSecretInput{Type: "registry", Data: map[string]string{"server": "registry.example.com:5000/team/", "username": "alice"}})
	assert.True(errors.Is(err, ErrInvalid))
	updated, err := manager.PutSecret(ctx, target, first.Name, PutSecretInput{Type: "registry", Data: map[string]string{"server": "https://registry.example.com:5000/team/", "username": "alice", "password": "replacement-password"}})
	assert.NoErr(err)
	assert.Equal(updated.Data, map[string]string{"server": "registry.example.com:5000/team/", "username": "alice"})
	current, err = manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	updatedAuth := current[first.Name]
	vault, err := platform.ManagementVault(ctx, fixture.kubeconfig, "borq")
	assert.NoErr(err)
	entry, err := vault.ReadKv(ctx, "secret", updatedAuth.CredentialsRef, 1)
	assert.NoErr(err)
	var docker struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	assert.NoErr(json.Unmarshal([]byte(entry.Values[".dockerconfigjson"]), &docker))
	decoded, err := base64.StdEncoding.DecodeString(docker.Auths["registry.example.com:5000/team/"].Auth)
	assert.NoErr(err)
	assert.Equal(string(decoded), "alice:replacement-password")
	assert.True(docker.Auths["github-packages"].Auth == "")
	assert.NoErr(manager.DeleteSecret(ctx, target, first.Name, updated.Version))
	for _, auth := range []RegistryAuth{firstAuth, updatedAuth} {
		_, err := vault.ReadKv(ctx, "secret", auth.CredentialsRef, 1)
		assert.True(errors.Is(err, openbao.ErrNotFound))
	}
	_, err = vault.ReadKv(ctx, "secret", peerAuth.CredentialsRef, 1)
	assert.NoErr(err)
	// Simulate an existing GHCR document from before named credential metadata.
	id, err := newId()
	assert.NoErr(err)
	legacy := RegistryAuth{CredentialsRef: "borq/registry-auth/" + space.ID + "/" + id}
	encoded := base64.StdEncoding.EncodeToString([]byte("legacy-user:legacy-token"))
	_, err = vault.WriteKv(ctx, "secret", legacy.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{".dockerconfigjson": `{"auths":{"ghcr.io":{"auth":"` + encoded + `"}}}`}})
	assert.NoErr(err)
	assert.NoErr(manager.activateSpaceRegistries(ctx, space, map[string]RegistryAuth{"ghcr.io": legacy}))
	old, err := manager.GetSecret(ctx, target, "ghcr.io", "latest")
	assert.NoErr(err)
	assert.Equal(old.Data, map[string]string{"server": "ghcr.io", "username": "legacy-user"})
	// The old hostname is also a valid independent name for a different registry.
	custom, err := manager.PutSecret(ctx, target, "ghcr.io", PutSecretInput{Type: "registry", Data: map[string]string{"server": "quay.io", "username": "quay-user", "password": "quay-password"}})
	assert.NoErr(err)
	current, err = manager.readRegistries(ctx, space.ID)
	assert.NoErr(err)
	assert.NoErr(manager.verifyRegistryManifests(ctx, space.ID, registryExternalSecrets(current, space.Namespace, space.ID, "borq-registry-openbao")))
	assert.Equal(custom.Data, map[string]string{"server": "quay.io", "username": "quay-user"})
	assert.NoErr(manager.Close())
	reopened, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer reopened.Close()
	kept, err := reopened.GetSecret(ctx, target, peer.Name, "latest")
	assert.NoErr(err)
	assert.Equal(kept.Data, map[string]string{"server": "ghcr.io", "username": "builder"})
}

// TestRegistryAddresses validates Docker config registry keys, including Hub's
// legacy canonical key, without accepting credentials or URL suffixes as hosts.
func TestRegistryAddresses(t *testing.T) {
	for _, input := range []struct{ input, want string }{
		{"ghcr.io", "ghcr.io"}, {"https://GHCR.IO/", "ghcr.io"}, {"registry.example.com:5000/team/", "registry.example.com:5000/team/"},
		{"https://registry.example.com/v2/", "registry.example.com/v2/"},
		{"https://registry.example.com/team/", "registry.example.com/team/"},
		{"registry-1.docker.io", "registry-1.docker.io"},
		{"docker.io", "https://index.docker.io/v1/"}, {"https://index.docker.io/v1/", "https://index.docker.io/v1/"}, {"localhost:5000", "localhost:5000"}, {"[::1]:5000", "[::1]:5000"},
		{"", ""}, {"https://user:password@example.com", ""}, {"https://example.com?token=private", ""}, {"https://example.com#private", ""}, {"ftp://example.com", ""}, {"example.com:70000", ""}, {"example.com:", ""}, {"https://example.com/../private", ""}, {"https://*.example.com", ""},
	} {
		t.Run(input.input, func(t *testing.T) {
			assert := is.New(t)
			got, err := normalizeRegistryServer(input.input)
			if input.want == "" {
				assert.True(errors.Is(err, ErrInvalid))
				return
			}
			assert.NoErr(err)
			assert.Equal(got, input.want)
		})
	}
}
