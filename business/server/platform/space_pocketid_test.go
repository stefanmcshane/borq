package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func spacePocketIdTestInput(t *testing.T) SpacePocketIdInput {
	t.Helper()
	input, _, err := normalizeSpacePocketId(SpacePocketIdInput{Namespace: "space-test", ID: "0123456789abcdef0123456789abcdef", IssuerURL: "https://identity.example.test", EncryptionKey: strings.Repeat("e", 32), ApiKey: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// TestSpacePocketIdResources owns the space-specific single-writer manifests and
// retained-storage boundaries; the real cluster test owns rollover persistence.
func TestSpacePocketIdResources(t *testing.T) {
	is := is.New(t)
	input := spacePocketIdTestInput(t)
	resources := spacePocketIdResources(input)
	is.Equal(len(resources), 5)
	for _, object := range resources {
		is.Equal(object.GetNamespace(), input.Namespace)
		is.Equal(object.GetLabels()["borq.io/space-pocket-id"], input.ID)
		is.Equal(len(object.GetOwnerReferences()), 0)
	}
	claim := resources[1].(*corev1.PersistentVolumeClaim)
	is.Equal(claim.Spec.Resources.Requests.Storage().String(), "1Gi")
	is.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce})
	workload := resources[4].(*appsv1.StatefulSet)
	is.Equal(*workload.Spec.Replicas, int32(1))
	is.Equal(workload.Spec.UpdateStrategy.Type, appsv1.RollingUpdateStatefulSetStrategyType)
	is.Equal(workload.Spec.PodManagementPolicy, appsv1.OrderedReadyPodManagement)
	is.Equal(len(workload.Spec.VolumeClaimTemplates), 0)
	pod := workload.Spec.Template.Spec
	is.Equal(len(pod.InitContainers), 0)
	is.Equal(len(pod.Volumes), 3)
	is.Equal(pod.Volumes[0].PersistentVolumeClaim.ClaimName, claim.Name)
	is.Equal(len(pod.Containers), 2)
	environment := map[string]string{}
	for _, variable := range pod.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	is.Equal(environment["HOST"], "127.0.0.1")
	is.Equal(environment["DB_CONNECTION_STRING"], "/app/data/pocket-id.db")
	is.Equal(environment["ALLOW_USER_SIGNUPS"], "disabled")
	is.Equal(environment["LDAP_ENABLED"], "false")
	is.True(strings.Contains(pod.Containers[0].Image, "@sha256:"))
	is.True(strings.Contains(resources[2].(*corev1.ConfigMap).Data["nginx.conf"], "location ~* ^/api/signup/setup { return 404; }"))
}

// TestSpacePocketIdValidation rejects invalid durable configuration before any
// cluster write, including unpinned upstream images and missing space scope.
func TestSpacePocketIdValidation(t *testing.T) {
	for _, test := range []struct {
		name, message string
		change        func(*SpacePocketIdInput)
	}{
		{"namespace", "namespace", func(input *SpacePocketIdInput) { input.Namespace = "" }},
		{"identity", "identity", func(input *SpacePocketIdInput) { input.ID = "not-an-id" }},
		{"origin", "address", func(input *SpacePocketIdInput) { input.IssuerURL = "https://identity.example.test/path" }},
		{"port", "port", func(input *SpacePocketIdInput) { input.IssuerURL = "https://identity.example.test:70000" }},
		{"credentials", "keys", func(input *SpacePocketIdInput) { input.ApiKey = "short" }},
		{"capacity", "storage", func(input *SpacePocketIdInput) { input.Storage = "-1Gi" }},
		{"issuer", "issuer", func(input *SpacePocketIdInput) { input.CertificateIssuer = "bad/name" }},
		{"class", "storage class", func(input *SpacePocketIdInput) { input.StorageClass = "bad/name" }},
		{"image tag", "digest", func(input *SpacePocketIdInput) { input.Image = "ghcr.io/pocket-id/pocket-id:latest" }},
		{"foreign image", "digest", func(input *SpacePocketIdInput) { input.Image = "example.test/other@sha256:" + strings.Repeat("a", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := spacePocketIdTestInput(t)
			test.change(&input)
			_, _, err := normalizeSpacePocketId(input)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %s guard, got %v", test.message, err)
			}
		})
	}
}

// TestSpacePocketIdRecoveryGuards proves loss, replacement and altered runtime
// state cannot silently create an empty identity database or a second writer.
func TestSpacePocketIdRecoveryGuards(t *testing.T) {
	for _, test := range []struct {
		name, message string
		change        func([]ctrlclient.Object) []ctrlclient.Object
	}{
		{"lost secret", "Secret is missing", func(objects []ctrlclient.Object) []ctrlclient.Object { return objects[1:] }},
		{"lost volume", "volume claim is missing", func(objects []ctrlclient.Object) []ctrlclient.Object { return append(objects[:1], objects[2:]...) }},
		{"replaced volume", "claim was replaced", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[1].SetUID(types.UID("replacement"))
			return objects
		}},
		{"replaced secret", "Secret was replaced", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[0].SetUID(types.UID("replacement"))
			return objects
		}},
		{"changed credentials", "credentials differ", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[0].(*corev1.Secret).Data["encryption-key"] = []byte(strings.Repeat("z", 32))
			return objects
		}},
		{"extra replicas", "one ordered", func(objects []ctrlclient.Object) []ctrlclient.Object {
			*objects[4].(*appsv1.StatefulSet).Spec.Replicas = 2
			return objects
		}},
		{"network exposure", "runtime boundary", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[4].(*appsv1.StatefulSet).Spec.Template.Spec.HostNetwork = true
			return objects
		}},
		{"credential injection", "runtime boundary", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[4].(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0].Env = append(objects[4].(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "EXTRA", Value: "bad"})
			return objects
		}},
		{"foreign owner", "not owned", func(objects []ctrlclient.Object) []ctrlclient.Object {
			objects[1].SetLabels(map[string]string{"app.kubernetes.io/managed-by": "somebody-else"})
			return objects
		}},
		{"concurrent deployment", "concurrent Deployment", func(objects []ctrlclient.Object) []ctrlclient.Object {
			return append(objects, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: objects[0].GetName(), Namespace: objects[0].GetNamespace()}})
		}},
		{"unexpected writer", "unexpected writer", func(objects []ctrlclient.Object) []ctrlclient.Object {
			return append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-writer", Namespace: objects[0].GetNamespace()}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: objects[0].GetName()}}}}}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := spacePocketIdTestInput(t)
			desired := spacePocketIdResources(input)
			objects := spacePocketIdResources(input)
			objects[0].SetUID(types.UID("original-secret"))
			objects[1].SetUID(types.UID("original-volume"))
			objects[0].GetAnnotations()["borq.io/pocket-id-pvc-uid"] = "original-volume"
			objects[1].GetAnnotations()["borq.io/pocket-id-secret-uid"] = "original-secret"
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(test.change(objects)...).Build()
			err := checkSpacePocketIdStorage(context.Background(), client, input, desired)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %s guard, got %v", test.message, err)
			}
		})
	}
}

// TestSpacePocketIdEncryptedArchive keeps archive keys in their own credential
// Secret, while both snapshot and WAL replication use the shared encrypted policy.
func TestSpacePocketIdEncryptedArchive(t *testing.T) {
	is := is.New(t)
	input := spacePocketIdTestInput(t)
	archive, err := sqliteworkload.NewArchiveKey()
	is.NoErr(err)
	input.ArchiveKey = archive
	input.BackupDestination = sqliteworkload.Destination{Endpoint: "http://rustfs:9000", Region: "us-east-1", Bucket: "space-archive", Prefix: "sqlite/space/provider", PathStyle: true, SyncInterval: "1s"}
	input.BackupCredentials = objectstore.Credentials{AccessKey: "archive-access", SecretKey: "archive-secret"}
	resources := spacePocketIdResources(input)
	is.Equal(len(resources), 7)
	workload := resources[4].(*appsv1.StatefulSet)
	is.Equal(len(workload.Spec.Template.Spec.InitContainers), 1)
	is.Equal(workload.Spec.Template.Spec.InitContainers[0].Image, sqliteworkload.Image)
	archiveSecret := resources[5].(*corev1.Secret)
	is.Equal(string(archiveSecret.Data["LITESTREAM_AGE_IDENTITY"]), archive.Identity)
	is.Equal(string(archiveSecret.Data["LITESTREAM_AGE_RECIPIENT"]), archive.Recipient)
	is.True(archiveSecret.Immutable == nil)
	configuration := resources[6].(*corev1.ConfigMap).Data["litestream.yml"]
	is.True(strings.Contains(configuration, "${LITESTREAM_AGE_IDENTITY}"))
	is.True(strings.Contains(configuration, "${LITESTREAM_AGE_RECIPIENT}"))
	is.True(!strings.Contains(configuration, archive.Identity))
	is.True(!strings.Contains(configuration, archive.Recipient))
	for _, container := range workload.Spec.Template.Spec.Containers {
		for _, variable := range container.Env {
			is.True(variable.Name != "LITESTREAM_AGE_IDENTITY" && variable.Name != "LITESTREAM_AGE_RECIPIENT")
		}
	}
}

// TestSpacePocketIdArchiveRebinding allows recovery to replace S3 coordinates and
// access credentials while the retained primary Secret fences archive-key changes.
func TestSpacePocketIdArchiveRebinding(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	input := spacePocketIdTestInput(t)
	archive, err := sqliteworkload.NewArchiveKey()
	is.NoErr(err)
	input.ArchiveKey = archive
	input.BackupDestination = sqliteworkload.Destination{Endpoint: "https://storage.example.test", Region: "us-east-1", Bucket: "original", Prefix: "sqlite/space/pocketid/id", PathStyle: true}
	input.BackupCredentials = objectstore.Credentials{AccessKey: "before-access", SecretKey: "before-secret"}
	original := spacePocketIdResources(input)
	original[0].SetUID(types.UID("original-secret"))
	original[1].SetUID(types.UID("original-volume"))
	original[0].GetAnnotations()["borq.io/pocket-id-pvc-uid"] = "original-volume"
	original[1].GetAnnotations()["borq.io/pocket-id-secret-uid"] = "original-secret"
	client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(original...).Build()
	input.BackupDestination.Bucket = "rehydrated"
	input.BackupDestination.Prefix = "recovered/sqlite/space/pocketid/id"
	input.BackupCredentials = objectstore.Credentials{AccessKey: "after-access", SecretKey: "after-secret"}
	desired := spacePocketIdResources(input)
	is.NoErr(checkSpacePocketIdStorage(ctx, client, input, desired))
	for _, object := range []ctrlclient.Object{desired[5], desired[6], desired[4]} {
		is.NoErr(ensureSpacePocketIdResource(ctx, client, object))
	}
	var retained corev1.Secret
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired[5]), &retained))
	is.Equal(string(retained.Data["AWS_SECRET_ACCESS_KEY"]), "after-secret")
	is.Equal(string(retained.Data["LITESTREAM_AGE_IDENTITY"]), archive.Identity)
	var workload appsv1.StatefulSet
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired[4]), &workload))
	is.True(workload.Spec.Template.Annotations["borq.io/pocket-id-archive-configuration"] != original[4].(*appsv1.StatefulSet).Spec.Template.Annotations["borq.io/pocket-id-archive-configuration"])
	is.NoErr(client.Delete(ctx, &retained))
	input.ArchiveKey, err = sqliteworkload.NewArchiveKey()
	is.NoErr(err)
	err = checkSpacePocketIdStorage(ctx, client, input, spacePocketIdResources(input))
	is.True(err != nil && strings.Contains(err.Error(), "credentials differ"))
}
