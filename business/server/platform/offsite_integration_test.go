//go:build integration

package platform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestRustfsOffsiteIntegration proves real PVC survival, pinned Restic commit/failure semantics and offline rehydration.
// BORQ_OFFSITE_SEEDED_POSTGRES also creates a genuine source backup and compares recovered database rows and schema.
func TestRustfsOffsiteIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_KUBECONFIG")
	binary := os.Getenv("BORQ_OFFSITE_TEST_BINARY")
	if kubeconfig == "" || binary == "" {
		t.Skip("BORQ_KUBECONFIG and BORQ_OFFSITE_TEST_BINARY are required")
	}
	assert := is.New(t)
	version, err := exec.Command("restic", "version").Output()
	assert.NoErr(err)
	assert.True(strings.HasPrefix(string(version), "restic 0.18.0 "))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	restConfig := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&restConfig)
	assert.NoErr(err)
	namespace := fmt.Sprintf("borq-offsite-%d", time.Now().UnixNano())
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = client.RbacV1().ClusterRoleBindings().Delete(cleanup, namespace+"-openbao-server-binding", metav1.DeleteOptions{})
	})
	primaryKms := prepareOffsiteKms(t, ctx, client, kubeconfig, namespace, "source-kms", "borq-offsite-access", "borq-offsite-secret-independent-test")
	externalKms := prepareOffsiteKms(t, ctx, client, kubeconfig, namespace, "external-kms", "independent-offsite-access", "independent-offsite-secret")
	_, err = client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-rustfs"}, StringData: map[string]string{"RUSTFS_ACCESS_KEY": "borq-offsite-access", "RUSTFS_SECRET_KEY": "borq-offsite-secret-independent-test", "RUSTFS_KMS_VAULT_TOKEN": primaryKms.token}}, metav1.CreateOptions{})
	assert.NoErr(err)
	settings := platform.Settings{ClusterName: "offsite-source", Profile: "production", Namespace: namespace, StorageClass: offsiteStorageClass(), Timeout: 4 * time.Minute}
	catalog, err := platform.GetComponentCatalog(platform.ComponentCatalogInput{Settings: settings, Scope: platform.ComponentScopeManagement})
	assert.NoErr(err)
	var definition platform.ComponentConfiguration
	for _, candidate := range catalog.Components {
		if candidate.Name == "rustfs" {
			definition = candidate
		}
	}
	values := definition.Values
	configureOffsiteKms(values, namespace, "source-kms")
	values["service"].(map[string]any)["type"] = "ClusterIP"
	values["storageclass"].(map[string]any)["dataStorageSize"] = "1Gi"
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer releases.Close()
	_, err = releases.Reconcile(ctx, helm.InstallOrUpgradeInput{Namespace: namespace, ReleaseName: "rustfs", ChartRef: definition.Chart, Version: definition.Version, Values: values, Timeout: 4 * time.Minute, Owner: namespace})
	assert.NoErr(err)

	_, err = client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-rustfs-offsite"}, StringData: map[string]string{"RUSTFS_ACCESS_KEY": "independent-offsite-access", "RUSTFS_SECRET_KEY": "independent-offsite-secret", "RUSTFS_KMS_VAULT_TOKEN": externalKms.token}}, metav1.CreateOptions{})
	assert.NoErr(err)
	externalCatalog, err := platform.GetComponentCatalog(platform.ComponentCatalogInput{Settings: settings, Scope: platform.ComponentScopeManagement})
	assert.NoErr(err)
	var externalValues map[string]any
	for _, candidate := range externalCatalog.Components {
		if candidate.Name == "rustfs" {
			externalValues = candidate.Values
		}
	}
	configureOffsiteKms(externalValues, namespace, "external-kms")
	externalValues["fullnameOverride"] = "rustfs-offsite"
	externalValues["service"].(map[string]any)["type"] = "ClusterIP"
	externalValues["storageclass"].(map[string]any)["dataStorageSize"] = "1Gi"
	externalValues["secret"].(map[string]any)["existingSecret"] = "borq-rustfs-offsite"
	job := externalValues["extraManifests"].([]any)[0].(map[string]any)
	job["metadata"].(map[string]any)["name"] = "borq-rustfs-offsite-buckets"
	container := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	args := container["args"].([]any)
	args[0] = strings.ReplaceAll(strings.ReplaceAll(args[0].(string), "http://rustfs-svc:9000", "http://rustfs-offsite-svc:9000"), platform.ManagementBucketName(settings.ClusterName), "offsite")
	for _, value := range container["env"].([]any) {
		environment := value.(map[string]any)
		if reference, exists := environment["valueFrom"]; exists {
			reference.(map[string]any)["secretKeyRef"].(map[string]any)["name"] = "borq-rustfs-offsite"
		}
	}
	_, err = releases.Reconcile(ctx, helm.InstallOrUpgradeInput{Namespace: namespace, ReleaseName: "rustfs-offsite", ChartRef: definition.Chart, Version: definition.Version, Values: externalValues, Timeout: 4 * time.Minute, Owner: namespace})
	assert.NoErr(err)
	externalListener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	externalPort := externalListener.Addr().(*net.TCPAddr).Port
	assert.NoErr(externalListener.Close())
	stopExternal := startOffsiteForward(t, ctx, kubeconfig, namespace, "rustfs-offsite-svc", externalPort, 9000)
	defer stopExternal()
	externalEndpoint := fmt.Sprintf("http://127.0.0.1:%d", externalPort)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	stopForward := startOffsiteForward(t, ctx, kubeconfig, namespace, "rustfs-svc", port, 9000)
	source := objectstore.Config{ServerSideEncryption: "AES256", Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Region: "us-east-1", Bucket: platform.ManagementBucketName(settings.ClusterName), Prefix: "source", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "borq-offsite-access", SecretKey: "borq-offsite-secret-independent-test"}}
	storage, err := objectstore.New(source)
	assert.NoErr(err)
	archiveID := strings.Repeat("b", 32)
	server := "organizations/" + strings.Repeat("a", 32) + "/postgres/" + archiveID + "/archive-" + archiveID
	data := map[string][]byte{
		server + "/base/20261024T120000/backup.info":                  []byte("status=DONE\nversion=180006\nsize=1048576\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nsystemid=12345\nxlog_segment_size=16777216\nend_time=2026-10-24 12:00:00+00:00\ntablespaces=None\n"),
		server + "/base/20261024T120000/data.tar.gz":                  bytes.Repeat([]byte("physical-streaming-payload\n"), 400000),
		server + "/wals/0000000100000000/000000010000000000000001.gz": []byte("wal-one"),
		server + "/wals/0000000100000000/000000010000000000000002.gz": []byte("wal-two"),
		server + "/wals/00000002.history":                             []byte("timeline-history"),
	}
	nativeEnabled := os.Getenv("BORQ_OFFSITE_NATIVE") == "true"
	managementOnly := os.Getenv("BORQ_OFFSITE_MANAGEMENT_ONLY") == "true"
	var native nativeAcceptance
	if managementOnly {
		data = map[string][]byte{}
	}
	seeded := os.Getenv("BORQ_OFFSITE_SEEDED_POSTGRES") == "true"
	var recoveryDatabase postgres.Input
	expectedRows := ""
	var postgresClient postgres.Client
	var recoveryTarget time.Time
	if seeded {
		postgresClient, recoveryDatabase, expectedRows, recoveryTarget = seedOffsitePostgres(t, ctx, kubeconfig, namespace, source)
		assertOffsitePostgresEncryption(t, ctx, source)
		data = map[string][]byte{}
		objects, err := storage.List(ctx, "")
		assert.NoErr(err)
		for _, object := range objects {
			body, err := storage.OpenCaptured(ctx, object)
			assert.NoErr(err)
			data[object.Key], err = io.ReadAll(body)
			assert.NoErr(err)
			assert.NoErr(body.Close())
		}
		assert.True(len(data) > 0)
	} else {
		for key, body := range data {
			assert.NoErr(storage.PutNew(ctx, key, bytes.NewReader(body), int64(len(body))))
		}
	}
	if nativeEnabled {
		native = seedNativeManagement(t, ctx, connection, releases, client, kubeconfig, namespace, source)
		data = map[string][]byte{}
		objects, err := storage.List(ctx, "")
		assert.NoErr(err)
		for _, object := range objects {
			body, err := storage.OpenCaptured(ctx, object)
			assert.NoErr(err)
			data[object.Key], err = io.ReadAll(body)
			assert.NoErr(err)
			assert.NoErr(body.Close())
		}
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=rustfs,app.kubernetes.io/instance=rustfs"})
	assert.NoErr(err)
	assert.Equal(len(pods.Items), 1)
	oldPod := pods.Items[0]
	assert.NoErr(client.CoreV1().Pods(namespace).Delete(ctx, oldPod.Name, metav1.DeleteOptions{}))
	stopForward()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pods, err = client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=rustfs,app.kubernetes.io/instance=rustfs"})
		if err == nil && len(pods.Items) == 1 && pods.Items[0].UID != oldPod.UID && podReady(pods.Items[0]) {
			assert.Equal(pods.Items[0].Spec.NodeName, oldPod.Spec.NodeName)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("RustFS replacement pod did not become ready")
		case <-ticker.C:
		}
	}
	stopForward = startOffsiteForward(t, ctx, kubeconfig, namespace, "rustfs-svc", port, 9000)
	defer stopForward()
	t.Log("RustFS replacement pod reopened the same persistent volume on its data node")
	captured, err := storage.List(ctx, "")
	assert.NoErr(err)
	for _, object := range captured {
		body, err := storage.OpenCaptured(ctx, object)
		assert.NoErr(err)
		var actual bytes.Buffer
		_, err = actual.ReadFrom(body)
		assert.NoErr(err)
		assert.NoErr(body.Close())
		assert.Equal(actual.Bytes(), data[object.Key])
	}
	repository := offsite.Repository{URL: fmt.Sprintf("s3:%s/offsite/repository", externalEndpoint), Region: source.Region, Password: "independently-retained-recovery-password", AccessKey: "independent-offsite-access", SecretKey: "independent-offsite-secret"}
	inventory, err := offsite.Freeze(ctx, source)
	assert.NoErr(err)
	recipient := ""
	if nativeEnabled {
		recipient = native.bao.Recipient
	}
	checkpoint := captureOffsiteCheckpoint(t, ctx, primaryKms.client, recipient)
	data[managementbackup.PayloadKey(checkpoint.Artifact)], err = os.ReadFile(checkpoint.File)
	assert.NoErr(err)
	data[managementbackup.CompletionKey(checkpoint.Artifact)], err = json.Marshal(checkpoint.Artifact)
	assert.NoErr(err)
	run := offsite.RunInput{Repository: repository, Source: source, Inventory: inventory, Checkpoint: checkpoint, Executable: binary, CacheDir: filepath.Join(t.TempDir(), "metadata-cache"), KeepLast: 3}
	first, err := offsite.Run(ctx, run)
	assert.NoErr(err)
	assert.True(first != "")
	cacheEntries, err := os.ReadDir(run.CacheDir)
	assert.NoErr(err)
	assert.True(len(cacheEntries) > 0)
	second, err := offsite.Run(ctx, run)
	assert.NoErr(err)
	assert.True(second != "" && second != first)
	snapshots, err := offsite.List(ctx, repository, offsite.SourceID(source))
	assert.NoErr(err)
	assert.Equal(len(snapshots), 2)
	missingWal := server + "/wals/0000000100000000/000000010000000000000002.gz"
	if seeded {
		for key := range data {
			if strings.Contains(key, "/base/") && strings.HasSuffix(key, "data.tar.gz") {
				missingWal = key
				break
			}
		}
	}
	if managementOnly {
		missingWal = "management/sqlite/" + native.sqlite.ID + "/payload"
	}
	assert.NoErr(storage.Delete(ctx, missingWal))
	failed, err := offsite.Run(ctx, run)
	assert.True(err != nil)
	assert.Equal(failed, "")
	after, err := offsite.List(ctx, repository, offsite.SourceID(source))
	assert.NoErr(err)
	assert.Equal(after, snapshots)
	t.Log("two Restic snapshots committed; a producer failure preserved both prior snapshot identities")
	for key := range data {
		assert.NoErr(storage.Delete(ctx, key))
	}

	sourceDeployment, err := client.AppsV1().Deployments(namespace).Get(ctx, "rustfs", metav1.GetOptions{})
	assert.NoErr(err)
	zero := int32(0)
	sourceDeployment.Spec.Replicas = &zero
	_, err = client.AppsV1().Deployments(namespace).Update(ctx, sourceDeployment, metav1.UpdateOptions{})
	assert.NoErr(err)
	stopForward()
	assert.NoErr(client.CoreV1().Pods(namespace).Delete(ctx, "source-kms", metav1.DeleteOptions{}))
	externalKms.seal(t, ctx)
	extractOffsiteCheckpoint(t, ctx, repository, second, checkpoint.Artifact)
	for _, key := range externalKms.original.UnsealKeysB64 {
		assert.NoErr(externalKms.client.Unseal(ctx, key))
	}
	assert.NoErr(externalKms.client.WaitForActive(ctx, 100*time.Millisecond, time.Minute))
	t.Log("direct Restic/tar extraction retained the verified OpenBao checkpoint with both provider KMS services unavailable")
	destination := offsite.StorageConfig{Endpoint: externalEndpoint, Region: source.Region, Bucket: "offsite", Prefix: "restored", PathStyle: true, AccessKey: "independent-offsite-access", SecretKey: "independent-offsite-secret"}
	recovery := offsite.RecoveryConfig{Repository: repository, Source: offsite.SourceID(source), Destination: destination}
	private, err := json.Marshal(recovery)
	assert.NoErr(err)
	file := filepath.Join(t.TempDir(), "recovery.json")
	assert.NoErr(os.WriteFile(file, private, 0600))
	command := exec.CommandContext(ctx, binary, "management", "backups", "list", "--config", file)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("offline list failed: %v %s", err, output)
	}
	assert.True(bytes.Contains(output, []byte(first)))
	manifestFile := filepath.Join(t.TempDir(), "restored-manifest.json")
	command = exec.CommandContext(ctx, binary, "management", "backups", "restore", second, "--config", file, "--manifest", manifestFile)
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("offline recovery failed: %v %s", err, output)
	}
	assert.True(bytes.Contains(output, []byte(fmt.Sprintf("restored %d verified objects", len(data)))))
	completion, err := offsite.ReadArchiveManifest(manifestFile)
	assert.NoErr(err)
	assert.Equal(completion.Source, offsite.SourceID(source))
	assert.Equal(len(completion.Objects), len(data))
	assert.Equal(completion.Version, 4)
	assert.Equal(completion.VaultCheckpoint, checkpoint.Artifact)
	manifestInfo, err := os.Stat(manifestFile)
	assert.NoErr(err)
	assert.Equal(manifestInfo.Mode().Perm(), os.FileMode(0600))
	restored, err := objectstore.New(destination.Config())
	assert.NoErr(err)
	for key, expected := range data {
		objects, err := restored.List(ctx, key)
		assert.NoErr(err)
		assert.Equal(len(objects), 1)
		body, err := restored.OpenCaptured(ctx, objects[0])
		assert.NoErr(err)
		var actual bytes.Buffer
		_, err = actual.ReadFrom(body)
		assert.NoErr(err)
		assert.NoErr(body.Close())
		assert.Equal(actual.Bytes(), expected)
	}
	t.Log("offline CLI restored every byte after source deletion and source-service shutdown using only independent repository and destination credentials")
	if nativeEnabled {
		recoverNativeManagement(t, ctx, kubeconfig, binary, namespace, connection, releases, client, native, recovery)
	}
	if seeded {
		assertOffsitePostgresEncryption(t, ctx, destination.Config())
		readConfig := destination.Config()
		readConfig.Prefix = path.Join(destination.Prefix, path.Dir(server))
		readKeys, err := objectstore.RustfsReadCredentials(ctx, readConfig)
		assert.NoErr(err)
		recoveryDatabase.Recovery.Storage = postgres.Storage{Endpoint: "http://rustfs-offsite-svc." + namespace + ".svc:9000", Region: destination.Region, Bucket: destination.Bucket, Prefix: path.Join(destination.Prefix, path.Dir(server)), ServerName: path.Base(server), PathStyle: true, AccessKey: readKeys.AccessKey, SecretKey: readKeys.SecretKey}
		liveConfig := destination.Config()
		liveConfig.Bucket, liveConfig.Prefix = "recovered-live", ""
		liveClient, err := objectstore.New(liveConfig)
		assert.NoErr(err)
		assert.NoErr(liveClient.EnsureBucket(ctx))
		liveKeys, err := objectstore.RustfsCredentials(ctx, liveConfig)
		assert.NoErr(err)
		recoveryDatabase.Storage = postgres.Storage{Endpoint: recoveryDatabase.Recovery.Storage.Endpoint, Region: liveConfig.Region, Bucket: liveConfig.Bucket, ServerName: "archive-recovered-live", PathStyle: true, AccessKey: liveKeys.AccessKey, SecretKey: liveKeys.SecretKey}
		assert.NoErr(postgresClient.Reconcile(ctx, recoveryDatabase))
		waitOffsitePostgres(t, ctx, postgresClient, recoveryDatabase)
		actual, err := postgresClient.Query(ctx, namespace, recoveryDatabase.Name, "app", offsiteSeedFingerprint())
		assert.NoErr(err)
		assert.Equal(actual, expectedRows)
		t.Log("real CNPG recovered the seeded schema and row fingerprint from rehydrated S3 with the original database and source storage unavailable")
		assert.NoErr(postgresClient.RetireRecoveryAccess(ctx, recoveryDatabase))
		_, err = client.CoreV1().Secrets(namespace).Get(ctx, recoveryDatabase.Name+"-source-s3", metav1.GetOptions{})
		assert.True(apierrors.IsNotFound(err))
		_, err = postgresClient.Query(ctx, namespace, recoveryDatabase.Name, "app", "SELECT 1;")
		assert.NoErr(err)
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + recoveryDatabase.Name})
		assert.NoErr(err)
		assert.Equal(len(pods.Items), 1)
		assert.NoErr(client.CoreV1().Pods(namespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{}))
		for {
			replacements, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + recoveryDatabase.Name})
			assert.NoErr(err)
			ready := false
			for _, replacement := range replacements.Items {
				ready = ready || replacement.UID != pods.Items[0].UID && replacement.DeletionTimestamp == nil && podReady(replacement)
			}
			if ready {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("replacement PostgreSQL pod did not become ready without source credentials")
			case <-time.After(time.Second):
			}
		}
		waitOffsitePostgres(t, ctx, postgresClient, recoveryDatabase)
		afterRestart, err := postgresClient.Query(ctx, namespace, recoveryDatabase.Name, "app", offsiteSeedFingerprint())
		assert.NoErr(err)
		assert.Equal(afterRestart, expectedRows)
		pointInTime := recoveryDatabase
		pointInTime.Name, pointInTime.Owner = "offsite-pitr", strings.Repeat("9", 32)
		pointInTime.Recovery = &postgres.Recovery{Storage: recoveryDatabase.Recovery.Storage, BackupID: recoveryDatabase.Recovery.BackupID, Target: recoveryTarget}
		assert.NoErr(postgresClient.Reconcile(ctx, pointInTime))
		waitOffsitePostgres(t, ctx, postgresClient, pointInTime)
		rows, err := postgresClient.Query(ctx, namespace, pointInTime.Name, "app", "SELECT id FROM offsite_recovery.probe WHERE id >= 65 ORDER BY id;")
		assert.NoErr(err)
		assert.Equal(strings.TrimSpace(rows), "65")
		unreachable := pointInTime
		unreachable.Name, unreachable.Owner = "offsite-unreachable", strings.Repeat("8", 32)
		unreachable.Recovery = &postgres.Recovery{Storage: pointInTime.Recovery.Storage, BackupID: pointInTime.Recovery.BackupID, Target: time.Now().UTC()}
		assert.NoErr(postgresClient.Reconcile(ctx, unreachable))
		failureContext, cancelFailure := context.WithTimeout(ctx, 2*time.Minute)
		defer cancelFailure()
		for {
			observation, err := postgresClient.Observe(failureContext, unreachable)
			if err == nil {
				assert.True(!observation.Ready)
			}
			pods, err := client.CoreV1().Pods(namespace).List(failureContext, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + unreachable.Name})
			assert.NoErr(err)
			failed := false
			for _, pod := range pods.Items {
				for _, container := range pod.Spec.Containers {
					if container.Name != "postgres" && container.Name != "full-recovery" {
						continue
					}
					logs, _ := client.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name}).DoRaw(failureContext)
					failed = failed || bytes.Contains(logs, []byte("recovery ended before configured recovery target was reached"))
				}
			}
			if failed {
				break
			}
			select {
			case <-failureContext.Done():
				t.Fatal("PostgreSQL did not report the unreachable recovery target")
			case <-time.After(time.Second):
			}
		}
		t.Log("latest recovery includes post-base WAL commits; PITR excludes the later commit; unreachable target fails in PostgreSQL")
	}

}
func podReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
func startOffsiteForward(t *testing.T, ctx context.Context, kubeconfig, namespace, service string, port, remotePort int) func() {
	t.Helper()
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", namespace, "port-forward", "service/"+service, fmt.Sprintf("%d:%d", port, remotePort), "--address=127.0.0.1")
	log, err := os.CreateTemp(t.TempDir(), "forward-")
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = log.Close()
		}
	}
	t.Cleanup(stop)
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("isolated %s port forward: %s", service, data)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			connection.Close()
			return stop
		}
		if time.Now().After(deadline) {
			stop()
			data, _ := os.ReadFile(log.Name())
			t.Fatalf("S3 forward did not become available: %s", data)
		}
		select {
		case <-ctx.Done():
			stop()
			t.Fatal("S3 forward canceled")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func seedOffsitePostgres(t *testing.T, ctx context.Context, kubeconfig, namespace string, source objectstore.Config) (postgres.Client, postgres.Input, string, time.Time) {
	t.Helper()
	assert := is.New(t)
	client, err := postgres.New(kubernetes.Config{KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	archiveID := strings.Repeat("b", 32)
	prefix := path.Join(source.Prefix, "organizations", strings.Repeat("a", 32), "postgres", archiveID)
	input := postgres.Input{Namespace: namespace, Name: "offsite-source", Owner: strings.Repeat("d", 32), Instances: 1, StorageSize: "1Gi", StorageClass: offsiteStorageClass(), Image: "ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie", Database: "app", Username: "app", BootstrapPassword: "isolated-offsite-bootstrap", BackupEnabled: true, BackupSchedule: "0 0 0 * * *", RetentionDays: 7, Storage: postgres.Storage{Endpoint: "http://rustfs-svc." + namespace + ".svc:9000", Region: source.Region, Bucket: source.Bucket, Prefix: prefix, ServerName: "archive-" + archiveID, PathStyle: true, AccessKey: source.Credentials.AccessKey, SecretKey: source.Credentials.SecretKey}}
	assert.NoErr(client.Reconcile(ctx, input))
	waitOffsitePostgres(t, ctx, client, input)
	_, err = client.Query(ctx, namespace, input.Name, "app", `CREATE SCHEMA offsite_recovery; CREATE TABLE offsite_recovery.probe(id integer PRIMARY KEY, label text, payload bytea, amount numeric(12,3)); INSERT INTO offsite_recovery.probe SELECT n, CASE WHEN n%7=0 THEN NULL ELSE 'unicode-λ-'||n END, decode(lpad(to_hex(n),8,'0'),'hex'), n*1.125 FROM generate_series(1,64) n;`)
	assert.NoErr(err)
	expected, err := client.Query(ctx, namespace, input.Name, "app", offsiteSeedFingerprint())
	assert.NoErr(err)
	operation := strings.Repeat("e", 32)
	assert.NoErr(client.RequestBackup(ctx, input, operation))
	for {
		complete, err := client.BackupComplete(ctx, input, operation)
		assert.NoErr(err)
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("real PostgreSQL backup did not complete")
		case <-time.After(2 * time.Second):
		}
	}
	archiveConfig := source
	archiveConfig.Prefix = prefix
	selected := ""
	for selected == "" {
		backups, err := postgres.Catalogue(ctx, archiveConfig, input.Storage.ServerName)
		assert.NoErr(err)
		for _, backup := range backups {
			if backup.Eligible {
				selected = backup.ID
			}
		}
		if selected != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("real PostgreSQL backup did not become recoverable")
		case <-time.After(2 * time.Second):
		}
	}
	// These commits exist only in WAL newer than the completed base backup.
	_, err = client.Query(ctx, namespace, input.Name, "app", `INSERT INTO offsite_recovery.probe VALUES (65,'after-base',NULL,65);`)
	assert.NoErr(err)
	targetValue, err := client.Query(ctx, namespace, input.Name, "app", `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"');`)
	assert.NoErr(err)
	recoveryTarget, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(targetValue))
	assert.NoErr(err)
	_, err = client.Query(ctx, namespace, input.Name, "app", `INSERT INTO offsite_recovery.probe VALUES (66,'after-target',NULL,66);`)
	assert.NoErr(err)
	expected, err = client.Query(ctx, namespace, input.Name, "app", offsiteSeedFingerprint())
	assert.NoErr(err)
	segment, err := client.Query(ctx, namespace, input.Name, "app", `SELECT pg_walfile_name(pg_current_wal_insert_lsn()); SELECT pg_switch_wal();`)
	assert.NoErr(err)
	wal := strings.Split(strings.TrimSpace(segment), "\n")[0]
	for {
		archived, err := client.Query(ctx, namespace, input.Name, "app", "SELECT coalesce(last_archived_wal >= '"+wal+"',false) FROM pg_stat_archiver;")
		assert.NoErr(err)
		if strings.TrimSpace(archived) == "t" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("post-backup transactions were not archived")
		case <-time.After(time.Second):
		}
	}
	// Removing the source stops further WAL writes before the captured export inventory is selected.
	for {
		removed, err := client.DeleteOwned(ctx, input)
		assert.NoErr(err)
		if removed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("source PostgreSQL did not stop")
		case <-time.After(time.Second):
		}
	}
	recovered := input
	recovered.Name = "offsite-recovered"
	recovered.Owner = strings.Repeat("f", 32)
	recovered.Isolated = true
	recovered.BackupEnabled = false
	recovered.Recovery = &postgres.Recovery{BackupID: selected}
	return client, recovered, expected, recoveryTarget
}
func waitOffsitePostgres(t *testing.T, ctx context.Context, client postgres.Client, input postgres.Input) {
	t.Helper()
	for {
		observed, err := client.Observe(ctx, input)
		if err == nil && observed.Ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PostgreSQL %s did not become ready: %v", input.Name, err)
		case <-time.After(2 * time.Second):
		}
	}
}
func offsiteSeedFingerprint() string {
	return `SELECT 'rows='||count(*)||';digest='||md5(string_agg(row_to_json(p)::text,E'\n' ORDER BY id)) FROM offsite_recovery.probe p; SELECT column_name||':'||data_type FROM information_schema.columns WHERE table_schema='offsite_recovery' AND table_name='probe' ORDER BY ordinal_position;`
}

func assertOffsitePostgresEncryption(t *testing.T, ctx context.Context, config objectstore.Config) {
	t.Helper()
	assert := is.New(t)
	store, err := objectstore.New(config)
	assert.NoErr(err)
	objects, err := store.List(ctx, "organizations/")
	assert.NoErr(err)
	provider := s3.New(s3.Options{Region: config.Region, Credentials: credentials.NewStaticCredentialsProvider(config.Credentials.AccessKey, config.Credentials.SecretKey, ""), BaseEndpoint: aws.String(config.Endpoint), UsePathStyle: config.PathStyle, RetryMaxAttempts: 1})
	base, wal := 0, 0
	for _, object := range objects {
		if !strings.Contains(object.Key, "/postgres/") {
			continue
		}
		head, err := provider.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String(path.Join(config.Prefix, object.Key))})
		assert.NoErr(err)
		assert.Equal(string(head.ServerSideEncryption), "AES256")
		if strings.Contains(object.Key, "/base/") {
			base++
		}
		if strings.Contains(object.Key, "/wals/") {
			wal++
		}
	}
	assert.True(base > 0)
	assert.True(wal > 0)
	t.Logf("verified AES256 on actual PostgreSQL base-backup (%d) and WAL (%d) objects", base, wal)
}

func offsiteStorageClass() string {
	if selected := os.Getenv("BORQ_TEST_STORAGE_CLASS"); selected != "" {
		return selected
	}
	return "local-path"
}
