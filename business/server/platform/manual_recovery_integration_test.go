//go:build integration

package platform_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/cli/management"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestManualStaticSealRecovery proves a representative manual disaster recovery:
// source Kubernetes, OpenBao, SQLite files and S3 are destroyed before recovery.
// Only the exported combined keys, retained ciphertext and independent S3 access survive.
// It deliberately uses native manual Raft restore rather than the deferred bootstrap command.
func TestManualStaticSealRecovery(t *testing.T) {
	check := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	defer cancel()
	binary := os.Getenv("BORQ_MANUAL_RECOVERY_BINARY")
	if binary == "" {
		t.Skip("BORQ_MANUAL_RECOVERY_BINARY is required; use make test-manual-recovery")
	}
	random := make([]byte, 32)
	_, err := rand.Read(random)
	check.NoErr(err)
	name := "borq-manual-dr-" + hex.EncodeToString(random[:5])
	root := hex.EncodeToString(random)
	_, err = rand.Read(random)
	check.NoErr(err)
	sealKey := base64.StdEncoding.EncodeToString(random)
	archive, err := sqliteworkload.NewArchiveKey()
	check.NoErr(err)
	private := t.TempDir()
	source := filepath.Join(private, "source")
	saved := filepath.Join(private, "saved")
	check.NoErr(os.Mkdir(source, 0755))
	check.NoErr(os.Mkdir(saved, 0700))
	redact := strings.NewReplacer(root, "[root]", sealKey, "[seal]", archive.Identity, "[archive]")
	run := func(command string, arguments ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, command, arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("manual recovery fixture %s failed: %v: %s", command, err, redact.Replace(string(output)))
		}
		return strings.TrimSpace(string(output))
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-fv", name+"-source-vault", name+"-recovered-vault", name+"-source-s3", name+"-recovered-s3", name+"-replicate").Run()
		_ = exec.CommandContext(cleanup, "docker", "network", "rm", name).Run()
		_ = exec.CommandContext(cleanup, "kind", "delete", "cluster", "--name", name).Run()
	})
	run("docker", "network", "create", name)
	kubeconfig := filepath.Join(source, "kubeconfig")
	run("kind", "create", "cluster", "--name", name, "--kubeconfig", kubeconfig, "--wait", "60s")
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	check.NoErr(err)
	kubeConfiguration := connection.RestConfig()
	kube, err := kubeclient.NewForConfig(&kubeConfiguration)
	check.NoErr(err)
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "borq-mgmt"}}, metav1.CreateOptions{})
	check.NoErr(err)
	for secretName, values := range map[string]map[string]string{
		"borq-admin-auth":    {"token": root},
		"borq-openbao-admin": {"token": root, "sealKey": sealKey},
	} {
		_, err = kube.CoreV1().Secrets("borq-mgmt").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName}, StringData: values}, metav1.CreateOptions{})
		check.NoErr(err)
	}
	keysFile, err := management.ExportKeys(ctx, management.ExportKeysInput{KubeconfigPath: kubeconfig, OutputFile: filepath.Join(saved, "borq-management-recovery.json"), Timeout: 10 * time.Second})
	check.NoErr(err)
	keysInfo, err := os.Stat(keysFile)
	check.NoErr(err)
	check.Equal(keysInfo.Mode().Perm(), os.FileMode(0600))
	vault := startManualRecoveryVault(t, ctx, name, "source", source, root, sealKey, run)
	check.NoErr(vault.EnsureKvV2(ctx, "secret"))
	for {
		_, err = vault.WriteKv(ctx, "secret", "borq/sqlite-archives/representative/app", openbao.WriteKvInput{Values: map[string]string{"ageIdentity": archive.Identity, "ageRecipient": archive.Recipient}, CheckVersion: 0})
		if !errors.Is(err, openbao.ErrUpgrading) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	check.NoErr(err)
	store := startManualRecoveryStorage(t, ctx, name, "source", run)
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(source, "borq.sqlite")})
	check.NoErr(err)
	organization, err := manager.CreateOrganization(ctx, "manual-dr-proof")
	check.NoErr(err)
	check.NoErr(manager.Close())
	database, err := sqlite.New(ctx, filepath.Join(source, "borq.sqlite"))
	check.NoErr(err)
	check.NoErr(database.Snapshot(ctx, filepath.Join(source, "snapshot.sqlite")))
	check.NoErr(database.Close())
	check.NoErr(managementbackup.BundleSQLite(ctx, filepath.Join(source, "snapshot.sqlite"), []managementbackup.Connection{}, filepath.Join(source, "sqlite.bundle")))
	identity, err := managementbackup.RecoveryIdentity(sealKey)
	check.NoErr(err)
	for _, kind := range []string{"sqlite", "openbao"} {
		plaintext := filepath.Join(source, "sqlite.bundle")
		if kind == "openbao" {
			plaintext = filepath.Join(source, "vault.raft")
			file, err := os.OpenFile(plaintext, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			check.NoErr(err)
			check.NoErr(vault.SaveRaftSnapshot(ctx, file))
			check.NoErr(file.Close())
		}
		id, err := managementbackup.NewID()
		check.NoErr(err)
		artifact, err := managementbackup.Encrypt(ctx, managementbackup.Artifact{Version: 1, Kind: kind, ID: id, CapturedAt: time.Now().UTC(), Recipient: identity.Recipient().String()}, plaintext, filepath.Join(saved, kind+".age"))
		check.NoErr(err)
		marker, err := json.Marshal(artifact)
		check.NoErr(err)
		check.NoErr(os.WriteFile(filepath.Join(saved, kind+".json"), marker, 0600))
	}
	app, err := sql.Open("sqlite", filepath.Join(source, "app.sqlite"))
	check.NoErr(err)
	app.SetMaxOpenConns(1)
	_, err = app.ExecContext(ctx, "PRAGMA journal_mode=WAL; CREATE TABLE proof(value TEXT); INSERT INTO proof VALUES ('manual-app-snapshot')")
	check.NoErr(err)
	configuration := sqliteworkload.Config("/data/app.sqlite", sqliteworkload.Destination{Endpoint: "http://" + name + "-source-s3:9000", Region: "us-east-1", Bucket: "manual-dr", Prefix: "app", PathStyle: true, SyncInterval: "100ms"})
	config, err := json.Marshal(configuration)
	check.NoErr(err)
	check.NoErr(os.WriteFile(filepath.Join(source, "litestream.yml"), config, 0644))
	writeManualArchiveEnvironment(t, source, archive)
	run("docker", "run", "-d", "--name", name+"-replicate", "--network", name, "--env-file", filepath.Join(source, "archive.env"), "-v", source+":/data", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), sqliteworkload.Image, "replicate", "-config", "/data/litestream.yml")
	for {
		objects, err := store.List(ctx, "app/")
		check.NoErr(err)
		if len(objects) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("app replication timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
	_, err = app.ExecContext(ctx, "INSERT INTO proof VALUES ('manual-app-wal')")
	check.NoErr(err)
	time.Sleep(time.Second)
	run("docker", "stop", "--time", "20", name+"-replicate")
	check.NoErr(app.Close())
	objects, err := store.List(ctx, "app/")
	check.NoErr(err)
	check.True(len(objects) >= 2)
	for index, object := range objects {
		body, err := store.Read(ctx, object.Key)
		check.NoErr(err)
		check.True(bytes.HasPrefix(body, []byte("age-encryption.org/v1\n")))
		check.NoErr(os.WriteFile(filepath.Join(saved, fmt.Sprintf("object-%d", index)), body, 0600))
	}
	// Destroy source services and their data, including Kubernetes-held keys and the plaintext app/key files.
	run("docker", "rm", "-fv", name+"-replicate", name+"-source-vault", name+"-source-s3")
	run("kind", "delete", "cluster", "--name", name)
	probe, stopProbe := context.WithTimeout(ctx, time.Second)
	_, err = kube.CoreV1().Secrets("borq-mgmt").Get(probe, "borq-openbao-admin", metav1.GetOptions{})
	stopProbe()
	check.True(err != nil)
	check.NoErr(os.RemoveAll(source))
	t.Log("source kind cluster, Vault, storage, writer and plaintext files removed; beginning recovery from saved artifacts")
	archive, root, sealKey = sqliteworkload.ArchiveKey{}, "", ""
	var recovered struct {
		Borq struct {
			AdminToken string `json:"adminToken"`
		} `json:"borq"`
		OpenBao managementbackup.OpenBaoRecovery `json:"openbao"`
	}
	check.NoErr(offsite.ReadPrivateConfig(keysFile, &recovered))
	check.True(recovered.Borq.AdminToken != "")
	recovery := filepath.Join(private, "recovery")
	check.NoErr(os.Mkdir(recovery, 0755))
	destination := startManualRecoveryStorage(t, ctx, name, "recovered", run)
	configurationFile := filepath.Join(saved, "recovery.json")
	storageConfig := offsite.StorageConfig{Endpoint: "http://" + run("docker", "port", name+"-recovered-s3", "9000/tcp"), Region: "us-east-1", Bucket: "manual-dr", PathStyle: true, AccessKey: "manual-independent-access", SecretKey: "manual-independent-storage-secret"}
	recoveryConfig, err := json.Marshal(offsite.RecoveryConfig{Local: storageConfig})
	check.NoErr(err)
	check.NoErr(os.WriteFile(configurationFile, recoveryConfig, 0600))
	restoredVault := startManualRecoveryVault(t, ctx, name, "recovered", recovery, recovered.OpenBao.OriginalToken, recovered.OpenBao.SealKey, run)
	for _, kind := range []string{"openbao", "sqlite"} {
		marker, err := os.ReadFile(filepath.Join(saved, kind+".json"))
		check.NoErr(err)
		var artifact managementbackup.Artifact
		check.NoErr(json.Unmarshal(marker, &artifact))
		_, err = managementbackup.ParseCompletion(managementbackup.CompletionKey(artifact), marker)
		check.NoErr(err)
		ciphertext, err := os.ReadFile(filepath.Join(saved, kind+".age"))
		check.NoErr(err)
		digest := sha256.Sum256(ciphertext)
		check.Equal(artifact.Size, int64(len(ciphertext)))
		check.Equal(artifact.SHA256, hex.EncodeToString(digest[:]))
		check.NoErr(destination.Put(ctx, managementbackup.PayloadKey(artifact), ciphertext))
		check.NoErr(destination.Put(ctx, managementbackup.CompletionKey(artifact), marker))
		run(binary, "management", "backups", "local", "decrypt", kind, artifact.ID, "--config", configurationFile, "--keys-file", keysFile, "--output", filepath.Join(recovery, kind+".native"))
	}
	run("docker", "exec", name+"-recovered-vault", "/bin/sh", "-ec", `export BAO_ADDR=http://127.0.0.1:8200; export BAO_TOKEN="$(cat /openbao/borq-bootstrap/token)"; bao operator raft snapshot restore /openbao/borq-bootstrap/openbao.native`)
	check.NoErr(restoredVault.WaitForActive(ctx, 100*time.Millisecond, 20*time.Second))
	check.NoErr(restoredVault.CheckToken(ctx))
	retained, err := restoredVault.ReadKv(ctx, "secret", "borq/sqlite-archives/representative/app", 1)
	check.NoErr(err)
	restoredArchive := sqliteworkload.ArchiveKey{Identity: retained.Values["ageIdentity"], Recipient: retained.Values["ageRecipient"]}
	check.NoErr(restoredArchive.Validate())
	dataDir := filepath.Join(recovery, "borq")
	check.NoErr(managementbackup.RestoreSQLite(ctx, filepath.Join(recovery, "sqlite.native"), dataDir))
	_, err = apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(dataDir, "borq.sqlite")})
	check.True(err != nil && strings.Contains(err.Error(), "offline review"))
	pending, err := managementbackup.Inspect(ctx, dataDir)
	check.NoErr(err)
	check.Equal(len(pending), 0)
	check.NoErr(managementbackup.Resume(dataDir))
	restoredManager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: filepath.Join(dataDir, "borq.sqlite")})
	check.NoErr(err)
	organizations, err := restoredManager.ListOrganizations(ctx)
	check.NoErr(err)
	check.Equal(len(organizations), 1)
	check.Equal(organizations[0].ID, organization.ID)
	check.NoErr(restoredManager.Close())
	for index, object := range objects {
		body, err := os.ReadFile(filepath.Join(saved, fmt.Sprintf("object-%d", index)))
		check.NoErr(err)
		check.NoErr(destination.Put(ctx, object.Key, body))
	}
	configuration.Databases[0].Replicas[0].Endpoint = "http://" + name + "-recovered-s3:9000"
	config, err = json.Marshal(configuration)
	check.NoErr(err)
	check.NoErr(os.WriteFile(filepath.Join(recovery, "litestream.yml"), config, 0644))
	writeManualArchiveEnvironment(t, recovery, restoredArchive)
	run("docker", "run", "--rm", "--network", name, "--env-file", filepath.Join(recovery, "archive.env"), "-v", recovery+":/data", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), sqliteworkload.Image, "restore", "-config", "/data/litestream.yml", "-o", "/data/restored.sqlite", "/data/app.sqlite")
	restoredApp, err := sql.Open("sqlite", filepath.Join(recovery, "restored.sqlite"))
	check.NoErr(err)
	var values string
	check.NoErr(restoredApp.QueryRowContext(ctx, "SELECT group_concat(value, ',') FROM (SELECT value FROM proof ORDER BY rowid)").Scan(&values))
	check.Equal(values, "manual-app-snapshot,manual-app-wal")
	check.NoErr(restoredApp.Close())
	t.Log("source cluster/services/data destroyed; exported combined keys recovered production static-seal Raft, reopened Borq organization, and decrypted app snapshot+WAL using only the restored vault identity")
}

func startManualRecoveryVault(t *testing.T, ctx context.Context, network, phase, directory, token, sealKey string, run func(string, ...string) string) *openbao.Client {
	t.Helper()
	check := is.New(t)
	check.NoErr(os.WriteFile(filepath.Join(directory, "token"), []byte(token), 0600))
	check.NoErr(os.WriteFile(filepath.Join(directory, "sealKey"), []byte(sealKey), 0600))
	config := `disable_mlock=true
api_addr="http://127.0.0.1:8200"
cluster_addr="https://127.0.0.1:8201"
listener "tcp" {
 address="0.0.0.0:8200"
 tls_disable=1
}
storage "raft" {
 path="/bao-data"
 node_id="manual"
}
seal "static" {
 current_key_id="borq-v1"
 current_key="file:///openbao/borq-bootstrap/sealKey"
}
initialize "borq" {
 request "administrator" {
  operation="update"
  path="auth/token/create-orphan"
  data={ id={ eval_source="file", eval_type="string", path="/openbao/borq-bootstrap/token" }, policies=["root"], no_default_policy=true, ttl="0" }
 }
}
`
	check.NoErr(os.WriteFile(filepath.Join(directory, "bao.hcl"), []byte(config), 0600))
	name := network + "-" + phase + "-vault"
	run("docker", "run", "-d", "--user", "0", "--entrypoint", "bao", "--name", name, "--network", network, "-p", "127.0.0.1::8200", "-v", directory+":/openbao/borq-bootstrap:ro", "-v", "/bao-data", "quay.io/openbao/openbao:2.7.1", "server", "-config=/openbao/borq-bootstrap/bao.hcl")
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: "http://" + run("docker", "port", name, "8200/tcp"), Token: token, RequestTimeout: 20 * time.Second})
	check.NoErr(err)
	if err := vault.WaitForReady(ctx, 100*time.Millisecond, 20*time.Second); err != nil {
		t.Fatalf("manual static-seal vault startup failed: %v: %s", err, run("docker", "logs", "--tail", "30", name))
	}
	check.NoErr(vault.WaitForActive(ctx, 100*time.Millisecond, 20*time.Second))
	return vault
}

func startManualRecoveryStorage(t *testing.T, ctx context.Context, network, phase string, run func(string, ...string) string) objectstore.Client {
	t.Helper()
	check := is.New(t)
	name := network + "-" + phase + "-s3"
	run("docker", "run", "-d", "--name", name, "--network", network, "-p", "127.0.0.1::9000", "-e", "RUSTFS_ACCESS_KEY=manual-independent-access", "-e", "RUSTFS_SECRET_KEY=manual-independent-storage-secret", "-e", "RUSTFS_CONSOLE_ENABLE=false", "rustfs/rustfs:1.0.1", "/data")
	client, err := objectstore.New(objectstore.Config{Endpoint: "http://" + run("docker", "port", name, "9000/tcp"), Region: "us-east-1", Bucket: "manual-dr", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "manual-independent-access", SecretKey: "manual-independent-storage-secret"}})
	check.NoErr(err)
	for {
		if client.EnsureBucket(ctx) == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("manual recovery storage did not start")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return client
}

func writeManualArchiveEnvironment(t *testing.T, directory string, archive sqliteworkload.ArchiveKey) {
	t.Helper()
	contents := "AWS_ACCESS_KEY_ID=manual-independent-access\nAWS_SECRET_ACCESS_KEY=manual-independent-storage-secret\nAWS_EC2_METADATA_DISABLED=true\nLITESTREAM_AGE_IDENTITY=" + archive.Identity + "\nLITESTREAM_AGE_RECIPIENT=" + archive.Recipient + "\n"
	is.New(t).NoErr(os.WriteFile(filepath.Join(directory, "archive.env"), []byte(contents), 0600))
}
