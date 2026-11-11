//go:build integration

package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/postgres"
)

// TestRustfsBackupEncryption exercises released RustFS AES256 with retained OpenBao Transit keys and fail-closed writes and reads.
// Its disposable containers never connect to an existing management installation.
func TestRustfsBackupEncryption(t *testing.T) {
	check := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	identity, err := randomCredential()
	check.NoErr(err)
	identity = "borq-kms-" + identity[:12]
	root, err := randomCredential()
	check.NoErr(err)
	access, err := randomCredential()
	check.NoErr(err)
	secret, err := randomCredential()
	check.NoErr(err)
	docker := func(arguments ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("disposable encryption container operation failed: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	docker("network", "create", identity)
	docker("volume", "create", identity+"-data")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-fv", identity+"-rustfs", identity+"-recovery", identity+"-bao").Run()
		_ = exec.CommandContext(cleanup, "docker", "network", "rm", identity).Run()
		_ = exec.CommandContext(cleanup, "docker", "volume", "rm", identity+"-data").Run()
	})
	transport, err := newVaultKmsTlsSecret("test")
	check.NoErr(err)
	certificates := t.TempDir()
	check.NoErr(os.Chmod(certificates, 0755))
	for name, value := range transport.Data {
		check.NoErr(os.WriteFile(filepath.Join(certificates, name), []byte(value), 0644))
	}
	configuration := `disable_mlock = true
api_addr = "http://127.0.0.1:8200"
cluster_addr = "https://127.0.0.1:8201"
listener "tcp" {
  address = "0.0.0.0:8200"
  tls_disable = 1
  cluster_address = "0.0.0.0:8201"
}
listener "tcp" {
  address = "0.0.0.0:8202"
  tls_cert_file = "/kms/tls.crt"
  tls_key_file = "/kms/tls.key"
  tls_auto_reload = true
  tls_auto_reload_interval = "5s"
  cluster_address = "0.0.0.0:8203"
}
storage "raft" {
  path = "/bao-data"
  node_id = "bao"
}
`
	sealKey := make([]byte, 32)
	_, err = rand.Read(sealKey)
	check.NoErr(err)
	check.NoErr(os.WriteFile(filepath.Join(certificates, "sealKey"), []byte(base64.StdEncoding.EncodeToString(sealKey)), 0644))
	check.NoErr(os.WriteFile(filepath.Join(certificates, "token"), []byte(root), 0644))
	configuration += vaultBootstrapConfig
	check.NoErr(os.WriteFile(filepath.Join(certificates, "bao.hcl"), []byte(configuration), 0644))
	docker("run", "-d", "--user", "0", "--entrypoint", "bao", "--name", identity+"-bao", "--network", identity, "--network-alias", "openbao", "-p", "127.0.0.1::8200", "-p", "127.0.0.1::8202", "-v", certificates+":/kms:ro", "-v", certificates+":/openbao/borq-bootstrap:ro", "-v", "/bao-data", "quay.io/openbao/openbao:2.7.1", "server", "-config=/kms/bao.hcl")
	baoAddress := "http://" + docker("port", identity+"-bao", "8200/tcp")
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: baoAddress, Token: root, RequestTimeout: 30 * time.Second})
	check.NoErr(err)
	if err := vault.WaitForReady(ctx, 100*time.Millisecond, 20*time.Second); err != nil {
		t.Fatalf("disposable OpenBao startup failed: %v\n%s", err, docker("logs", identity+"-bao"))
	}
	check.NoErr(vault.WaitForActive(ctx, 100*time.Millisecond, 20*time.Second))
	check.NoErr(vault.EnsureKvV2(ctx, "secret"))
	kvReady, stopKvReady := context.WithTimeout(ctx, 10*time.Second)
	defer stopKvReady()
	for {
		_, err = vault.ReadKv(kvReady, "secret", "borq/platform/rustfs", 0)
		if !errors.Is(err, openbao.ErrUpgrading) {
			break
		}
		select {
		case <-kvReady.Done():
			t.Fatal("disposable OpenBao KV initialization timed out")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !errors.Is(err, openbao.ErrNotFound) {
		check.NoErr(err)
	}
	_, err = vault.WriteKv(ctx, "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": access, "RUSTFS_SECRET_KEY": secret}})
	check.NoErr(err)
	check.NoErr(ReconcileRustfsEncryption(ctx, vault))
	check.NoErr(CheckRetainedRustfsEncryptionRoot(ctx, vault))
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	check.NoErr(err)
	check.NoErr(ReconcileRustfsEncryption(ctx, vault))
	retained, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	check.NoErr(err)
	check.Equal(retained.Version, entry.Version)
	arguments := []string{"run", "-d", "--name", identity + "-rustfs", "--network", identity, "-v", identity + "-data:/data", "-v", certificates + ":/kms:ro", "-p", "127.0.0.1::9000", "-e", "RUSTFS_ACCESS_KEY=" + access, "-e", "RUSTFS_SECRET_KEY=" + secret,
		"-e", "RUSTFS_KMS_ENABLE=true", "-e", "RUSTFS_KMS_BACKEND=vault-transit", "-e", "RUSTFS_KMS_VAULT_ADDRESS=https://openbao:8202", "-e", "RUSTFS_KMS_VAULT_CA_CERT=/kms/ca.crt", "-e", "RUSTFS_KMS_VAULT_TOKEN=" + entry.Values["RUSTFS_KMS_VAULT_TOKEN"],
		"-e", "RUSTFS_KMS_VAULT_MOUNT_PATH=borq-rustfs-kms", "-e", "RUSTFS_KMS_VAULT_TRANSIT_METADATA_KV_MOUNT=secret", "-e", "RUSTFS_KMS_VAULT_TRANSIT_METADATA_PREFIX=borq/platform/rustfs-kms/metadata", "-e", "RUSTFS_KMS_DEFAULT_KEY_ID=borq-managed-backups-v1", "-e", "RUSTFS_KMS_ENABLE_CACHE=false", "-e", "RUSTFS_CONSOLE_ENABLE=false", "rustfs/rustfs:1.0.1", "/data"}
	docker(arguments...)
	s3Address := "http://" + docker("port", identity+"-rustfs", "9000/tcp")
	sdk := s3.New(s3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""), BaseEndpoint: aws.String(s3Address), UsePathStyle: true, RetryMaxAttempts: 1})
	ready, stopReady := context.WithTimeout(ctx, 30*time.Second)
	defer stopReady()
	for {
		_, err = sdk.CreateBucket(ready, &s3.CreateBucketInput{Bucket: aws.String("backups")})
		if err == nil {
			break
		}
		select {
		case <-ready.Done():
			t.Fatalf("disposable RustFS did not become ready: %v\n%s", err, docker("logs", identity+"-rustfs"))
		case <-time.After(200 * time.Millisecond):
		}
	}
	config := objectstore.Config{Endpoint: s3Address, Region: "us-east-1", Bucket: "backups", PathStyle: true, ServerSideEncryption: "AES256", Credentials: objectstore.Credentials{AccessKey: access, SecretKey: secret}}
	store, err := objectstore.New(config)
	check.NoErr(err)
	refreshRustfs := func() {
		config.Endpoint = "http://" + docker("port", identity+"-rustfs", "9000/tcp")
		store, err = objectstore.New(config)
		check.NoErr(err)
		sdk = s3.New(s3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""), BaseEndpoint: aws.String(config.Endpoint), UsePathStyle: true, RetryMaxAttempts: 1})
	}
	check.NoErr(store.Validate(ctx))
	// The native RustFS worker, not the fixture, must create its reserved health
	// key. The same scoped credential must still be unable to recreate archive keys.
	runtimeVault, err := openbao.NewClient(openbao.NewClientInput{Address: baoAddress, Token: entry.Values["RUSTFS_KMS_VAULT_TOKEN"], RequestTimeout: time.Second})
	check.NoErr(err)
	probeReady, stopProbe := context.WithTimeout(ctx, 15*time.Second)
	defer stopProbe()
	for {
		_, err = runtimeVault.ReadTransitKey(probeReady, "borq-rustfs-kms", "rustfs-internal-kms-probe")
		if err == nil {
			_, err = runtimeVault.ReadKvDocument(probeReady, "secret", "borq/platform/rustfs-kms/metadata/rustfs-internal-kms-probe")
		}
		if err == nil {
			break
		}
		select {
		case <-probeReady.Done():
			t.Fatalf("RustFS health probe could not provision its scoped key: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	probeCiphertext, err := runtimeVault.EncryptTransit(ctx, "borq-rustfs-kms", "rustfs-internal-kms-probe", []byte("health probe"))
	check.NoErr(err)
	probePlaintext, err := runtimeVault.DecryptTransit(ctx, "borq-rustfs-kms", "rustfs-internal-kms-probe", probeCiphertext)
	check.NoErr(err)
	check.Equal(string(probePlaintext), "health probe")
	for _, path := range []string{
		"borq-rustfs-kms/keys/borq-managed-backups-v1",
		"borq-rustfs-kms/keys/unrelated-key",
		"secret/data/borq/platform/rustfs-kms/metadata/borq-managed-backups-v1",
	} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baoAddress+"/v1/"+path, strings.NewReader(`{}`))
		check.NoErr(err)
		request.Header.Set("X-Vault-Token", entry.Values["RUSTFS_KMS_VAULT_TOKEN"])
		response, err := http.DefaultClient.Do(request)
		check.NoErr(err)
		check.Equal(response.StatusCode, http.StatusForbidden)
		check.NoErr(response.Body.Close())
	}
	payload := []byte("PostgreSQL backup payload protected by OpenBao Transit")
	check.NoErr(store.Put(ctx, "base/backup.info", payload))
	read, err := store.Read(ctx, "base/backup.info")
	check.NoErr(err)
	check.Equal(read, payload)
	head, err := sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("backups"), Key: aws.String("base/backup.info")})
	check.NoErr(err)
	check.Equal(string(head.ServerSideEncryption), "AES256")
	check.NoErr(store.PutNew(ctx, "wal/segment", bytes.NewReader(payload), int64(len(payload))))
	read, err = store.Read(ctx, "wal/segment")
	check.NoErr(err)
	check.Equal(read, payload)
	metadata := []byte("status=DONE\nversion=180006\nsize=64\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000001\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-11-05 00:00:00+00:00\ntablespaces=None\n")
	check.NoErr(store.Put(ctx, "server/base/20261105T000000/backup.info", metadata))
	check.NoErr(store.Put(ctx, "server/base/20261105T000000/data.tar.gz", payload))
	check.NoErr(store.Put(ctx, "server/wals/0000000100000000/000000010000000000000001.gz", payload))
	catalogue, err := postgres.Catalogue(ctx, config, "server")
	check.NoErr(err)
	check.Equal(len(catalogue), 1)
	check.True(catalogue[0].Eligible)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baoAddress+"/v1/borq-rustfs-kms/keys/borq-managed-backups-v1", nil)
	check.NoErr(err)
	request.Header.Set("X-Vault-Token", root)
	response, err := http.DefaultClient.Do(request)
	check.NoErr(err)
	check.Equal(response.StatusCode, http.StatusOK)
	body, err := io.ReadAll(response.Body)
	check.NoErr(response.Body.Close())
	check.NoErr(err)
	var key struct {
		Data struct {
			Exportable      bool `json:"exportable"`
			DeletionAllowed bool `json:"deletion_allowed"`
		} `json:"data"`
	}
	check.NoErr(json.Unmarshal(body, &key))
	check.True(!key.Data.Exportable && !key.Data.DeletionAllowed)

	docker("restart", identity+"-rustfs")
	refreshRustfs()
	for attempt := 0; attempt < 20; attempt++ {
		read, err = store.Read(ctx, "base/backup.info")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		upstream, upstreamError := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("backups"), Key: aws.String("base/backup.info")})
		if upstream != nil && upstream.Body != nil {
			_ = upstream.Body.Close()
		}
		t.Fatalf("cold RustFS encrypted read failed: %v; S3 detail: %v\nRustFS:\n%s\nOpenBao:\n%s", err, upstreamError, docker("logs", "--tail", "100", identity+"-rustfs"), docker("logs", "--tail", "100", identity+"-bao"))
	}
	check.Equal(read, payload)
	started := docker("inspect", "--format", "{{.State.StartedAt}}", identity+"-bao")
	replacement, err := newVaultKmsTlsSecret("test")
	check.NoErr(err)
	for name, value := range replacement.Data {
		check.NoErr(os.WriteFile(filepath.Join(certificates, name), []byte(value), 0644))
	}
	trust := x509.NewCertPool()
	check.True(trust.AppendCertsFromPEM([]byte(replacement.Data["ca.crt"])))
	https := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}}, Timeout: time.Second}
	httpsAddress := "https://" + docker("port", identity+"-bao", "8202/tcp")
	for attempt := 0; attempt < 100; attempt++ {
		response, err = https.Get(httpsAddress + "/v1/sys/health")
		if err == nil {
			check.Equal(response.StatusCode, http.StatusOK)
			check.NoErr(response.Body.Close())
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	check.NoErr(err)
	https.CloseIdleConnections()
	check.Equal(docker("inspect", "--format", "{{.State.StartedAt}}", identity+"-bao"), started)
	docker("restart", identity+"-rustfs")
	refreshRustfs()
	for attempt := 0; attempt < 20; attempt++ {
		read, err = store.Read(ctx, "base/backup.info")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	check.NoErr(err)
	check.Equal(read, payload)

	var snapshot bytes.Buffer
	check.NoErr(vault.SaveRaftSnapshot(ctx, &snapshot))
	archiveIdentity, err := managementbackup.RecoveryIdentity(base64.StdEncoding.EncodeToString(sealKey))
	check.NoErr(err)
	var nativeCiphertext bytes.Buffer
	encrypted, err := age.Encrypt(&nativeCiphertext, archiveIdentity.Recipient())
	check.NoErr(err)
	_, err = encrypted.Write(snapshot.Bytes())
	check.NoErr(err)
	check.NoErr(encrypted.Close())
	nativeConfig := config
	nativeConfig.ServerSideEncryption = ""
	nativeStore, err := objectstore.New(nativeConfig)
	check.NoErr(err)
	check.NoErr(nativeStore.Put(ctx, "management/openbao/native/payload", nativeCiphertext.Bytes()))
	nativeHead, err := sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("backups"), Key: aws.String("management/openbao/native/payload")})
	check.NoErr(err)
	check.Equal(string(nativeHead.ServerSideEncryption), "")
	docker("stop", identity+"-bao")
	failed, stopFailed := context.WithTimeout(ctx, 12*time.Second)
	defer stopFailed()
	check.True(store.Put(failed, "base/must-not-exist", payload) != nil)
	failedRead, stopRead := context.WithTimeout(ctx, 12*time.Second)
	defer stopRead()
	_, err = store.Read(failedRead, "base/backup.info")
	check.True(err != nil)
	_, err = sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("backups"), Key: aws.String("base/must-not-exist")})
	check.True(err != nil)

	// The normal server and every backup writer are stopped before the private recovery reader mounts the same data.
	docker("stop", identity+"-rustfs")
	docker("run", "-d", "--name", identity+"-recovery", "--network", identity, "-v", identity+"-data:/data", "-p", "127.0.0.1::9000", "-e", "RUSTFS_ACCESS_KEY="+access, "-e", "RUSTFS_SECRET_KEY="+secret, "-e", "RUSTFS_KMS_ENABLE=false", "-e", "RUSTFS_CONSOLE_ENABLE=false", "rustfs/rustfs:1.0.1", "/data")
	nativeConfig.Endpoint = "http://" + docker("port", identity+"-recovery", "9000/tcp")
	recoveryStore, err := objectstore.New(nativeConfig)
	check.NoErr(err)
	var recoveredCiphertext []byte
	for attempt := 0; attempt < 20; attempt++ {
		recoveredCiphertext, err = recoveryStore.Read(ctx, "management/openbao/native/payload")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		recoveryS3 := s3.New(s3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""), BaseEndpoint: aws.String(nativeConfig.Endpoint), UsePathStyle: true, RetryMaxAttempts: 1})
		result, detail := recoveryS3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("backups"), Key: aws.String("management/openbao/native/payload")})
		if result != nil && result.Body != nil {
			_ = result.Body.Close()
		}
		t.Fatalf("isolated native recovery read failed: %v; S3 detail: %v\n%s", err, detail, docker("logs", "--tail", "100", identity+"-recovery"))
	}
	check.Equal(recoveredCiphertext, nativeCiphertext.Bytes())
	decrypted, err := age.Decrypt(bytes.NewReader(recoveredCiphertext), archiveIdentity)
	check.NoErr(err)
	recoveredSnapshot, err := io.ReadAll(decrypted)
	check.NoErr(err)
	check.Equal(recoveredSnapshot, snapshot.Bytes())
	_, err = recoveryStore.Read(ctx, "base/backup.info")
	check.True(err != nil)
	docker("stop", identity+"-recovery")
	docker("start", identity+"-bao")
	baoAddress = "http://" + docker("port", identity+"-bao", "8200/tcp")
	vault, err = openbao.NewClient(openbao.NewClientInput{Address: baoAddress, Token: root, RequestTimeout: 30 * time.Second})
	check.NoErr(err)
	check.NoErr(vault.WaitForReady(ctx, 100*time.Millisecond, 20*time.Second))
	check.NoErr(vault.WaitForActive(ctx, 100*time.Millisecond, 20*time.Second))
	check.NoErr(ReconcileRustfsEncryption(ctx, vault))
	docker("start", identity+"-rustfs")
	refreshRustfs()
	for attempt := 0; attempt < 20; attempt++ {
		read, err = store.Read(ctx, "base/backup.info")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	read, err = store.Read(ctx, "base/backup.info")
	check.NoErr(err)
	check.Equal(read, payload)
	for _, missing := range []string{"transit-key", "native-metadata"} {
		location := "/v1/sys/mounts/borq-rustfs-kms"
		if missing == "native-metadata" {
			location = "/v1/secret/metadata/borq/platform/rustfs-kms/metadata/borq-managed-backups-v1"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, baoAddress+location, nil)
		check.NoErr(err)
		request.Header.Set("X-Vault-Token", root)
		response, err := http.DefaultClient.Do(request)
		check.NoErr(err)
		check.Equal(response.StatusCode, http.StatusNoContent)
		check.NoErr(response.Body.Close())
		check.True(CheckRetainedRustfsEncryptionRoot(ctx, vault) != nil)
		check.True(ReconcileRustfsEncryption(ctx, vault) != nil)
		docker("restart", identity+"-rustfs")
		refreshRustfs()
		for attempt := 0; attempt < 20; attempt++ {
			_, err = sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("backups")})
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		check.NoErr(err)
		check.True(store.Put(ctx, "base/must-not-rekey", payload) != nil)
		read, err = store.Read(ctx, "base/backup.info")
		if missing == "transit-key" {
			check.True(err != nil)
		} else {
			check.NoErr(err)
			check.Equal(read, payload)
		}
		if missing == "transit-key" {
			_, err = vault.ReadTransitKey(ctx, "borq-rustfs-kms", "borq-managed-backups-v1")
			check.True(errors.Is(err, openbao.ErrNotFound))
		}
		check.NoErr(vault.RestoreRaftSnapshot(ctx, bytes.NewReader(snapshot.Bytes()), false))
		check.NoErr(vault.WaitForReady(ctx, 100*time.Millisecond, 20*time.Second))
		check.NoErr(vault.WaitForActive(ctx, 100*time.Millisecond, 20*time.Second))
		check.NoErr(ReconcileRustfsEncryption(ctx, vault))
		docker("restart", identity+"-rustfs")
		refreshRustfs()
		for attempt := 0; attempt < 20; attempt++ {
			read, err = store.Read(ctx, "base/backup.info")
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		check.NoErr(err)
		check.Equal(read, payload)
	}
	fmt.Println("RustFS verified TLS AES256, live TLS replacement, encrypted native cold recovery without KMS, unavailable KMS, cold key/metadata loss refusal, and original-key snapshot recovery verified")
}
