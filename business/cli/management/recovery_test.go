package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// TestBootstrapRecoveryFile owns durable private recovery handoff and prevents
// a bootstrap retry from replacing another vault's independently retained keys.
func TestBootstrapRecoveryFile(t *testing.T) {
	assert := is.New(t)
	destination := filepath.Join(t.TempDir(), "recovery.json")
	material := bootstrapRecovery{OpenBao: managementbackup.OpenBaoRecovery{OriginalToken: "private-root-token", SealKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}}
	material.Borq.AdminToken = strings.Repeat("a", 64)
	path, err := saveBootstrapRecovery(destination, material)
	assert.NoErr(err)
	assert.Equal(path, destination)
	saved, err := os.ReadFile(path)
	assert.NoErr(err)
	var recovered bootstrapRecovery
	assert.NoErr(json.Unmarshal(saved, &recovered))
	assert.Equal(recovered, material)
	info, err := os.Stat(path)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	repeated, err := saveBootstrapRecovery(destination, material)
	assert.NoErr(err)
	assert.Equal(repeated, path)
	other := material
	other.OpenBao.OriginalToken = "different-private-token"
	_, err = saveBootstrapRecovery(destination, other)
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), other.OpenBao.OriginalToken))
	after, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.Equal(after, saved)
	token, err := ReadAdminToken(path)
	assert.NoErr(err)
	assert.True(token == material.Borq.AdminToken)
	incomplete := filepath.Join(t.TempDir(), "incomplete.json")
	assert.NoErr(os.WriteFile(incomplete, []byte(`{"borq":`), 0600))
	_, err = saveBootstrapRecovery(incomplete, material)
	assert.True(err != nil && strings.Contains(err.Error(), "incomplete"))
	retained, err := os.ReadFile(incomplete)
	assert.NoErr(err)
	assert.Equal(string(retained), `{"borq":`)
	assert.NoErr(os.Chmod(path, 0644))
	_, err = saveBootstrapRecovery(destination, material)
	assert.True(err != nil && strings.Contains(err.Error(), "private regular file"))
	assert.NoErr(os.Chmod(path, 0600))
	link := filepath.Join(t.TempDir(), "recovery.json")
	assert.NoErr(os.Symlink(path, link))
	_, err = saveBootstrapRecovery(link, material)
	assert.True(err != nil && strings.Contains(err.Error(), "private regular file"))
}

// TestManagementKeysRead owns missing, malformed and complete Kubernetes key export
// without claiming that synthetic Secrets prove OpenBao initialization.
func TestManagementKeysRead(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	connection := kubernetes.NewConnectionForTest(fake.NewClientset())
	_, ready, err := readManagementKeys(ctx, connection)
	assert.NoErr(err)
	assert.True(!ready)
	assert.NoErr(connection.Secrets().Create(ctx, kubernetes.Secret{Name: "borq-admin-auth", Namespace: defaultNamespace, Data: map[string]string{"token": strings.Repeat("a", 64)}}))
	_, ready, err = readManagementKeys(ctx, connection)
	assert.NoErr(err)
	assert.True(!ready)
	vault := kubernetes.Secret{Name: "borq-openbao-admin", Namespace: defaultNamespace, Data: map[string]string{"token": strings.Repeat("b", 64)}}
	assert.NoErr(connection.Secrets().Create(ctx, vault))
	_, _, err = readManagementKeys(ctx, connection)
	assert.True(err != nil && strings.Contains(err.Error(), "incomplete recovery material"))
	vault.Data["sealKey"] = base64.StdEncoding.EncodeToString(make([]byte, 32))
	assert.NoErr(connection.Secrets().Apply(ctx, vault))
	keys, ready, err := readManagementKeys(ctx, connection)
	assert.NoErr(err)
	assert.True(ready)
	assert.True(keys.OpenBao.SealKey == vault.Data["sealKey"])
	vault.Data["token"] = "invalid"
	assert.NoErr(connection.Secrets().Apply(ctx, vault))
	_, _, err = readManagementKeys(ctx, connection)
	assert.True(err != nil && strings.Contains(err.Error(), "invalid administrator token"))
}
