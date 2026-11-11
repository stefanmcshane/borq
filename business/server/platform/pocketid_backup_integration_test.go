//go:build integration

package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// pocketIdBackupFixture runs real RustFS in the test's management namespace. Its
// ephemeral disk is independent of both the source and restored Pocket ID PVCs.
func pocketIdBackupFixture(t *testing.T, ctx context.Context, kubeconfig string, connection kubernetes.Connection, client ctrlclient.Client, namespace, instance string) (objectstore.Config, objectstore.Credentials) {
	t.Helper()
	is := is.New(t)
	root := objectstore.Credentials{AccessKey: "pocketid-" + rand.Text(), SecretKey: rand.Text() + rand.Text()}
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-rustfs", Namespace: namespace},
		StringData: map[string]string{"access": root.AccessKey, "secret": root.SecretKey}}
	is.NoErr(client.Create(ctx, &secret))
	labels := map[string]string{"fixture": "rustfs"}
	replicas, uid, disabled, enabled := int32(1), int64(10001), false, true
	deployment := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "fixture-rustfs", Namespace: namespace}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &disabled,
			SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, RunAsNonRoot: &enabled},
			Containers: []corev1.Container{{Name: "rustfs", Image: "rustfs/rustfs:1.0.1", Command: []string{"/usr/bin/rustfs"}, Args: []string{"/data"},
				Env: []corev1.EnvVar{
					{Name: "RUSTFS_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "access"}}},
					{Name: "RUSTFS_SECRET_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "secret"}}},
					{Name: "RUSTFS_CONSOLE_ENABLE", Value: "false"}, {Name: "RUSTFS_OBS_LOG_DIRECTORY", Value: ""},
				},
				Ports:          []corev1.ContainerPort{{ContainerPort: 9000}},
				VolumeMounts:   []corev1.VolumeMount{{Name: "objects", MountPath: "/data"}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health/ready", Port: intstr.FromInt32(9000)}}, PeriodSeconds: 2},
				Resources:      corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}},
			}},
			Volumes: []corev1.Volume{{Name: "objects", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<30, resource.BinarySI)}}}},
		}},
	}}
	is.NoErr(client.Create(ctx, &deployment))
	service := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "rustfs-svc", Namespace: namespace}, Spec: corev1.ServiceSpec{
		Selector: labels, Ports: []corev1.ServicePort{{Port: 9000, TargetPort: intstr.FromInt32(9000)}},
	}}
	is.NoErr(client.Create(ctx, &service))
	is.NoErr(connection.Deployments().WaitForRollout(ctx, namespace, deployment.Name))
	endpoint := pocketIdRustfsForward(t, ctx, kubeconfig, namespace)
	config := objectstore.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "borq-pocketid-" + instance, Prefix: "sqlite", PathStyle: true, Credentials: root}
	storage, err := objectstore.New(config)
	is.NoErr(err)
	is.NoErr(storage.EnsureBucket(ctx))
	scoped, err := objectstore.RustfsCredentials(ctx, config)
	is.NoErr(err)
	config.Credentials = scoped
	scopedStore, err := objectstore.New(config)
	is.NoErr(err)
	is.NoErr(scopedStore.Validate(ctx))

	// Each forbidden object is first written and read using root credentials, so
	// the negative cases must reach a real authorization guard, not a missing key.
	sdk := s3.New(s3.Options{Region: config.Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(scoped.AccessKey, scoped.SecretKey, ""), RetryMaxAttempts: 1})
	for _, forbidden := range []struct{ bucket, prefix string }{
		{config.Bucket, "other-provider"}, {"other-" + instance, "sqlite"},
	} {
		foreignConfig := config
		foreignConfig.Credentials, foreignConfig.Bucket, foreignConfig.Prefix = root, forbidden.bucket, forbidden.prefix
		foreign, err := objectstore.New(foreignConfig)
		is.NoErr(err)
		is.NoErr(foreign.EnsureBucket(ctx))
		is.NoErr(foreign.Put(ctx, "sentinel", []byte("foreign archive")))
		stored, err := foreign.Read(ctx, "sentinel")
		is.NoErr(err)
		is.Equal(string(stored), "foreign archive")
		unexpected, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(forbidden.bucket), Key: aws.String(forbidden.prefix + "/sentinel")})
		if unexpected != nil && unexpected.Body != nil {
			unexpected.Body.Close()
		}
		var denied smithy.APIError
		is.True(errors.As(err, &denied))
		is.Equal(denied.ErrorCode(), "AccessDenied")
	}
	return config, scoped
}

func pocketIdRustfsForward(t *testing.T, ctx context.Context, kubeconfig, namespace string) string {
	t.Helper()
	is := is.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	is.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	is.NoErr(listener.Close())
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", namespace, "port-forward", "service/rustfs-svc", fmt.Sprintf("%d:9000", port), "--address=127.0.0.1")
	output, err := os.CreateTemp(t.TempDir(), "rustfs-forward-")
	is.NoErr(err)
	command.Stdout, command.Stderr = output, output
	is.NoErr(command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = output.Close()
	})
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	transport := http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(deadline, http.MethodGet, endpoint+"/health/ready", nil)
		is.NoErr(err)
		response, err := transport.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		select {
		case <-deadline.Done():
			t.Fatal("RustFS port-forward did not become ready")
		case <-ticker.C:
		}
	}
}

func pocketIdWaitForArchive(t *testing.T, ctx context.Context, config objectstore.Config) {
	t.Helper()
	is := is.New(t)
	storage, err := objectstore.New(config)
	is.NoErr(err)
	deadline, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		objects, err := storage.List(deadline, "")
		is.NoErr(err)
		snapshot, wal := false, false
		for _, object := range objects {
			snapshot = snapshot || strings.Contains(object.Key, "/snapshots/") && object.Size > 0
			wal = wal || strings.Contains(object.Key, "/wal/") && object.Size > 0
		}
		if snapshot && wal {
			return
		}
		select {
		case <-deadline.Done():
			t.Fatal("Litestream did not upload a real snapshot and WAL archive to management RustFS")
		case <-ticker.C:
		}
	}
}

// The recovery Pod uses a fresh PVC and the original credentials; the stopped
// source database volume is never attached, copied, or used as a fallback.
func pocketIdRestoreArchive(t *testing.T, ctx context.Context, client ctrlclient.Client, apiHost string, source appsv1.Deployment) string {
	t.Helper()
	is := is.New(t)
	name := source.Name + "-recovery"
	class := "local-path"
	claim := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: source.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &class,
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}}
	is.NoErr(client.Create(ctx, &claim))
	spec := source.Spec.Template.Spec.DeepCopy()
	is.Equal(len(spec.InitContainers), 1)
	restore := spec.InitContainers[0]
	restore.Name, restore.RestartPolicy = "restore", nil
	restore.Args = []string{"restore", "-config", "/etc/litestream/litestream.yml", "/app/data/pocket-id.db"}
	restore.StartupProbe, restore.ReadinessProbe, restore.LivenessProbe = nil, nil, nil
	spec.InitContainers = []corev1.Container{restore}
	for index := range spec.Volumes {
		if spec.Volumes[index].Name == "data" {
			spec.Volumes[index].PersistentVolumeClaim.ClaimName = name
		}
	}
	labels := map[string]string{"pocket-id-recovery": name}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: source.Namespace, Labels: labels}, Spec: *spec}
	is.NoErr(client.Create(ctx, &pod))
	service := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: source.Namespace}, Spec: corev1.ServiceSpec{
		Selector: labels, Ports: []corev1.ServicePort{{Name: "http", Port: 1411, TargetPort: intstr.FromInt32(1411)}},
	}}
	is.NoErr(client.Create(ctx, &service))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&pod), &pod))
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			break
		}
		for _, status := range pod.Status.InitContainerStatuses {
			if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
				t.Fatalf("Litestream restore exited with status %d", status.State.Terminated.ExitCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("Pocket ID did not become ready from its independently restored database")
		case <-ticker.C:
		}
	}
	return strings.TrimRight(apiHost, "/") + "/api/v1/namespaces/" + source.Namespace + "/services/http:" + name + ":1411/proxy"
}

func pocketIdVerifyRestoredIdentity(t *testing.T, ctx context.Context, transport *http.Client, baseURL, apiKey string, admin pocketid.Admin, client pocketid.OidcClient) {
	t.Helper()
	is := is.New(t)
	for _, expected := range []struct {
		path   string
		verify func([]byte)
	}{
		{"/api/users/" + admin.ID, func(data []byte) {
			var account struct {
				ID, Username, Email string
				IsAdmin             bool
			}
			is.NoErr(json.Unmarshal(data, &account))
			is.Equal(account.ID, admin.ID)
			is.Equal(account.Username, admin.Username)
			is.Equal(account.Email, admin.Email)
			is.True(account.IsAdmin)
		}},
		{"/api/oidc/clients/" + client.ID, func(data []byte) {
			var restored struct {
				ID, Name     string
				CallbackURLs []string
				PkceEnabled  bool
			}
			is.NoErr(json.Unmarshal(data, &restored))
			is.Equal(restored.ID, client.ID)
			is.Equal(restored.Name, client.Name)
			is.Equal(restored.CallbackURLs, []string{client.CallbackURL})
			is.True(restored.PkceEnabled)
		}},
	} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+expected.path, nil)
		is.NoErr(err)
		request.Header.Set("X-API-KEY", apiKey)
		response, err := transport.Do(request)
		is.NoErr(err)
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		is.NoErr(err)
		is.Equal(response.StatusCode, http.StatusOK)
		expected.verify(data)
	}
	// Authenticate with the original saved secret without any provisioning call
	// that could repair a missing client or rotate its restored credential.
	grant := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "client_secret": {client.Secret},
		"code": {"borq-restore-verification"}, "redirect_uri": {client.CallbackURL}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/oidc/token", strings.NewReader(grant.Encode()))
	is.NoErr(err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := transport.Do(request)
	is.NoErr(err)
	defer response.Body.Close()
	is.Equal(response.StatusCode, http.StatusBadRequest)
	var rejectedGrant struct {
		Error string `json:"error"`
	}
	is.NoErr(json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&rejectedGrant))
	is.Equal(rejectedGrant.Error, "invalid_grant")
}
