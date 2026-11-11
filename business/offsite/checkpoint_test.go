package offsite

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// TestFrozenCheckpointRecovery binds each tenant's exact inventory to its retained key checkpoint,
// even after native pruning, and keeps PostgreSQL rehydration encrypted without a Vault restore cycle.
func TestFrozenCheckpointRecovery(t *testing.T) {
	for _, root := range []string{"sqlite/0123456789abcdef0123456789abcdef/web/generations/", "sqlite/generations/", "postgres"} {
		t.Run(root, func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			source := fixture.config("source")
			objectKey := root + "0123456789abcdef/snapshots/00000000.snapshot.lz4"
			if root == "postgres" {
				seedArchive(fixture)
			} else {
				fixture.objects["source/"+objectKey] = []byte("already encrypted Litestream snapshot")
			}
			inventory, err := Freeze(t.Context(), source)
			assert.NoErr(err)
			checkpoint, identity := publishCheckpoint(t, fixture, inventory)
			fixture.mutex.Lock()
			later := "sqlite/generations/abcdef0123456789/snapshots/00000000.snapshot.lz4"
			fixture.objects["source/"+later] = []byte("newer backup whose key could postdate checkpoint")
			delete(fixture.objects, "native/"+managementbackup.PayloadKey(checkpoint.Artifact))
			delete(fixture.objects, "native/"+managementbackup.CompletionKey(checkpoint.Artifact))
			fixture.mutex.Unlock()
			var output bytes.Buffer
			assert.NoErr(ExportFrozen(t.Context(), ExportInput{Storage: storageConfig(source), Inventory: inventory, Checkpoint: checkpoint}, &output))
			destination := fixture.config("rehydrated")
			manifest, err := Restore(t.Context(), destination, SourceID(source), bytes.NewReader(output.Bytes()))
			assert.NoErr(err)
			assert.Equal(manifest.Version, 4)
			assert.True(manifest.InventoryCapturedAt.Equal(inventory.CapturedAt))
			assert.Equal(manifest.VaultCheckpoint, checkpoint.Artifact)
			assert.Equal(len(manifest.Native), 1)
			assert.Equal(manifest.Native[0], checkpoint.Artifact)
			for _, object := range manifest.Objects {
				assert.True(object.Key != later)
			}
			file := filepath.Join(t.TempDir(), "recovered-vault.age")
			_, err = managementbackup.Download(t.Context(), destination, "openbao", checkpoint.Artifact.ID, file)
			assert.NoErr(err)
			encrypted, err := os.Open(file)
			assert.NoErr(err)
			defer encrypted.Close()
			plaintext, err := age.Decrypt(encrypted, identity)
			assert.NoErr(err)
			data, err := io.ReadAll(plaintext)
			assert.NoErr(err)
			assert.Equal(string(data), "vault snapshot containing frozen workload keys")
			fixture.mutex.Lock()
			defer fixture.mutex.Unlock()
			for key := range fixture.objects {
				assert.True(!strings.HasPrefix(key, "source/management/"))
			}
			for _, object := range manifest.Objects {
				expected := ""
				if managedKey(object.Key) {
					expected = "AES256"
				}
				assert.Equal(fixture.encryption["rehydrated/"+object.Key], expected)
			}
		})
	}
}

// TestFrozenCheckpointGuards refuses mismatched or stale key material and fails source mutations before completion.
func TestFrozenCheckpointGuards(t *testing.T) {
	for _, failure := range []string{"missing", "older", "source", "changed", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			assert := is.New(t)
			fixture := newArchiveS3(t)
			key := "sqlite/generations/0123456789abcdef/snapshots/00000000.snapshot.lz4"
			fixture.objects["source/"+key] = []byte("encrypted workload")
			source := fixture.config("source")
			inventory, err := Freeze(t.Context(), source)
			assert.NoErr(err)
			checkpoint, _ := publishCheckpoint(t, fixture, inventory)
			switch failure {
			case "missing":
				checkpoint = VaultCheckpoint{}
			case "older":
				checkpoint.Artifact.CapturedAt = inventory.CapturedAt.Add(-time.Second)
			case "source":
				inventory.Source = strings.Repeat("0", 32)
			case "changed":
				fixture.objects["source/"+key] = []byte("different workload")
			case "corrupt":
				data, err := os.ReadFile(checkpoint.File)
				assert.NoErr(err)
				data[len(data)-1] ^= 1
				assert.NoErr(os.WriteFile(checkpoint.File, data, 0600))
			}
			var output bytes.Buffer
			err = ExportFrozen(t.Context(), ExportInput{Storage: storageConfig(source), Inventory: inventory, Checkpoint: checkpoint}, &output)
			assert.True(err != nil)
			assert.True(!bytes.Contains(output.Bytes(), []byte("borq-manifest.json")))
		})
	}
}

// TestCheckpointManifestRequired makes a completed workload stream unusable without its exact key checkpoint association.
func TestCheckpointManifestRequired(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	fixture.objects["source/sqlite/generations/0123456789abcdef/snapshots/00000000.snapshot.lz4"] = []byte("encrypted workload")
	source := fixture.config("source")
	inventory, err := Freeze(t.Context(), source)
	assert.NoErr(err)
	checkpoint, _ := publishCheckpoint(t, fixture, inventory)
	var output bytes.Buffer
	assert.NoErr(ExportFrozen(t.Context(), ExportInput{Storage: storageConfig(source), Inventory: inventory, Checkpoint: checkpoint}, &output))
	for _, failure := range []string{"absent", "unrelated", "older"} {
		t.Run(failure, func(t *testing.T) {
			reader := tar.NewReader(bytes.NewReader(output.Bytes()))
			var changed bytes.Buffer
			writer := tar.NewWriter(&changed)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				assert.NoErr(err)
				body, err := io.ReadAll(reader)
				assert.NoErr(err)
				if header.Name == "borq-manifest.json" {
					var manifest Archive
					assert.NoErr(json.Unmarshal(body, &manifest))
					switch failure {
					case "absent":
						manifest.VaultCheckpoint = managementbackup.Artifact{}
					case "unrelated":
						manifest.VaultCheckpoint.ID = strings.Repeat("f", 32)
					case "older":
						manifest.InventoryCapturedAt = manifest.VaultCheckpoint.CapturedAt.Add(time.Second)
					}
					body, err = json.Marshal(manifest)
					assert.NoErr(err)
					header.Size = int64(len(body))
				}
				assert.NoErr(writer.WriteHeader(header))
				_, err = writer.Write(body)
				assert.NoErr(err)
			}
			assert.NoErr(writer.Close())
			_, err := Restore(t.Context(), fixture.config("failed-"+failure), SourceID(source), bytes.NewReader(changed.Bytes()))
			assert.True(err != nil && (strings.Contains(err.Error(), "checkpoint") || strings.Contains(err.Error(), "snapshot")))
		})
	}
}

func publishCheckpoint(t *testing.T, fixture *archiveS3, inventory FrozenInventory) (VaultCheckpoint, *age.X25519Identity) {
	t.Helper()
	assert := is.New(t)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "raft")
	assert.NoErr(os.WriteFile(file, []byte("vault snapshot containing frozen workload keys"), 0600))
	config := fixture.config("native")
	client, err := objectstore.New(config)
	assert.NoErr(err)
	id, err := managementbackup.NewID()
	assert.NoErr(err)
	artifact, err := managementbackup.Publish(t.Context(), client, managementbackup.Artifact{Version: 1, Kind: "openbao", ID: id, CapturedAt: time.Now().UTC(), Recipient: identity.Recipient().String()}, file)
	assert.NoErr(err)
	encrypted := filepath.Join(t.TempDir(), "vault.age")
	_, err = managementbackup.Download(t.Context(), config, "openbao", artifact.ID, encrypted)
	assert.NoErr(err)
	return VaultCheckpoint{Artifact: artifact, File: encrypted}, identity
}

// TestCheckpointMetadataBudget reserves recovery metadata capacity for the new
// checkpoint before committing a stream that the restore bounds would reject.
func TestCheckpointMetadataBudget(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	digest := sha256.Sum256([]byte("x"))
	for index := range 512 {
		artifact := managementbackup.Artifact{Version: 1, Kind: "openbao", ID: fmt.Sprintf("%032x", index), CapturedAt: time.Unix(1, 0).UTC(), Recipient: identity.Recipient().String(), Size: 1, SHA256: hex.EncodeToString(digest[:])}
		body, err := json.Marshal(artifact)
		assert.NoErr(err)
		body = append(body, bytes.Repeat([]byte(" "), (64<<10)-len(body))...)
		fixture.objects["source/"+managementbackup.CompletionKey(artifact)] = body
		fixture.objects["source/"+managementbackup.PayloadKey(artifact)] = []byte("x")
	}
	source := fixture.config("source")
	inventory, err := Freeze(t.Context(), source)
	assert.NoErr(err)
	checkpoint, _ := publishCheckpoint(t, fixture, inventory)
	var output bytes.Buffer
	err = ExportFrozen(t.Context(), ExportInput{Storage: storageConfig(source), Inventory: inventory, Checkpoint: checkpoint}, &output)
	assert.True(err != nil && strings.Contains(err.Error(), "completion metadata exceeds supported bounds"))
	assert.Equal(output.Len(), 0)
}
