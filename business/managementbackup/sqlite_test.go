package managementbackup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestSQLiteBundleRecovery proves self-contained connections, durable queued-state readback and the explicit replay barrier.
func TestSQLiteBundleRecovery(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	database, err := sqlite.New(t.Context(), filepath.Join(directory, "source.sqlite"))
	assert.NoErr(err)
	cluster, _ := json.Marshal(map[string]any{"name": "remote", "kubeconfigPath": "/lost/source/client.yaml"})
	assert.NoErr(database.Commit(t.Context(), []sqlite.Write{{Collection: "cluster", Key: "org/remote", Value: cluster}, {Collection: "operation", Key: "queued", Value: json.RawMessage(`{"status":"queued"}`)}}))
	snapshot := filepath.Join(directory, "snapshot.sqlite")
	assert.NoErr(database.Snapshot(t.Context(), snapshot))
	assert.NoErr(database.Close())
	assert.NoErr(os.Remove(filepath.Join(directory, "source.sqlite")))
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: remote\n  cluster:\n    server: https://cluster.example\ncontexts:\n- name: remote\n  context:\n    cluster: remote\n    user: remote\ncurrent-context: remote\nusers:\n- name: remote\n  user:\n    token: retained-connection-token\n"
	bundle := filepath.Join(directory, "snapshot.tar")
	assert.NoErr(BundleSQLite(t.Context(), snapshot, []Connection{{Key: "org/remote", Kubeconfig: kubeconfig}}, bundle))
	destination := filepath.Join(directory, "recovered")
	assert.NoErr(RestoreSQLite(t.Context(), bundle, destination))
	operations, err := Inspect(t.Context(), destination)
	assert.NoErr(err)
	assert.Equal(len(operations), 1)
	assert.Equal(operations[0].Status, "queued")
	recovered, err := sqlite.New(t.Context(), filepath.Join(destination, "borq.sqlite"))
	assert.NoErr(err)
	record, err := recovered.Get(t.Context(), "cluster", "org/remote")
	assert.NoErr(err)
	assert.NoErr(recovered.Close())
	var value struct {
		KubeconfigPath string `json:"kubeconfigPath"`
	}
	assert.NoErr(json.Unmarshal(record.Value, &value))
	assert.True(strings.HasPrefix(value.KubeconfigPath, destination+"/clusters/"))
	saved, err := os.ReadFile(value.KubeconfigPath)
	assert.NoErr(err)
	assert.True(strings.Contains(string(saved), "retained-connection-token"))
	assert.True(RestoreSQLite(t.Context(), bundle, destination) != nil)
}

// TestNativeCiphertextAuthentication rejects wrong identities and altered authenticated EOF before native consumers run.
func TestNativeCiphertextAuthentication(t *testing.T) {
	assert := is.New(t)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	other, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	encrypted := filepath.Join(directory, "encrypted")
	assert.NoErr(os.WriteFile(source, []byte(strings.Repeat("private management credentials\n", 5000)), 0600))
	assert.NoErr(encryptFile(t.Context(), source, encrypted, identity.Recipient().String()))
	assert.True(Decrypt(t.Context(), encrypted, filepath.Join(directory, "wrong"), other.String()) != nil)
	_, err = os.Stat(filepath.Join(directory, "wrong"))
	assert.True(os.IsNotExist(err))
	data, err := os.ReadFile(encrypted)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(data), "private management credentials"))
	data[len(data)-1] ^= 1
	assert.NoErr(os.WriteFile(encrypted, data, 0600))
	assert.True(Decrypt(t.Context(), encrypted, filepath.Join(directory, "corrupt"), identity.String()) != nil)
	_, err = os.Stat(filepath.Join(directory, "corrupt"))
	assert.True(os.IsNotExist(err))
}
