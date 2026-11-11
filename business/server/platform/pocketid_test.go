package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestPocketIdResources owns the rendered storage, credential, container and
// routing contracts; actual readiness and persistence belong to the cluster test.
func TestPocketIdResources(t *testing.T) {
	is := is.New(t)
	input, address, err := normalizePocketId(pocketIdTestInput(t))
	is.NoErr(err)
	resources, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	is.Equal(len(resources), 7)
	var deployment appsv1.Deployment
	for _, object := range resources {
		is.Equal(object.GetNamespace(), "borq-mgmt")
		is.Equal(object.GetLabels()["borq.io/pocket-id"], input.InstanceID)
		is.Equal(len(object.GetOwnerReferences()), 0) // Deleting a deployment must not garbage-collect identity data.
		switch object := object.(type) {
		case *corev1.Secret:
			is.True(*object.Immutable)
			if strings.HasSuffix(object.Name, "-backup") {
				is.Equal(len(object.Data), 4)
				is.Equal(string(object.Data["AWS_ACCESS_KEY_ID"]), input.BackupCredentials.AccessKey)
				is.Equal(string(object.Data["AWS_SECRET_ACCESS_KEY"]), input.BackupCredentials.SecretKey)
				is.Equal(string(object.Data["LITESTREAM_AGE_IDENTITY"]), input.ArchiveKey.Identity)
				is.Equal(string(object.Data["LITESTREAM_AGE_RECIPIENT"]), input.ArchiveKey.Recipient)
			} else {
				is.Equal(string(object.Data["encryption-key"]), input.EncryptionKey)
				is.Equal(string(object.Data["api-key"]), input.ApiKey)
			}
		case *corev1.PersistentVolumeClaim:
			is.Equal(object.Spec.Resources.Requests.Storage().String(), "1Gi")
			is.Equal(object.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce})
			is.Equal(*object.Spec.StorageClassName, "local-path")
		case *appsv1.Deployment:
			deployment = *object
		case *corev1.Service:
			is.Equal(object.Spec.Type, corev1.ServiceTypeClusterIP)
			is.Equal(object.Spec.Ports[0].Port, int32(1411))
		case *corev1.ConfigMap:
			is.True(*object.Immutable)
			if strings.HasSuffix(object.Name, "-backup") {
				var configuration struct {
					Databases []struct {
						Path     string           `json:"path"`
						Replicas []map[string]any `json:"replicas"`
					} `json:"dbs"`
				}
				is.NoErr(json.Unmarshal([]byte(object.Data["litestream.yml"]), &configuration))
				is.Equal(configuration.Databases[0].Path, "/app/data/pocket-id.db")
				is.Equal(configuration.Databases[0].Replicas[0], map[string]any{"type": "s3", "endpoint": "http://rustfs-svc.borq-mgmt.svc:9000", "region": "us-east-1", "bucket": "borq-pocketid-" + input.InstanceID, "path": "sqlite", "force-path-style": true, "retention": "168h", "snapshot-interval": "24h", "sync-interval": "1s", "age": map[string]any{"identities": []any{"${LITESTREAM_AGE_IDENTITY}"}, "recipients": []any{"${LITESTREAM_AGE_RECIPIENT}"}}})
				is.True(!strings.Contains(object.Data["litestream.yml"], input.BackupCredentials.SecretKey))
			}
		default:
			t.Fatalf("unexpected private resource %T", object)
		}
	}
	is.Equal(*deployment.Spec.Replicas, int32(1))
	is.Equal(deployment.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	pod := deployment.Spec.Template.Spec
	is.Equal(*pod.SecurityContext.RunAsUser, int64(65532))
	is.Equal(*pod.SecurityContext.FSGroup, int64(65532))
	is.True(!*pod.AutomountServiceAccountToken)
	is.True(*pod.SecurityContext.RunAsNonRoot)
	is.Equal(len(pod.InitContainers), 1)
	backup := pod.InitContainers[0]
	is.Equal(*backup.RestartPolicy, corev1.ContainerRestartPolicyAlways)
	is.True(strings.HasPrefix(backup.Image, "litestream/litestream:0.3.14@sha256:"))
	is.True(*backup.SecurityContext.ReadOnlyRootFilesystem)
	is.True(!*backup.SecurityContext.AllowPrivilegeEscalation)
	is.Equal(backup.VolumeMounts[0].Name, "data")
	is.Equal(backup.VolumeMounts[0].MountPath, "/app/data")
	is.Equal(backup.Env[0].ValueFrom.SecretKeyRef.Name, "pocketid-"+input.InstanceID+"-backup")
	container := pod.Containers[0]
	is.True(strings.HasPrefix(container.Image, "ghcr.io/pocket-id/pocket-id:v2.19.1@sha256:"))
	is.True(*container.SecurityContext.ReadOnlyRootFilesystem)
	is.True(!*container.SecurityContext.AllowPrivilegeEscalation)
	is.Equal(container.VolumeMounts[0].MountPath, "/app/data")
	is.Equal(container.ReadinessProbe.HTTPGet.Path, "/healthz")
	is.Equal(container.ReadinessProbe.HTTPGet.Port.IntVal, int32(1411))
	environment := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		environment[variable.Name] = variable
	}
	is.Equal(environment["APP_URL"].Value, "https://id.example.test")
	is.Equal(environment["HOST"].Value, "127.0.0.1")
	is.Equal(environment["PORT"].Value, "1412")
	is.Equal(len(container.Ports), 0)
	is.Equal(pod.Containers[1].Ports[0].ContainerPort, int32(1411))
	is.Equal(environment["DB_CONNECTION_STRING"].Value, "/app/data/pocket-id.db")
	is.Equal(environment["UPLOAD_PATH"].Value, "/app/data/uploads")
	is.Equal(environment["UI_CONFIG_DISABLED"].Value, "true")
	is.Equal(environment["AUTO_CREATE_OIDC_CLIENT_SECRET"].Value, "false")
	is.Equal(environment["ALLOW_INSECURE_CALLBACK_URLS"].Value, "false")
	is.Equal(environment["ENCRYPTION_KEY"].Value, "")
	is.Equal(environment["ENCRYPTION_KEY"].ValueFrom.SecretKeyRef.Key, "encryption-key")
	is.Equal(environment["STATIC_API_KEY"].Value, "")
	is.Equal(environment["STATIC_API_KEY"].ValueFrom.SecretKeyRef.Key, "api-key")
	route := pocketIdHttpRoute(input, address.Hostname())
	is.Equal(route.Spec.Hostnames, []gatewayv1.Hostname{"id.example.test"})
	is.Equal(*route.Spec.Rules[0].BackendRefs[0].Port, gatewayv1.PortNumber(1411))
	is.Equal(string(route.Spec.ParentRefs[0].Name), route.Name)
	gateway := pocketIdGateway(input, address.Hostname(), true)
	is.Equal(gateway.Spec.GatewayClassName, gatewayv1.ObjectName(borqTraefikName))
	is.Equal(gateway.Spec.Listeners[0].Protocol, gatewayv1.HTTPSProtocolType)
	is.Equal(string(gateway.Spec.Listeners[0].TLS.CertificateRefs[0].Name), "pocketid-"+input.InstanceID+"-tls")
	certificate := pocketIdCertificate(input, address.Hostname())
	is.Equal(certificate.Spec.DNSNames, []string{"id.example.test"})
	is.Equal(certificate.Spec.IssuerRef.Name, "borq")
	local := pocketIdGateway(input, "localhost", false)
	is.True(local.Spec.Listeners[0].TLS == nil)
	is.Equal(local.Spec.Listeners[0].Protocol, gatewayv1.HTTPProtocolType)

}

// TestPocketIdValidation rejects malformed immutable inputs before any cluster write.
func TestPocketIdValidation(t *testing.T) {
	for _, address := range []string{"http://example.test", "https://id.example.test/path", "https://id.example.test?", "https://id.example.test#", "https://name:password@id.example.test", "http://127.0.0.1", "https://id.example.test:70000", "https://*.example.test", "https://id.example.test "} {
		t.Run(address, func(t *testing.T) {
			is := is.New(t)
			input := pocketIdTestInput(t)
			input.IssuerURL = address
			_, _, err := normalizePocketId(input)
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "address"))
		})
	}
	for _, change := range []struct {
		name    string
		modify  func(*PocketIdInput)
		message string
	}{
		{"identity", func(input *PocketIdInput) { input.InstanceID = "../another" }, "identity"},
		{"namespace", func(input *PocketIdInput) { input.Namespace = "A_B" }, "namespace"},
		{"encryption key", func(input *PocketIdInput) { input.EncryptionKey = "short" }, "keys"},
		{"API key", func(input *PocketIdInput) { input.ApiKey = "short" }, "keys"},
		{"backup credentials", func(input *PocketIdInput) { input.BackupCredentials = objectstore.Credentials{} }, "backup credentials"},
		{"temporary backup credentials", func(input *PocketIdInput) { input.BackupCredentials.SessionToken = "temporary" }, "backup credentials"},
		{"issuer", func(input *PocketIdInput) { input.ClusterIssuer = "A_B" }, "issuer"},
		{"storage class", func(input *PocketIdInput) { input.StorageClass = "A_B" }, "storage class"},
	} {
		t.Run(change.name, func(t *testing.T) {
			is := is.New(t)
			input := pocketIdTestInput(t)
			change.modify(&input)
			_, _, err := normalizePocketId(input)
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), change.message))
		})
	}
	is := is.New(t)
	input := pocketIdTestInput(t)
	input.IssuerURL = "http://localhost:1411/"
	normalized, _, err := normalizePocketId(input)
	is.NoErr(err)
	is.Equal(normalized.IssuerURL, "http://localhost:1411")
}

// TestPocketIdResourceOwnership owns rejection of foreign objects and immutable
// data drift. The Kubernetes fake proves API decisions, never controller success.
func TestPocketIdResourceOwnership(t *testing.T) {
	input, _, err := normalizePocketId(pocketIdTestInput(t))
	is.New(t).NoErr(err)
	for _, test := range []struct {
		name    string
		index   int
		modify  func(ctrlclient.Object)
		message string
	}{
		{"unowned", 0, func(object ctrlclient.Object) { object.SetLabels(nil) }, "not owned"},
		{"other instance", 2, func(object ctrlclient.Object) { object.GetLabels()["borq.io/pocket-id"] = strings.Repeat("b", 32) }, "not owned"},
		{"other issuer", 2, func(object ctrlclient.Object) {
			object.GetAnnotations()["borq.io/pocket-id-issuer"] = "https://other.example.test"
		}, "different issuer"},
		{"owner reference", 1, func(object ctrlclient.Object) { object.SetOwnerReferences([]metav1.OwnerReference{{Name: "foreign"}}) }, "owner"},
		{"different key", 0, func(object ctrlclient.Object) { object.(*corev1.Secret).Data["encryption-key"] = []byte("different") }, "credentials differ"},
		{"mutable secret", 0, func(object ctrlclient.Object) { object.(*corev1.Secret).Immutable = nil }, "immutable Secret"},
		{"mutable setup guard", 4, func(object ctrlclient.Object) { object.(*corev1.ConfigMap).Immutable = nil }, "immutable configuration"},
		{"backup credential drift", 5, func(object ctrlclient.Object) {
			object.(*corev1.Secret).Data["AWS_SECRET_ACCESS_KEY"] = []byte("different")
		}, "credentials differ"},
		{"backup destination drift", 6, func(object ctrlclient.Object) { object.(*corev1.ConfigMap).Data["litestream.yml"] = "dbs: []" }, "immutable configuration"},
		{"storage class", 1, func(object ctrlclient.Object) {
			class := "different"
			object.(*corev1.PersistentVolumeClaim).Spec.StorageClassName = &class
		}, "persistent storage"},
		{"public service", 3, func(object ctrlclient.Object) { object.(*corev1.Service).Spec.Type = corev1.ServiceTypeLoadBalancer }, "remain private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			resources, err := pocketIdPrivateResources(input)
			is.NoErr(err)
			desired := resources[test.index]
			foreign := desired.DeepCopyObject().(ctrlclient.Object)
			test.modify(foreign)
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(foreign).Build()
			err = ensurePocketIdResource(context.Background(), client, desired)
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), test.message))
			stored := foreign.DeepCopyObject().(ctrlclient.Object)
			is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(foreign), stored))
			is.Equal(stored, foreign)
		})
	}
}

// TestPocketIdIngressHostOverlap protects dedicated identity hostnames from
// catch-all, wildcard and exact routes while leaving unrelated hosts available.
func TestPocketIdIngressHostOverlap(t *testing.T) {
	is := is.New(t)
	for _, existing := range []string{"", "id.example.test", "ID.EXAMPLE.TEST", "*.example.test"} {
		is.True(pocketIdHostsOverlap("id.example.test", existing))
	}
	for _, existing := range []string{"other.example.test", "*.test", "*.sub.example.test"} {
		is.True(!pocketIdHostsOverlap("id.example.test", existing))
	}
}

// TestPocketIdRouteConflicts rejects competing hosts only on Borq's GatewayClass.
func TestPocketIdRouteConflicts(t *testing.T) {
	for _, test := range []struct {
		name, class, host string
		conflict          bool
	}{
		{"exact", borqTraefikName, "id.example.test", true}, {"wildcard", borqTraefikName, "*.example.test", true},
		{"catchall", borqTraefikName, "", true}, {"other host", borqTraefikName, "other.example.test", false}, {"other controller", "tailscale", "id.example.test", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			input, _, err := normalizePocketId(pocketIdTestInput(t))
			assert.NoErr(err)
			gateway := pocketIdGateway(input, "id.example.test", true)
			gateway.Name = "existing"
			gateway.Spec.GatewayClassName = gatewayv1.ObjectName(test.class)
			route := pocketIdHttpRoute(input, test.host)
			route.Name = "existing"
			route.Spec.ParentRefs[0].Name = "existing"
			if test.host == "" {
				route.Spec.Hostnames = nil
			}
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(gateway, route).Build()
			err = checkPocketIdRouteConflicts(t.Context(), client, input, "id.example.test")
			assert.Equal(err != nil, test.conflict)
		})
	}
}

// TestPocketIdMissingStorageRecovery rejects empty identity replacements after
// installation while permitting retries interrupted before the initial PVC write.
func TestPocketIdMissingStorageRecovery(t *testing.T) {
	input, address, err := normalizePocketId(pocketIdTestInput(t))
	is.New(t).NoErr(err)
	for _, test := range []struct {
		name            string
		indexes         []int
		ingress         bool
		requireExisting bool
		foreignSecret   bool
		message         string
	}{
		{name: "new installation"},
		{name: "interrupted before PVC", indexes: []int{0}},
		{name: "complete storage", indexes: []int{0, 1}},
		{name: "ready instance lost all runtime resources", requireExisting: true, message: "credentials Secret is missing"},
		{name: "ready instance lost credentials only", indexes: []int{1}, requireExisting: true, message: "credentials Secret is missing"},
		{name: "ready instance lost PVC only", indexes: []int{0}, requireExisting: true, message: "persistent volume claim is missing"},
		{name: "ready instance retained storage without workloads", indexes: []int{0, 1}, requireExisting: true},
		{name: "ready instance foreign credentials", indexes: []int{0, 1}, requireExisting: true, foreignSecret: true, message: "not owned by this instance"},
		{name: "missing secret with PVC", indexes: []int{1}, message: "credentials Secret is missing"},
		{name: "backup secret cannot replace encryption key", indexes: []int{1, 5}, message: "credentials Secret is missing"},
		{name: "missing PVC with deployment", indexes: []int{0, 2}, message: "persistent volume claim is missing"},
		{name: "missing PVC with ingress", indexes: []int{0}, ingress: true, message: "persistent volume claim is missing"},
		{name: "missing secret with ingress", indexes: []int{1}, ingress: true, message: "credentials Secret is missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			input := input
			input.RequireExistingStorage = test.requireExisting
			resources, err := pocketIdPrivateResources(input)
			is.NoErr(err)
			if test.foreignSecret {
				resources[0].SetLabels(map[string]string{"borq.io/pocket-id": "other-instance"})
			}
			var objects []ctrlclient.Object
			for _, index := range test.indexes {
				objects = append(objects, resources[index])
			}
			if test.ingress {
				objects = append(objects, pocketIdHttpRoute(input, address.Hostname()))
			}
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(objects...).Build()
			err = checkPocketIdStorage(context.Background(), client, input)
			if test.message == "" {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), test.message))
		})
	}
}

// TestPocketIdCertificateOwnership prevents adopting TLS material
// that is not owned by this managed identity instance.
func TestPocketIdCertificateOwnership(t *testing.T) {
	input, address, err := normalizePocketId(pocketIdTestInput(t))
	is.New(t).NoErr(err)
	for _, test := range []struct {
		name    string
		object  ctrlclient.Object
		message string
	}{
		{"foreign secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: input.Namespace, Name: "pocketid-" + input.InstanceID + "-tls"}, Type: corev1.SecretTypeTLS}, "TLS Secret is not owned"},
		{"foreign certificate", &certmanagerv1.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: input.Namespace, Name: "pocketid-" + input.InstanceID + "-tls"}}, "not owned by this instance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(test.object).Build()
			err := checkPocketIdCertificate(context.Background(), client, input)
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), test.message))
		})
	}
	for _, test := range []struct {
		name              string
		certificateIssuer string
		requestOwner      string
		accept            bool
	}{
		{"previous issuer before reconciliation", "previous", "certificate-owner", true},
		{"previous secret during reconciliation", "borq", "certificate-owner", true},
		{"foreign issuance during reconciliation", "borq", "foreign-owner", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			is := is.New(t)
			certificate := *pocketIdCertificate(input, address.Hostname())
			certificate.UID = "certificate-owner"
			controller := true
			certificate.Spec.IssuerRef.Name = test.certificateIssuer

			secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: certificate.Name, Namespace: input.Namespace,
				Annotations: map[string]string{"cert-manager.io/certificate-name": certificate.Name, "cert-manager.io/issuer-name": "previous", "cert-manager.io/issuer-kind": "ClusterIssuer"}},
				Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: []byte("previous-issued-certificate")}}
			request := certmanagerv1.CertificateRequest{ObjectMeta: metav1.ObjectMeta{Name: certificate.Name + "-1", Namespace: input.Namespace,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: certificate.Name, UID: types.UID(test.requestOwner), Controller: &controller}},
			}}
			request.Spec.IssuerRef.Name = "previous"
			request.Status.Certificate = []byte("previous-issued-certificate")
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(&certificate, &secret, &request).Build()
			err := checkPocketIdCertificate(context.Background(), client, input)
			if test.accept {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "does not match an issuance owned"))
		})
	}
}

func pocketIdTestInput(t *testing.T) PocketIdInput {
	t.Helper()
	archive, err := sqliteworkload.NewArchiveKey()
	is.New(t).NoErr(err)
	return PocketIdInput{ArchiveKey: archive, InstanceID: strings.Repeat("a", 32), IssuerURL: "https://id.example.test", StorageClass: "local-path",
		EncryptionKey: strings.Repeat("e", 32), ApiKey: strings.Repeat("k", 32), BackupCredentials: objectstore.Credentials{AccessKey: "instance-backup-key", SecretKey: strings.Repeat("s", 40)}}
}

// These are ownership fixtures only; no certificate issuance is simulated.
func TestPocketIdRetainedTunnelSecret(t *testing.T) {
	input, address, err := normalizePocketId(pocketIdTestInput(t))
	is.New(t).NoErr(err)
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			assert := is.New(t)
			desired := pocketIdCertificate(input, address.Hostname())
			metadata := desired.ObjectMeta
			metadata.Annotations["cert-manager.io/certificate-name"] = desired.Name
			metadata.Annotations["cert-manager.io/issuer-kind"] = "ClusterIssuer"
			metadata.Annotations["cert-manager.io/issuer-name"] = input.ClusterIssuer
			if foreign {
				metadata.Labels["borq.io/pocket-id"] = "another-instance"
			}
			secret := &corev1.Secret{ObjectMeta: metadata, Type: corev1.SecretTypeTLS}
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(secret).Build()
			err := checkPocketIdCertificate(t.Context(), client, input)
			assert.Equal(err != nil, foreign)
		})
	}
}
