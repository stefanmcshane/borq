//go:build integration

package platform_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

type offsiteKms struct {
	client   *openbao.Client
	token    string
	original openbao.InitResult
	address  string
}

// prepareOffsiteKms uses real isolated Raft storage and Transit keys for each provider.
// HTTP is confined to this acceptance fixture; production KMS TLS has a separate native test.
func prepareOffsiteKms(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, kubeconfig, namespace, name, access, secret string) offsiteKms {
	t.Helper()
	assert := is.New(t)
	configuration := `disable_mlock = true
api_addr = "http://127.0.0.1:8200"
cluster_addr = "https://127.0.0.1:8201"
listener "tcp" {
 address = "0.0.0.0:8200"
 tls_disable = 1
 cluster_address = "0.0.0.0:8201"
}
storage "raft" {
 path = "/bao-data"
 node_id = "kms"
}
`
	_, err := kube.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: map[string]string{"bao.hcl": configuration}}, metav1.CreateOptions{})
	assert.NoErr(err)
	root := int64(0)
	_, err = kube.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"offsite-kms": name}}, Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: &root}, Containers: []corev1.Container{{Name: "bao", Image: "openbao/openbao-ubi:2.7.2", Args: []string{"server", "-config=/kms/bao.hcl"}, VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/kms", ReadOnly: true}, {Name: "data", MountPath: "/bao-data"}}}}, Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}, {Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().Services(namespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.ServiceSpec{Selector: map[string]string{"offsite-kms": name}, Ports: []corev1.ServicePort{{Name: "http", Port: 8200}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	for {
		pod, err := kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
		if pod.Status.Phase == corev1.PodRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("isolated KMS Vault did not start")
		case <-time.After(time.Second):
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	stop := startOffsiteForward(t, ctx, kubeconfig, namespace, name, port, 8200)
	t.Cleanup(stop)
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	fresh, err := openbao.NewClient(openbao.NewClientInput{Address: address})
	assert.NoErr(err)
	if err := fresh.WaitForReady(ctx, 100*time.Millisecond, time.Minute); err != nil {
		_, statusError := fresh.Status(ctx)
		logs, _ := kube.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
		t.Fatalf("isolated KMS startup failed: %v; status: %v\n%s", err, statusError, logs)
	}
	initialized, err := fresh.Init(ctx)
	assert.NoErr(err)
	for _, key := range initialized.UnsealKeysB64 {
		assert.NoErr(fresh.Unseal(ctx, key))
	}
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: address, Token: initialized.RootToken})
	assert.NoErr(err)
	assert.NoErr(vault.WaitForActive(ctx, 100*time.Millisecond, time.Minute))
	assert.NoErr(vault.EnsureKvV2(ctx, "secret"))
	ready, cancelReady := context.WithTimeout(ctx, 30*time.Second)
	defer cancelReady()
	for {
		_, err = vault.WriteKv(ready, "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": access, "RUSTFS_SECRET_KEY": secret}})
		if !errors.Is(err, openbao.ErrUpgrading) {
			break
		}
		select {
		case <-ready.Done():
			t.Fatal("isolated KMS Vault did not finish initializing KV v2")
		case <-time.After(100 * time.Millisecond):
		}
	}
	assert.NoErr(err)
	assert.NoErr(platform.ReconcileRustfsEncryption(ctx, vault))
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	assert.NoErr(err)
	return offsiteKms{client: vault, token: entry.Values["RUSTFS_KMS_VAULT_TOKEN"], original: *initialized, address: address}
}

func configureOffsiteKms(values map[string]any, namespace, name string) {
	environment := []any{}
	for _, raw := range values["extraEnv"].([]any) {
		value := raw.(map[string]any)
		if value["name"] == "RUSTFS_KMS_VAULT_CA_CERT" {
			continue
		}
		if value["name"] == "RUSTFS_KMS_VAULT_ADDRESS" {
			value["value"] = "http://" + name + "." + namespace + ".svc:8200"
		}
		environment = append(environment, value)
	}
	environment = append(environment, map[string]any{"name": "RUSTFS_KMS_ALLOW_INSECURE_DEV_DEFAULTS", "value": "true"})
	values["extraEnv"], values["extraVolumes"], values["extraVolumeMounts"] = environment, []any{}, []any{}
}

func captureOffsiteCheckpoint(t *testing.T, ctx context.Context, vault *openbao.Client, recipient string) offsite.VaultCheckpoint {
	t.Helper()
	assert := is.New(t)
	if recipient == "" {
		identity, err := age.GenerateX25519Identity()
		assert.NoErr(err)
		recipient = identity.Recipient().String()
	}
	directory := t.TempDir()
	file := filepath.Join(directory, "raft")
	output, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	assert.NoErr(err)
	capturedAt := time.Now().UTC()
	assert.NoErr(vault.SaveRaftSnapshot(ctx, output))
	assert.NoErr(output.Close())
	id, err := managementbackup.NewID()
	assert.NoErr(err)
	encrypted := filepath.Join(directory, "vault.age")
	artifact, err := managementbackup.Encrypt(ctx, managementbackup.Artifact{Version: 1, Kind: "openbao", ID: id, Recipient: recipient, CapturedAt: capturedAt}, file, encrypted)
	assert.NoErr(err)
	return offsite.VaultCheckpoint{Artifact: artifact, File: encrypted}
}

func (fixture offsiteKms) seal(t *testing.T, ctx context.Context) {
	t.Helper()
	assert := is.New(t)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, fixture.address+"/v1/sys/seal", nil)
	assert.NoErr(err)
	request.Header.Set("X-Vault-Token", fixture.original.RootToken)
	response, err := http.DefaultClient.Do(request)
	assert.NoErr(err)
	assert.NoErr(response.Body.Close())
	assert.Equal(response.StatusCode, http.StatusNoContent)
}

// extractOffsiteCheckpoint proves the documented two-pass Restic/tar path needs
// neither an S3 recovery destination nor a running provider encryption service.
func extractOffsiteCheckpoint(t *testing.T, ctx context.Context, repository offsite.Repository, snapshot string, expected managementbackup.Artifact) {
	t.Helper()
	assert := is.New(t)
	directory := t.TempDir()
	dump := func() *exec.Cmd {
		command := exec.CommandContext(ctx, "restic", "dump", snapshot, "/objects.tar")
		command.Env = []string{"RESTIC_REPOSITORY=" + repository.URL, "RESTIC_PASSWORD=" + repository.Password, "AWS_ACCESS_KEY_ID=" + repository.AccessKey, "AWS_SECRET_ACCESS_KEY=" + repository.SecretKey, "AWS_DEFAULT_REGION=" + repository.Region, "AWS_EC2_METADATA_DISABLED=true"}
		return command
	}
	manifestFile := filepath.Join(directory, "manifest.json")
	assert.NoErr(extractOffsiteMembers(ctx, dump(), manifestFile, "-xOf", "-", "borq-manifest.json"))
	data, err := os.ReadFile(manifestFile)
	assert.NoErr(err)
	var manifest offsite.Archive
	assert.NoErr(json.Unmarshal(data, &manifest))
	assert.Equal(manifest.Version, 4)
	assert.Equal(manifest.VaultCheckpoint, expected)
	// Only fixed canonical paths derived from a validated native artifact are passed to tar.
	marker, err := json.Marshal(manifest.VaultCheckpoint)
	assert.NoErr(err)
	_, err = managementbackup.ParseCompletion(managementbackup.CompletionKey(manifest.VaultCheckpoint), marker)
	assert.NoErr(err)
	assert.NoErr(extractOffsiteMembers(ctx, dump(), "", "-xf", "-", "-C", directory, "--no-same-owner", "--no-same-permissions", managementbackup.PayloadKey(expected), managementbackup.CompletionKey(expected)))
	file, err := os.Open(filepath.Join(directory, managementbackup.PayloadKey(expected)))
	assert.NoErr(err)
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(file, managementbackup.MaxPayloadSize+1))
	assert.NoErr(err)
	assert.Equal(size, expected.Size)
	assert.Equal(hex.EncodeToString(digest.Sum(nil)), expected.SHA256)
	data, err = os.ReadFile(filepath.Join(directory, managementbackup.CompletionKey(expected)))
	assert.NoErr(err)
	actual, err := managementbackup.ParseCompletion(managementbackup.CompletionKey(expected), data)
	assert.NoErr(err)
	assert.Equal(actual, expected)
	info, err := file.Stat()
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
}

func extractOffsiteMembers(ctx context.Context, producer *exec.Cmd, outputFile string, arguments ...string) error {
	stream, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	consumer := exec.CommandContext(ctx, "tar", arguments...)
	consumer.Stdin = stream
	var output *os.File
	if outputFile != "" {
		output, err = os.OpenFile(outputFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer output.Close()
		consumer.Stdout = output
	}
	if err := producer.Start(); err != nil {
		return err
	}
	consumerErr := consumer.Run()
	if consumerErr != nil {
		_ = producer.Process.Kill()
	}
	producerErr := producer.Wait()
	return errors.Join(consumerErr, producerErr)
}

// TestOffsiteCheckpointFailedProducer rejects a successful exact-member extraction
// when its producer fails after a complete tar stream, matching the recovery procedure.
func TestOffsiteCheckpointFailedProducer(t *testing.T) {
	assert := is.New(t)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	assert.NoErr(writer.WriteHeader(&tar.Header{Name: "borq-manifest.json", Mode: 0600, Size: 2}))
	_, err := writer.Write([]byte("{}"))
	assert.NoErr(err)
	assert.NoErr(writer.Close())
	file := filepath.Join(t.TempDir(), "objects.tar")
	assert.NoErr(os.WriteFile(file, archive.Bytes(), 0600))
	producer := exec.CommandContext(t.Context(), "/bin/sh", "-c", `/bin/cat "$1"; exit 17`, "producer", file)
	err = extractOffsiteMembers(t.Context(), producer, filepath.Join(t.TempDir(), "manifest.json"), "-xOf", "-", "borq-manifest.json")
	assert.True(err != nil && strings.Contains(err.Error(), "exit status 17"))
}
