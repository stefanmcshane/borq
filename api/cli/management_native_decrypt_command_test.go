package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
)

// TestManagementNativeDecrypt owns the operator file boundary: saved combined keys
// decrypt verified artifacts, while failed authentication never publishes plaintext or overwrites a path.
func TestManagementNativeDecrypt(t *testing.T) {
	for _, test := range []struct{ name, problem string }{
		{"openbao", ""}, {"sqlite", ""},
		{"wrong key", "does not decrypt"}, {"missing key", "original OpenBao auto-unseal key"},
		{"private keys", "private regular file"}, {"existing", "must not already exist"}, {"symlink", "must not already exist"},
		{"ciphertext digest", "integrity validation"}, {"ciphertext authentication", "authentication failed"},
		{"invalid selection", "invalid native snapshot selection"}, {"cancelled", "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			sealKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
			identity, err := managementbackup.RecoveryIdentity(sealKey)
			check.NoErr(err)
			plaintext := []byte("private native snapshot content")
			var encrypted bytes.Buffer
			writer, err := age.Encrypt(&encrypted, identity.Recipient())
			check.NoErr(err)
			_, err = writer.Write(plaintext)
			check.NoErr(err)
			check.NoErr(writer.Close())
			payload := append([]byte{}, encrypted.Bytes()...)
			digest := sha256.Sum256(payload)
			kind := "openbao"
			if test.name == "sqlite" {
				kind = "sqlite"
			}
			artifact := managementbackup.Artifact{Version: 1, Kind: kind, ID: strings.Repeat("a", 32), Recipient: identity.Recipient().String(), CapturedAt: time.Now().UTC(), Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
			if strings.HasPrefix(test.name, "ciphertext") {
				payload[len(payload)-1] ^= 1
				if test.name == "ciphertext authentication" {
					digest = sha256.Sum256(payload)
					artifact.SHA256 = hex.EncodeToString(digest[:])
				}
			}
			marker, err := json.Marshal(artifact)
			check.NoErr(err)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				check.Equal(request.Method, http.MethodGet)
				if request.URL.Query().Get("list-type") == "2" {
					_, _ = fmt.Fprintf(writer, "<ListBucketResult><Name>recovery</Name><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size><ETag>marker</ETag></Contents><Contents><Key>%s</Key><Size>%d</Size><ETag>payload</ETag></Contents></ListBucketResult>", managementbackup.CompletionKey(artifact), len(marker), managementbackup.PayloadKey(artifact), len(payload))
					return
				}
				body, etag := marker, "marker"
				if request.URL.Path == "/recovery/"+managementbackup.PayloadKey(artifact) {
					body, etag = payload, "payload"
				} else {
					check.Equal(request.URL.Path, "/recovery/"+managementbackup.CompletionKey(artifact))
				}
				check.Equal(request.Header.Get("If-Match"), etag)
				writer.Header().Set("ETag", etag)
				writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = writer.Write(body)
			}))
			defer server.Close()
			directory := t.TempDir()
			configFile, keysFile, outputFile := filepath.Join(directory, "recovery.json"), filepath.Join(directory, "keys.json"), filepath.Join(directory, "native")
			configuration, err := json.Marshal(offsite.RecoveryConfig{Local: offsite.StorageConfig{Endpoint: server.URL, Region: "us-east-1", Bucket: "recovery", PathStyle: true, AccessKey: "access", SecretKey: "private-storage-secret"}})
			check.NoErr(err)
			check.NoErr(os.WriteFile(configFile, configuration, 0600))
			key := sealKey
			if test.name == "wrong key" {
				key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
			}
			if test.name == "missing key" {
				key = ""
			}
			keys, err := json.Marshal(map[string]any{"borq": map[string]string{"adminToken": strings.Repeat("b", 64)}, "openbao": managementbackup.OpenBaoRecovery{OriginalToken: strings.Repeat("c", 64), SealKey: key}})
			check.NoErr(err)
			check.NoErr(os.WriteFile(keysFile, keys, 0600))
			if test.name == "private keys" {
				check.NoErr(os.Chmod(keysFile, 0644))
			}
			if test.name == "existing" {
				check.NoErr(os.WriteFile(outputFile, []byte("original"), 0600))
			}
			if test.name == "symlink" {
				check.NoErr(os.Symlink(filepath.Join(directory, "absent-target"), outputFile))
			}
			selection := artifact.ID
			if test.name == "invalid selection" {
				selection = "invalid"
			}
			command := newManagementBackupsCommand()
			command.SilenceUsage, command.SilenceErrors = true, true
			command.SetArgs([]string{"local", "decrypt", kind, selection, "--config", configFile, "--keys-file", keysFile, "--output", outputFile})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			ctx := t.Context()
			if test.name == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err = command.ExecuteContext(ctx)
			if test.problem == "" {
				check.NoErr(err)
				actual, err := os.ReadFile(outputFile)
				check.NoErr(err)
				check.Equal(actual, plaintext)
				info, err := os.Stat(outputFile)
				check.NoErr(err)
				check.Equal(info.Mode().Perm(), os.FileMode(0600))
			} else {
				check.True(err != nil && strings.Contains(err.Error(), test.problem))
				check.True(!strings.Contains(err.Error(), key) || key == "")
				check.Equal(output.Len(), 0)
				if test.name == "existing" {
					actual, err := os.ReadFile(outputFile)
					check.NoErr(err)
					check.Equal(string(actual), "original")
				} else if test.name == "symlink" {
					_, err := os.Readlink(outputFile)
					check.NoErr(err)
				} else {
					_, err := os.Lstat(outputFile)
					check.True(os.IsNotExist(err))
				}
			}
			check.True(!strings.Contains(output.String(), sealKey) && !strings.Contains(output.String(), identity.String()))
			remaining, err := filepath.Glob(filepath.Join(directory, ".borq-native-decrypt-*"))
			check.NoErr(err)
			check.Equal(len(remaining), 0)
		})
	}
}
