package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// TestManagementBackupExporter consumes the private frozen inventory without relisting
// and emits its exact required key checkpoint in the completed transport stream.
func TestManagementBackupExporter(t *testing.T) {
	assert := is.New(t)
	key := "sqlite/generations/0123456789abcdef/snapshots/00000000.snapshot.lz4"
	body := []byte("encrypted workload fixture")
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/source/"+key)
		assert.Equal(request.Header.Get("If-Match"), `"frozen"`)
		writer.Header().Set("ETag", `"frozen"`)
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = writer.Write(body)
	}))
	defer source.Close()
	identity, err := age.GenerateX25519Identity()
	assert.NoErr(err)
	var encrypted bytes.Buffer
	encryptor, err := age.Encrypt(&encrypted, identity.Recipient())
	assert.NoErr(err)
	_, err = encryptor.Write([]byte("vault containing workload keys"))
	assert.NoErr(err)
	assert.NoErr(encryptor.Close())
	directory := t.TempDir()
	checkpointFile := filepath.Join(directory, "vault.age")
	assert.NoErr(os.WriteFile(checkpointFile, encrypted.Bytes(), 0600))
	digest := sha256.Sum256(encrypted.Bytes())
	now := time.Now().UTC()
	storage := offsite.StorageConfig{Endpoint: source.URL, Region: "us-east-1", Bucket: "source", PathStyle: true, AccessKey: "private-access", SecretKey: "private-secret"}
	input := offsite.ExportInput{Storage: storage, Inventory: offsite.FrozenInventory{Source: offsite.SourceID(storage.Config()), CapturedAt: now}, Checkpoint: offsite.VaultCheckpoint{File: checkpointFile, Artifact: managementbackup.Artifact{Version: 1, Kind: "openbao", ID: "0123456789abcdef0123456789abcdef", CapturedAt: now, Recipient: identity.Recipient().String(), SHA256: hex.EncodeToString(digest[:]), Size: int64(encrypted.Len())}}}
	// Capture the identity the exporter must read; listing the current bucket would hit the wrong route above.
	input.Inventory.Objects = append(input.Inventory.Objects, objectstore.Object{Key: key, Size: int64(len(body)), ETag: `"frozen"`})
	configFile := filepath.Join(directory, "source.json")
	data, err := json.Marshal(input)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(configFile, data, 0600))
	command := newManagementBackupsCommand()
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetArgs([]string{"export", "--source-config", configFile})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	assert.NoErr(command.Execute())
	reader := tar.NewReader(&output)
	keys := []string{}
	var manifest offsite.Archive
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		assert.NoErr(err)
		keys = append(keys, header.Name)
		if header.Name == "borq-manifest.json" {
			assert.NoErr(json.NewDecoder(reader).Decode(&manifest))
		}
	}
	assert.Equal(keys, []string{managementbackup.CompletionKey(input.Checkpoint.Artifact), managementbackup.PayloadKey(input.Checkpoint.Artifact), key, "borq-manifest.json"})
	assert.Equal(manifest.Version, 4)
	assert.Equal(manifest.VaultCheckpoint, input.Checkpoint.Artifact)
	assert.NoErr(os.Chmod(configFile, 0644))
	command = newManagementBackupsCommand()
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetArgs([]string{"export", "--source-config", configFile})
	output.Reset()
	command.SetOut(&output)
	command.SetErr(io.Discard)
	assert.True(command.Execute() != nil)
	assert.Equal(output.Len(), 0)
}
