package platform

import (
	"encoding/json"

	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func pocketIdBackupResources(input PocketIdInput) []ctrlclient.Object {
	metadata := pocketIdMetadata(input)
	metadata.Name += "-backup"
	immutable := true
	secret := &corev1.Secret{ObjectMeta: *metadata.DeepCopy(), Immutable: &immutable, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte(input.BackupCredentials.AccessKey), "AWS_SECRET_ACCESS_KEY": []byte(input.BackupCredentials.SecretKey), "LITESTREAM_AGE_IDENTITY": []byte(input.ArchiveKey.Identity), "LITESTREAM_AGE_RECIPIENT": []byte(input.ArchiveKey.Recipient)},
	}
	// These fixed coordinates never follow organization space storage or external S3 overrides.
	configuration := sqliteworkload.Config("/app/data/pocket-id.db", sqliteworkload.Destination{
		Endpoint: "http://rustfs-svc." + input.Namespace + ".svc:9000", Region: "us-east-1",
		Bucket: "borq-pocketid-" + input.InstanceID, Prefix: "sqlite", PathStyle: true, SyncInterval: "1s",
	})
	// JSON is valid YAML; this structure contains only scalar values and cannot fail encoding.
	encoded, _ := json.Marshal(configuration)
	config := &corev1.ConfigMap{ObjectMeta: *metadata.DeepCopy(), Immutable: &immutable, Data: map[string]string{"litestream.yml": string(encoded)}}
	return []ctrlclient.Object{secret, config}
}

func pocketIdBackupSidecar(input PocketIdInput) corev1.Container {
	name := "pocketid-" + input.InstanceID + "-backup"
	enabled, disabled := true, false
	container := sqliteworkload.ReplicationContainer(sqliteworkload.ReplicationInput{
		Name: "sqlite-backup", Directory: "/app/data", DataVolume: "data", ConfigVolume: "backup-config", CredentialSecret: name,
	})
	// Keep archive keys out of the identity-provider container and its environment.
	container.EnvFrom = nil
	container.Env = []corev1.EnvVar{
		{Name: "AWS_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "AWS_ACCESS_KEY_ID"}}},
		{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "AWS_SECRET_ACCESS_KEY"}}},
		{Name: "LITESTREAM_AGE_IDENTITY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "LITESTREAM_AGE_IDENTITY"}}},
		{Name: "LITESTREAM_AGE_RECIPIENT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "LITESTREAM_AGE_RECIPIENT"}}},
		{Name: "AWS_EC2_METADATA_DISABLED", Value: "true"},
	}
	container.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: &disabled, ReadOnlyRootFilesystem: &enabled, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	container.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}}
	return container
}
