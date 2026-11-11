//go:build integration

package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestSqliteIntegration proves scoped uploads, non-root writes, volume loss recovery,
// offsite rehydration, backup suspension and durable manager state against real controllers.
func TestSqliteIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(key, os.Getenv(key)+",127.0.0.1,localhost,172.23.0.0/16,10.42.0.0/16,10.43.0.0/16")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	assert.NoErr(err)
	client, err := kubeclient.NewForConfig(config)
	assert.NoErr(err)
	system := fmt.Sprintf("borq-sqlite-%d", time.Now().UnixNano())
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: system}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, system, metav1.DeleteOptions{})
	})
	deploySqliteDependency(t, ctx, client, system, "rustfs", "rustfs/rustfs:1.0.1", []string{"/data"}, 9000, []corev1.EnvVar{{Name: "RUSTFS_ACCESS_KEY", Value: "sqlite-test-admin"}, {Name: "RUSTFS_SECRET_KEY", Value: "sqlite-test-management-secret"}, {Name: "RUSTFS_CONSOLE_ENABLE", Value: "false"}})
	deploySqliteDependency(t, ctx, client, system, "openbao", "quay.io/openbao/openbao:2.7.1", []string{"server", "-dev", "-dev-root-token-id=sqlite-root", "-dev-listen-address=0.0.0.0:8200"}, 8200, nil)
	_, err = client.CoreV1().Secrets(system).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-openbao-admin"}, StringData: map[string]string{"token": "sqlite-root"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	service, err := client.CoreV1().Services(system).Get(ctx, "rustfs", metav1.GetOptions{})
	assert.NoErr(err)
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	nodeAddress := ""
	for _, address := range nodes.Items[0].Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			nodeAddress = address.Address
		}
	}
	endpoint := fmt.Sprintf("http://%s:%d", nodeAddress, service.Spec.Ports[0].NodePort)
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{Profile: "dev", Namespace: system}, PollInterval: 100 * time.Millisecond, RolloutTimeout: 3 * time.Minute}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	storage, err := manager.prepareManagementS3(ctx, ManagementS3Input{Endpoint: endpoint, Region: "us-east-1", Bucket: "management", PathStyle: true, AccessKey: "sqlite-test-admin", SecretKey: "sqlite-test-management-secret"})
	assert.NoErr(err)
	managementRoot, err := manager.ResolveManagementS3(ctx, storage)
	assert.NoErr(err)
	managementStore, err := objectstore.New(managementRoot)
	assert.NoErr(err)
	assert.NoErr(managementStore.EnsureBucket(ctx))
	write, err := documentWrite("management-s3", "shared", storage, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write}))
	organization := fmt.Sprintf("sqlite-%d", time.Now().UnixNano())
	owner, err := manager.CreateOrganization(ctx, organization)
	assert.NoErr(err)
	_, err = manager.RegisterCluster(ctx, ClusterInput{Organization: organization, Name: "apps", KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	space, err := manager.CreateSpace(ctx, SpaceInput{Organization: organization, Name: "test", Cluster: "apps"})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(cleanup, space.Namespace, metav1.DeleteOptions{})
	})
	apply := func(source string) Plan {
		t.Helper()
		var patch Patch
		assert.NoErr(json.Unmarshal([]byte(source), &patch))
		plan, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: patch})
		assert.NoErr(err)
		_, err = manager.Apply(ctx, plan.ID)
		assert.NoErr(err)
		for {
			operation, err := manager.GetOperation(ctx, plan.ID)
			assert.NoErr(err)
			if operationFinished(operation.Status) {
				if operation.Status != "succeeded" {
					t.Fatalf("apply %s: %s", operation.Status, operation.Message)
				}
				return plan
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	// Space creation provisions storage without requiring an application deployment.
	otherSpace, err := manager.CreateSpace(ctx, SpaceInput{Organization: organization, Name: "other", Cluster: "apps"})
	assert.NoErr(err)
	t.Cleanup(func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), otherSpace.Namespace, metav1.DeleteOptions{})
	})
	for {
		first, err := manager.GetSpace(ctx, organization, "test")
		assert.NoErr(err)
		second, err := manager.GetSpace(ctx, organization, "other")
		assert.NoErr(err)
		if first.BackupStorage.State == "ready" && second.BackupStorage.State == "ready" {
			assert.True(first.BackupStorage.Bucket != second.BackupStorage.Bucket)
			location, credentials, err := manager.ResolvePostgresStorage(ctx, organization, "test")
			assert.Equal(location.Prefix, "organizations/"+owner.ID+"/postgres")
			assert.NoErr(err)
			own, err := objectstore.New(credentials)
			assert.NoErr(err)
			assert.NoErr(own.Validate(ctx))
			// A restore source grants read access to one archive and cannot upload or enumerate the bucket.
			probe := credentials
			probe.Prefix = "recovery-fixture"
			archiveWriter, err := objectstore.New(probe)
			assert.NoErr(err)
			assert.NoErr(archiveWriter.Put(ctx, "sentinel", []byte("source archive")))
			readAccess, err := manager.recoveryArchiveStorage(ctx, postgresArchive{Endpoint: location.Endpoint, Region: location.Region, Bucket: location.Bucket, Prefix: probe.Prefix, PathStyle: location.PathStyle, CredentialsRef: location.CredentialsRef})
			assert.NoErr(err)
			probe.Credentials = objectstore.Credentials{AccessKey: readAccess.AccessKey, SecretKey: readAccess.SecretKey}
			archiveReader, err := objectstore.New(probe)
			assert.NoErr(err)
			data, err := archiveReader.Read(ctx, "sentinel")
			assert.NoErr(err)
			assert.Equal(string(data), "source archive")
			assert.True(archiveReader.Put(ctx, "forbidden", []byte("must fail")) != nil)
			probe.Prefix = ""
			bucketReader, err := objectstore.New(probe)
			assert.NoErr(err)
			_, err = bucketReader.List(ctx, "")
			assert.True(err != nil)
			credentials.Bucket = second.BackupStorage.Bucket
			foreign, err := objectstore.New(credentials)
			assert.NoErr(err)
			_, err = foreign.List(ctx, "")
			assert.True(err != nil)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("space provisioning: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	plan := apply(`{"apps":[{"name":"web","image":"borq-sqlite-test:local","port":8080,"env":{"version":"0"},"sqlite":{"enabled":true}}]}`)
	request := func(method string) int {
		t.Helper()
		data, err := client.CoreV1().RESTClient().Verb(method).Namespace(space.Namespace).Resource("services").Name("web:8080").SubResource("proxy").DoRaw(ctx)
		assert.NoErr(err)
		var result struct {
			Count     int
			Integrity string
			UID       int
		}
		assert.NoErr(json.Unmarshal(data, &result))
		assert.Equal(result.Integrity, "ok")
		assert.Equal(result.UID, 10001)
		return result.Count
	}
	for index := 1; index <= 3; index++ {
		assert.Equal(request("POST"), index)
	}
	claim, err := client.CoreV1().PersistentVolumeClaims(space.Namespace).Get(ctx, "web-sqlite", metav1.GetOptions{})
	assert.NoErr(err)
	secret, err := client.CoreV1().Secrets(space.Namespace).Get(ctx, "web-litestream", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(string(secret.Data["AWS_SECRET_ACCESS_KEY"]) != "sqlite-test-management-secret")
	record, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, "test"))
	assert.NoErr(err)
	root, err := manager.ResolveManagementS3(ctx, record.SqliteStorage)
	assert.NoErr(err)
	scoped := root
	scoped.Credentials = objectstore.Credentials{AccessKey: string(secret.Data["AWS_ACCESS_KEY_ID"]), SecretKey: string(secret.Data["AWS_SECRET_ACCESS_KEY"])}
	scoped.Prefix = "management"
	outside, err := objectstore.New(scoped)
	assert.NoErr(err)
	assert.True(outside.Put(ctx, "forbidden", []byte("must fail")) != nil)
	scoped.Bucket = "management"
	outside, err = objectstore.New(scoped)
	assert.NoErr(err)
	_, err = outside.List(ctx, "")
	assert.True(err != nil)
	// An update with a predeploy task must use the node already mounting this claim.
	apply(`{"apps":[{"name":"web","predeploy":{"command":"python -c 'import os,sqlite3; db=sqlite3.connect(os.environ[\"SQLITE_DATABASE_PATH\"]); db.execute(\"INSERT INTO records(value) VALUES (42)\"); db.commit()'"}}]}`)
	assert.Equal(request("GET"), 4)
	apply(`{"apps":[{"name":"web","replicas":0,"predeploy":{"command":""}}]}`)
	for {
		pods, err := client.CoreV1().Pods(space.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=web"})
		assert.NoErr(err)
		active := false
		for _, pod := range pods.Items {
			active = active || pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
		}
		if !active {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	// The complete Litestream objects also survive the offline archive protocol.
	var archive bytes.Buffer
	assert.NoErr(offsite.Export(ctx, root, &archive))
	recovery := root
	recovery.Prefix = "offsite-restored"
	manifest, err := offsite.Restore(ctx, recovery, offsite.SourceID(root), bytes.NewReader(archive.Bytes()))
	assert.NoErr(err)
	assert.True(len(manifest.Sqlite) > 0)
	// Exercise the operator-facing offline rebind rather than rewriting private records.
	selectedObjects, err := offsite.SelectSpaceObjects(manifest, space.ID, nil)
	assert.NoErr(err)
	original, err := objectstore.New(root)
	assert.NoErr(err)
	objects, err := original.List(ctx, "sqlite/")
	assert.NoErr(err)
	for _, object := range objects {
		assert.NoErr(original.Delete(ctx, object.Key))
	}
	assert.NoErr(manager.Close())
	directory := filepath.Dir(input.DatabasePath)
	assert.NoErr(os.WriteFile(filepath.Join(directory, "RECOVERY_REQUIRED"), []byte("test recovery"), 0600))
	vaultService, err := client.CoreV1().Services(system).Get(ctx, "openbao", metav1.GetOptions{})
	assert.NoErr(err)
	recoveryAdmin := recovery
	recoveryAdmin.Credentials = objectstore.Credentials{AccessKey: "sqlite-test-admin", SecretKey: "sqlite-test-management-secret"}
	assert.NoErr(managementbackup.RebindSpace(ctx, directory, managementbackup.SpaceRecovery{SpaceID: space.ID, Source: manifest.Source, Storage: recoveryAdmin, Objects: selectedObjects, OpenBao: managementbackup.OpenBaoRecovery{Address: fmt.Sprintf("http://%s:%d", nodeAddress, vaultService.Spec.Ports[0].NodePort), OriginalToken: "sqlite-root"}}))
	assert.NoErr(managementbackup.Resume(directory))
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	recoveredRecord, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, "test"))
	assert.NoErr(err)
	assert.True(strings.HasPrefix(recoveredRecord.SqliteStorage.Prefix, recovery.Prefix+"/live-"))
	jobs, err := client.BatchV1().Jobs(space.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/phase=predeploy"})
	assert.NoErr(err)
	for _, job := range jobs.Items {
		propagation := metav1.DeletePropagationForeground
		assert.NoErr(client.BatchV1().Jobs(space.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}))
	}
	assert.NoErr(client.CoreV1().PersistentVolumeClaims(space.Namespace).Delete(ctx, "web-sqlite", metav1.DeleteOptions{}))
	for {
		_, err := client.CoreV1().PersistentVolumeClaims(space.Namespace).Get(ctx, "web-sqlite", metav1.GetOptions{})
		if err != nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	apply(`{"apps":[{"name":"web","replicas":1}]}`)
	assert.Equal(request("GET"), 4)
	restoredClaim, err := client.CoreV1().PersistentVolumeClaims(space.Namespace).Get(ctx, "web-sqlite", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(claim.UID != restoredClaim.UID)
	// Run the actual scheduled verification pod against S3 and SQLite, without mounting the live PVC.
	check, err := client.BatchV1().CronJobs(space.Namespace).Get(ctx, "web-sqlite-check", metav1.GetOptions{})
	assert.NoErr(err)
	job, err := client.BatchV1().Jobs(space.Namespace).Create(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{GenerateName: "sqlite-verify-"}, Spec: check.Spec.JobTemplate.Spec}, metav1.CreateOptions{})
	assert.NoErr(err)
	for {
		observed, err := client.BatchV1().Jobs(space.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		assert.NoErr(err)
		if observed.Status.Failed > 0 {
			t.Fatal("scheduled SQLite restore verification failed")
		}
		if observed.Status.Succeeded > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	disabledPlan := apply(`{"apps":[{"name":"web","sqlite":{"backups":false}}]}`)
	assert.Equal(request("GET"), 4)
	retainedClaim, err := client.CoreV1().PersistentVolumeClaims(space.Namespace).Get(ctx, "web-sqlite", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retainedClaim.UID, restoredClaim.UID)
	reader, err := newClusterClient(Cluster{KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	differences, err := reader.Observe(ctx, disabledPlan.Manifests, space.ID)
	assert.NoErr(err)
	liveManifests := []map[string]any{}
	for _, difference := range differences {
		liveManifests = append(liveManifests, difference.Live)
	}
	live, err := manager.InspectApps(ctx, liveManifests)
	assert.NoErr(err)
	assert.Equal(live.Apps[0].Sqlite, SqliteSettings{Enabled: true})
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	retained, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, "test"))
	assert.NoErr(err)
	assert.Equal(retained.SqliteStorage, recoveredRecord.SqliteStorage)
	assert.Equal(retained.Space.Configuration.Apps[0].Sqlite, SqliteSettings{Enabled: true})
	// A lost volume and missing backup must fail at the initialization guard, even if its ConfigMap is lost.
	apply(`{"apps":[{"name":"web","replicas":0,"sqlite":{"backups":true}}]}`)
	for {
		pods, err := client.CoreV1().Pods(space.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=web"})
		assert.NoErr(err)
		active := false
		for _, pod := range pods.Items {
			active = active || pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
		}
		if !active {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	assert.NoErr(client.CoreV1().PersistentVolumeClaims(space.Namespace).Delete(ctx, "web-sqlite", metav1.DeleteOptions{}))
	liveStorage, err := manager.ResolveManagementS3(ctx, retained.SqliteStorage)
	assert.NoErr(err)
	liveStore, err := objectstore.New(liveStorage)
	assert.NoErr(err)
	missingObjects, err := liveStore.List(ctx, "sqlite/"+space.ID+"/web/")
	assert.NoErr(err)
	assert.True(len(missingObjects) > 0)
	for _, object := range missingObjects {
		assert.NoErr(liveStore.Delete(ctx, object.Key))
	}
	assert.NoErr(client.CoreV1().ConfigMaps(space.Namespace).Delete(ctx, "web-sqlite-state", metav1.DeleteOptions{}))
	for {
		_, err := client.CoreV1().PersistentVolumeClaims(space.Namespace).Get(ctx, "web-sqlite", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		assert.NoErr(err)
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	var restart Patch
	assert.NoErr(json.Unmarshal([]byte(`{"apps":[{"name":"web","replicas":1}]}`), &restart))
	restartPlan, err := manager.Plan(ctx, PlanInput{Organization: organization, Space: "test", Patch: restart})
	assert.NoErr(err)
	_, err = manager.Apply(ctx, restartPlan.ID)
	assert.NoErr(err)
	guardContext, cancelGuard := context.WithTimeout(ctx, 90*time.Second)
	defer cancelGuard()
	lastLog := ""
	for {
		pods, err := client.CoreV1().Pods(space.Namespace).List(guardContext, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=web"})
		assert.NoErr(err)
		guarded := false
		for _, pod := range pods.Items {
			for _, status := range pod.Status.InitContainerStatuses {
				failed := status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 || status.LastTerminationState.Terminated != nil && status.LastTerminationState.Terminated.ExitCode != 0
				if status.Name != "sqlite-prepare" || !failed {
					continue
				}
				// Read the latest attempt; older container logs may already have been collected by Kubernetes.
				logs, err := client.CoreV1().Pods(space.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "sqlite-prepare"}).DoRaw(guardContext)
				if err != nil {
					continue
				}
				lastLog = string(logs)
				guarded = strings.Contains(lastLog, "refusing to create an empty database")
			}
		}
		if guarded {
			break
		}
		select {
		case <-guardContext.Done():
			t.Fatalf("missing-backup guard was not reached: %s", lastLog)
		case <-time.After(100 * time.Millisecond):
		}
	}

	encoded, err := json.Marshal(plan)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "sqlite-test-management-secret"))
}

func deploySqliteDependency(t *testing.T, ctx context.Context, client *kubeclient.Clientset, namespace, name, image string, args []string, port int32, env []corev1.EnvVar) {
	t.Helper()
	assert := is.New(t)
	labels := map[string]string{"fixture": name}
	_, err := client.AppsV1().Deployments(namespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image, Args: args, Env: env, Ports: []corev1.ContainerPort{{ContainerPort: port}}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}}, Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Services(namespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: labels, Ports: []corev1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(port)}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	for {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
		if deployment.Status.AvailableReplicas == 1 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
