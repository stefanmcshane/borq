package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"go.opentelemetry.io/otel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// PocketIdInput identifies one organization's persistent identity provider.
// Credentials must come from the durable management vault before installation.
type PocketIdInput struct {
	// Connection selects the management cluster that owns this installation.
	Connection kubernetes.Connection
	// Namespace contains management services; empty uses borq-mgmt.
	Namespace string
	// InstanceID is the immutable, random 32-character lowercase hexadecimal identity.
	InstanceID string
	// IssuerURL is the browser-visible origin; it cannot change after installation.
	IssuerURL string
	// Tunnel uses cluster-local HTTP behind cloudflared while retaining the public HTTPS issuer URL.
	Tunnel bool
	// StorageClass selects local persistent storage; empty uses the cluster default.
	StorageClass string
	// EncryptionKey protects the Pocket ID database and must never be regenerated on retry.
	EncryptionKey string `json:"-"`
	// ApiKey authorizes Borq's private provisioning calls and stays in the management vault.
	ApiKey string `json:"-"`
	// BackupCredentials can access only this instance's SQLite archive in management RustFS.
	BackupCredentials objectstore.Credentials `json:"-"`
	// ArchiveKey encrypts SQLite archive objects independently from Pocket ID's application encryption key.
	ArchiveKey sqliteworkload.ArchiveKey `json:"-"`
	// ClusterIssuer selects the existing cert-manager issuer; empty uses borq.
	ClusterIssuer string
	// Timeout bounds installation readiness; empty uses three minutes.
	Timeout time.Duration
	// Values retains accepted legacy resource, scheduling and configuration overrides.
	Values map[string]any `json:"-"`
	// Environment contains the accepted secret-group snapshot, overriding legacy config values.
	Environment map[string]string `json:"-"`
	// Bootstrapping prevents directory imports and user signup until Borq has
	// established the initial administrator and its OIDC client.
	Bootstrapping bool
	// RequireExistingStorage prevents ready-instance recovery from creating a
	// fresh identity database after its original Secret or PVC has been lost.
	RequireExistingStorage bool
}

// PocketIdEndpoint identifies the private service used before publishing its HTTPRoute.
type PocketIdEndpoint struct {
	// Namespace contains the Pocket ID service.
	Namespace string
	// ServiceName is unique to this immutable instance.
	ServiceName string
	// Port is the service's HTTP port, reachable through the Kubernetes API proxy.
	Port int
}

// InstallPocketId starts one persistent Pocket ID instance without a public HTTPRoute.
// The caller creates its initial administrator through the private service before
// calling PublishPocketId, so an unauthenticated browser cannot claim the instance.
func InstallPocketId(ctx context.Context, input PocketIdInput) (PocketIdEndpoint, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.install_pocket_id")
	defer span.End()
	input, _, err := normalizePocketId(input)
	if err != nil {
		return PocketIdEndpoint{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	configuration := input.Connection.RestConfig()
	// The Kubernetes client owns shared transport state and requires pointers.
	client, err := ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	if err != nil {
		return PocketIdEndpoint{}, fmt.Errorf("connect to management cluster for Pocket ID: %w", err)
	}
	if err := checkPocketIdStorage(ctx, client, input); err != nil {
		return PocketIdEndpoint{}, err
	}
	objects, err := pocketIdPrivateResources(input)
	if err != nil {
		return PocketIdEndpoint{}, err
	}
	// Publish configuration before a new pod can read it. The content annotation
	// then restarts the workload exactly once with the requested environment.
	slices.SortStableFunc(objects, func(left, right ctrlclient.Object) int {
		order := func(object ctrlclient.Object) int {
			switch object.(type) {
			case *appsv1.Deployment, *corev1.Service:
				return 1
			default:
				return 0
			}
		}
		return order(left) - order(right)
	})
	for _, object := range objects {
		if err := ensurePocketIdResource(ctx, client, object); err != nil {
			return PocketIdEndpoint{}, err
		}
	}
	name := "pocketid-" + input.InstanceID
	if err := input.Connection.Deployments().WaitForRollout(ctx, input.Namespace, name); err != nil {
		return PocketIdEndpoint{}, fmt.Errorf("wait for Pocket ID: %w", err)
	}
	return PocketIdEndpoint{Namespace: input.Namespace, ServiceName: name, Port: 1411}, nil
}

// PublishPocketId exposes an initialized instance through the existing Traefik
// controller. The caller must establish the initial administrator first.
func PublishPocketId(ctx context.Context, input PocketIdInput) error {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.publish_pocket_id")
	defer span.End()
	input, address, err := normalizePocketId(input)
	if err != nil {
		return err
	}
	configuration := input.Connection.RestConfig()
	// The Kubernetes client owns shared transport state and requires pointers.
	client, err := ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	if err != nil {
		return fmt.Errorf("connect to management cluster for Pocket ID routing: %w", err)
	}
	if err := checkPocketIdRouteConflicts(ctx, client, input, address.Hostname()); err != nil {
		return err
	}
	secure := address.Scheme == "https" && !input.Tunnel
	if secure {
		if err := checkPocketIdCertificate(ctx, client, input); err != nil {
			return err
		}
	}
	if secure {
		if err := ensurePocketIdResource(ctx, client, pocketIdCertificate(input, address.Hostname())); err != nil {
			return err
		}
	}
	if err := ensurePocketIdResource(ctx, client, pocketIdGateway(input, address.Hostname(), secure)); err != nil {
		return err
	}
	if err := ensurePocketIdResource(ctx, client, pocketIdHttpRoute(input, address.Hostname())); err != nil {
		return err
	}
	if input.Tunnel {
		return retirePocketIdCertificate(ctx, client, input, address.Hostname())
	}
	return nil
}

// Stop renewal only for this instance's old origin Certificate after switching
// its Gateway to HTTP. Keep the TLS Secret; cert-manager may own its lifecycle.
func retirePocketIdCertificate(ctx context.Context, client ctrlclient.Client, input PocketIdInput, hostname string) error {
	desired := pocketIdCertificate(input, hostname)
	var current certmanagerv1.Certificate
	err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), &current)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkPocketIdCertificate(ctx, client, input); err != nil {
		return err
	}
	return client.Delete(ctx, &current, ctrlclient.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion})
}

func checkPocketIdRouteConflicts(ctx context.Context, client ctrlclient.Client, input PocketIdInput, hostname string) error {
	return checkPublicRouteConflicts(ctx, client, input.Namespace, "pocketid-"+input.InstanceID, hostname, "Pocket ID")
}

func checkPublicRouteConflicts(ctx context.Context, client ctrlclient.Client, namespace, name, hostname, service string) error {
	var routes gatewayv1.HTTPRouteList
	if err := client.List(ctx, &routes); err != nil {
		return fmt.Errorf("check %s address ownership: %w", service, err)
	}
	for _, route := range routes.Items {
		if route.Namespace == namespace && route.Name == name {
			continue
		}
		for _, parent := range route.Spec.ParentRefs {
			namespace := route.Namespace
			if parent.Namespace != nil {
				namespace = string(*parent.Namespace)
			}
			if parent.Kind != nil && *parent.Kind != "Gateway" || parent.Group != nil && *parent.Group != gatewayv1.GroupName {
				continue
			}
			var gateway gatewayv1.Gateway
			err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: string(parent.Name)}, &gateway)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if gateway.Spec.GatewayClassName != borqTraefikName {
				continue
			}
			if len(route.Spec.Hostnames) == 0 {
				return fmt.Errorf("an existing catch-all HTTPRoute conflicts with the %s address", service)
			}
			for _, host := range route.Spec.Hostnames {
				if pocketIdHostsOverlap(hostname, string(host)) {
					return fmt.Errorf("%s address is already routed by HTTPRoute %s/%s", service, route.Namespace, route.Name)
				}
			}
		}
	}
	return nil
}

func pocketIdCertificate(input PocketIdInput, hostname string) *certmanagerv1.Certificate {
	metadata := pocketIdMetadata(input)
	metadata.Name += "-tls"
	return &certmanagerv1.Certificate{ObjectMeta: metadata, Spec: certmanagerv1.CertificateSpec{
		SecretName: metadata.Name, DNSNames: []string{hostname}, IssuerRef: cmmeta.ObjectReference{Name: input.ClusterIssuer, Kind: "ClusterIssuer"},
		SecretTemplate: &certmanagerv1.CertificateSecretTemplate{Labels: metadata.Labels, Annotations: metadata.Annotations}, // cert-manager represents an optional Secret template with a pointer.
	}}
}

func checkPocketIdCertificate(ctx context.Context, client ctrlclient.Client, input PocketIdInput) error {
	name := "pocketid-" + input.InstanceID
	key := ctrlclient.ObjectKey{Namespace: input.Namespace, Name: name + "-tls"}
	var certificate certmanagerv1.Certificate
	err := client.Get(ctx, key, &certificate)
	certificateExists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read Pocket ID certificate ownership: %w", err)
	}
	if certificateExists {
		address, _ := url.Parse(input.IssuerURL)
		desired := pocketIdCertificate(input, address.Hostname())
		if err := checkPocketIdOwner(&certificate, desired); err != nil {
			return fmt.Errorf("Pocket ID certificate is not owned by this instance: %w", err)
		}
		if certificate.Spec.SecretName != key.Name || !slices.Equal(certificate.Spec.DNSNames, desired.Spec.DNSNames) || certificate.Spec.IssuerRef.Kind != "ClusterIssuer" {
			return errors.New("Pocket ID certificate is not owned by this instance")
		}
	}

	var secret corev1.Secret
	err = client.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Pocket ID TLS secret ownership: %w", err)
	}
	if secret.Type != corev1.SecretTypeTLS || secret.Annotations["cert-manager.io/certificate-name"] != key.Name ||
		secret.Annotations["cert-manager.io/issuer-kind"] != "ClusterIssuer" ||
		(secret.Annotations["cert-manager.io/issuer-group"] != "" && secret.Annotations["cert-manager.io/issuer-group"] != "cert-manager.io") {
		return errors.New("Pocket ID TLS Secret is not owned by this instance's certificate")
	}
	// Switching to tunnel exposure retires the Certificate but retains its Secret.
	// A later direct-mode setup may reuse only this instance's SecretTemplate identity.
	if !certificateExists {
		address, _ := url.Parse(input.IssuerURL)
		if secret.Annotations["cert-manager.io/issuer-name"] == "" || checkPocketIdOwner(&secret, pocketIdCertificate(input, address.Hostname())) != nil {
			return errors.New("Pocket ID TLS Secret is not owned by this instance's certificate")
		}
		return nil
	}
	if secret.Annotations["cert-manager.io/issuer-name"] == certificate.Spec.IssuerRef.Name {
		return nil
	}
	// During an issuer repair, Borq can update the Certificate before its
	// old Secret is replaced. Accept only an actual issuance owned by this same
	// Certificate, rather than requiring controllers to have already converged.
	var requests certmanagerv1.CertificateRequestList
	if err := client.List(ctx, &requests, ctrlclient.InNamespace(input.Namespace)); err != nil {
		return fmt.Errorf("read Pocket ID certificate issuance ownership: %w", err)
	}
	for _, request := range requests.Items {
		owner := metav1.GetControllerOf(&request)
		if owner != nil && owner.Kind == "Certificate" && owner.APIVersion == "cert-manager.io/v1" && owner.Name == certificate.Name && owner.UID == certificate.UID &&
			request.Spec.IssuerRef.Name == secret.Annotations["cert-manager.io/issuer-name"] && len(request.Status.Certificate) != 0 &&
			bytes.Equal(request.Status.Certificate, secret.Data[corev1.TLSCertKey]) {
			return nil
		}
	}
	return errors.New("Pocket ID TLS Secret does not match an issuance owned by this instance")
}

func pocketIdHostsOverlap(hostname, existing string) bool {
	if existing == "" || strings.EqualFold(hostname, existing) {
		return true
	}
	if !strings.HasPrefix(existing, "*.") {
		return false
	}
	_, suffix, found := strings.Cut(hostname, ".")
	return found && strings.EqualFold(suffix, strings.TrimPrefix(existing, "*."))
}

// A surviving workload or route is evidence of an earlier installation. Never
// silently replace missing identity data with an empty database or new keys.
func checkPocketIdStorage(ctx context.Context, client ctrlclient.Client, input PocketIdInput) error {
	objects, err := pocketIdPrivateResources(input)
	if err != nil {
		return err
	}
	address, _ := url.Parse(input.IssuerURL)
	objects = append(objects, pocketIdHttpRoute(input, address.Hostname()))
	secretExists, claimExists, deployed := false, false, false
	for _, desired := range objects {
		live := desired.DeepCopyObject().(ctrlclient.Object)
		err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Pocket ID storage ownership: %w", err)
		}
		if err := checkPocketIdOwner(live, desired); err != nil {
			return err
		}
		switch live.(type) {
		case *corev1.Secret:
			secretExists = secretExists || live.GetName() == "pocketid-"+input.InstanceID
		case *corev1.PersistentVolumeClaim:
			claimExists = true
		case *appsv1.Deployment, *corev1.Service, *gatewayv1.HTTPRoute, *gatewayv1.Gateway:
			deployed = true
		}
	}
	if (input.RequireExistingStorage || claimExists || deployed) && !secretExists {
		return errors.New("Pocket ID's original credentials Secret is missing; restore it before retrying")
	}
	if (input.RequireExistingStorage || deployed) && !claimExists {
		return errors.New("Pocket ID's original persistent volume claim is missing; restore its identity data before retrying")
	}
	return nil
}

func normalizePocketId(input PocketIdInput) (PocketIdInput, url.URL, error) {
	if input.Namespace == "" {
		input.Namespace = ManagementNamespace
	}
	if len(validation.IsDNS1123Label(input.Namespace)) != 0 {
		return input, url.URL{}, errors.New("Pocket ID management namespace is invalid")
	}
	identity, err := hex.DecodeString(input.InstanceID)
	if err != nil || len(identity) != 16 || strings.ToLower(input.InstanceID) != input.InstanceID {
		return input, url.URL{}, errors.New("Pocket ID requires an immutable 32-character lowercase hexadecimal instance identity")
	}
	address, err := url.Parse(input.IssuerURL)
	if err != nil || address == nil || address.Hostname() == "" || address.User != nil || address.Opaque != "" ||
		(address.Path != "" && address.Path != "/") || address.RawQuery != "" || address.ForceQuery ||
		strings.ContainsAny(input.IssuerURL, "#\\\r\n\t ") || net.ParseIP(address.Hostname()) != nil ||
		len(validation.IsDNS1123Subdomain(address.Hostname())) != 0 ||
		(address.Scheme != "https" && !(address.Scheme == "http" && address.Hostname() == "localhost")) {
		return input, url.URL{}, errors.New("Pocket ID address must be an HTTPS origin with a DNS hostname, or HTTP localhost for development")
	}
	if address.Port() != "" {
		port, err := strconv.Atoi(address.Port())
		if err != nil || port < 1 || port > 65535 {
			return input, url.URL{}, errors.New("Pocket ID address has an invalid port")
		}
	}
	input.IssuerURL = strings.TrimSuffix(input.IssuerURL, "/")
	if len(input.EncryptionKey) < 32 || len(input.ApiKey) < 32 {
		return input, url.URL{}, errors.New("Pocket ID encryption and API keys must each contain at least 32 characters")
	}
	if input.BackupCredentials.AccessKey == "" || input.BackupCredentials.SecretKey == "" || input.BackupCredentials.SessionToken != "" {
		return input, url.URL{}, errors.New("Pocket ID requires scoped, long-term management RustFS backup credentials")
	}
	if err := input.ArchiveKey.Validate(); err != nil {
		return input, url.URL{}, err
	}
	if input.ClusterIssuer == "" {
		input.ClusterIssuer = "borq"
	}
	if len(validation.IsDNS1123Subdomain(input.ClusterIssuer)) != 0 ||
		(input.StorageClass != "" && len(validation.IsDNS1123Subdomain(input.StorageClass)) != 0) {
		return input, url.URL{}, errors.New("Pocket ID certificate issuer or storage class is invalid")
	}
	if input.Timeout <= 0 {
		input.Timeout = 3 * time.Minute
	}
	return input, *address, nil
}

func pocketIdMetadata(input PocketIdInput) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: "pocketid-" + input.InstanceID, Namespace: input.Namespace,
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/name": "pocket-id", "borq.io/pocket-id": input.InstanceID},
		Annotations: map[string]string{"borq.io/pocket-id-issuer": input.IssuerURL},
	}
}

// PocketIdManagedEnvironment describes Borq's runtime settings without generated
// encryption or API credentials. The renderer uses the same literal settings.
func PocketIdManagedEnvironment(issuerURL string) map[string]string {
	environment := map[string]string{}
	for _, variable := range pocketIdManagedVariables(issuerURL) {
		environment[variable.Name] = variable.Value
	}
	return environment
}

func pocketIdManagedVariables(issuerURL string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "HOST", Value: "127.0.0.1"}, {Name: "PORT", Value: "1412"},
		{Name: "APP_URL", Value: issuerURL}, {Name: "DB_CONNECTION_STRING", Value: "/app/data/pocket-id.db"},
		{Name: "UPLOAD_PATH", Value: "/app/data/uploads"}, {Name: "UI_CONFIG_DISABLED", Value: "true"},
		{Name: "AUTO_CREATE_OIDC_CLIENT_SECRET", Value: "false"}, {Name: "ALLOW_INSECURE_CALLBACK_URLS", Value: "false"},
		{Name: "ANALYTICS_DISABLED", Value: "true"}, {Name: "VERSION_CHECK_DISABLED", Value: "true"},
	}
}

func pocketIdDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}
}

// Kubernetes uses pointers to distinguish omitted scalar settings and API objects.
func pocketIdManagedResources(input PocketIdInput) []ctrlclient.Object {
	metadata := pocketIdMetadata(input)
	immutable, enabled, disabled := true, true, false
	replicas, identity := int32(1), int64(65532)
	filesystemPolicy := corev1.FSGroupChangeOnRootMismatch
	secret := &corev1.Secret{
		ObjectMeta: *metadata.DeepCopy(), Immutable: &immutable, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"encryption-key": []byte(input.EncryptionKey), "api-key": []byte(input.ApiKey)},
	}
	if input.ArchiveKey.Recipient != "" {
		// Bind archive identity to the retained primary Secret even if the runtime
		// archive Secret is later lost and reconstructed from OpenBao.
		digest := sha256.Sum256([]byte(input.ArchiveKey.Recipient))
		secret.Data["archive-recipient-sha256"] = []byte(hex.EncodeToString(digest[:]))
	}
	proxy := &corev1.ConfigMap{ObjectMeta: *metadata.DeepCopy(), Immutable: &immutable,
		Data: map[string]string{"nginx.conf": pocketIdProxyConfig()},
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: *metadata.DeepCopy(), Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}}
	if input.StorageClass != "" {
		claim.Spec.StorageClassName = &input.StorageClass
	}
	deployment := &appsv1.Deployment{ObjectMeta: *metadata.DeepCopy(), Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		Selector: &metav1.LabelSelector{MatchLabels: metadata.Labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: metadata.Labels}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &disabled,
			InitContainers:               []corev1.Container{pocketIdBackupSidecar(input)},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &enabled, RunAsUser: &identity, RunAsGroup: &identity,
				FSGroup: &identity, FSGroupChangePolicy: &filesystemPolicy, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "pocket-id",
				Image: "ghcr.io/pocket-id/pocket-id:v2.19.1@sha256:f085157f0b6ea4b2e8aa5ecfd99eada0d83a4960f3a3dcfd46014097d0ef7ab5",
				// Upstream request/error logs include raw enrollment paths, queries
				// and Referer tokens. Keep its database audit trail, not those streams.
				Command: []string{"/bin/sh", "-ec", "exec /app/pocket-id >/dev/null 2>&1"},
				Env: append(pocketIdManagedVariables(input.IssuerURL), []corev1.EnvVar{
					{Name: "ENCRYPTION_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: metadata.Name}, Key: "encryption-key"}}},
					{Name: "STATIC_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: metadata.Name}, Key: "api-key"}}},
				}...),
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &disabled, ReadOnlyRootFilesystem: &enabled, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Resources:       pocketIdDefaultResources(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/app/data"}, {Name: "proxy-tmp", MountPath: "/tmp"}},
				StartupProbe:    &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(1411)}}, PeriodSeconds: 2, TimeoutSeconds: 2, FailureThreshold: 60},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(1411)}}, PeriodSeconds: 5, TimeoutSeconds: 2},
				LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(1411)}}, PeriodSeconds: 15, TimeoutSeconds: 2},
			}, {
				Name:    "setup-guard",
				Image:   "nginx/nginx-unprivileged:1.30.6-alpine-slim@sha256:984a21e29bb10d4c0e122374b97b89a2101823a9331e0f3c6c04bc285ccf447f",
				Command: []string{"nginx"}, Args: []string{"-c", "/etc/borq/nginx.conf", "-g", "daemon off;"},
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 1411}},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &disabled, ReadOnlyRootFilesystem: &enabled, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}},
				VolumeMounts: []corev1.VolumeMount{{Name: "setup-guard", MountPath: "/etc/borq", ReadOnly: true}, {Name: "proxy-tmp", MountPath: "/tmp"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: metadata.Name}}},
				{Name: "setup-guard", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: metadata.Name}}}},
				{Name: "backup-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: metadata.Name + "-backup"}}}},
				{Name: "proxy-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(64*1024*1024, resource.BinarySI)}}},
			},
		}},
	}}
	service := &corev1.Service{ObjectMeta: *metadata.DeepCopy(), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP,
		Selector: metadata.Labels, Ports: []corev1.ServicePort{{Name: "http", Port: 1411, TargetPort: intstr.FromInt32(1411)}},
	}}
	return append([]ctrlclient.Object{secret, claim, deployment, service, proxy}, pocketIdBackupResources(input)...)
}

func pocketIdProxyConfig() string {
	// Pocket ID has no setting to disable first-admin signup. Its listener is
	// pod-local, and every network request passes this permanent setup guard.
	// Request/error logging is disabled because enrollment URLs carry credentials.
	return `worker_processes 1;
pid /tmp/nginx.pid;
error_log /dev/null;
events { worker_connections 1024; }
http {
    access_log off;
    server_tokens off;
    client_body_temp_path /tmp/client_temp;
    proxy_temp_path /tmp/proxy_temp;
    fastcgi_temp_path /tmp/fastcgi_temp;
    uwsgi_temp_path /tmp/uwsgi_temp;
    scgi_temp_path /tmp/scgi_temp;
    map $request_uri $ambiguous_path {
        default 0;
        ~*^[^?]*(%2f|%5c|%2e|%25) 1;
        ~^[^?]*// 1;
        ~^[^?]*/\.\.?(/|\?|$) 1;
    }
    server {
        listen 1411;
        client_max_body_size 20m;
        if ($ambiguous_path) { return 404; }
        location ~* ^/api/signup/setup { return 404; }
        location / {
            proxy_pass http://127.0.0.1:1412;
            proxy_http_version 1.1;
            proxy_set_header Connection "";
            proxy_set_header Host $http_host;
            proxy_set_header X-Forwarded-Host $http_host;
            proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        }
    }
}
`
}

func ensurePocketIdResource(ctx context.Context, client ctrlclient.Client, desired ctrlclient.Object) error {
	live := desired.DeepCopyObject().(ctrlclient.Object)
	err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live)
	if apierrors.IsNotFound(err) {
		// Atomic create fails if another actor claims the name after our read.
		if err := client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create Pocket ID %T %s: %w", desired, desired.GetName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Pocket ID resource %s: %w", desired.GetName(), err)
	}
	if err := checkPocketIdOwner(live, desired); err != nil {
		return err
	}
	switch wanted := desired.(type) {
	case *corev1.ConfigMap:
		current := live.(*corev1.ConfigMap)
		if current.Immutable == nil || !*current.Immutable || !maps.Equal(current.Data, wanted.Data) || len(current.BinaryData) != 0 {
			return errors.New("Pocket ID configuration is not the original immutable configuration")
		}
		return nil
	case *corev1.Secret:
		current := live.(*corev1.Secret)
		if strings.HasSuffix(wanted.Name, "-config") {
			if current.Immutable != nil && *current.Immutable {
				return errors.New("Pocket ID environment Secret must permit managed updates")
			}
			if equality.Semantic.DeepEqual(current.Data, wanted.Data) {
				return nil
			}
			current.Data = wanted.Data
			break
		}
		if current.Immutable == nil || !*current.Immutable || len(current.Data) != len(wanted.Data) || current.Type != wanted.Type {
			return errors.New("Pocket ID credentials are not the original immutable Secret")
		}
		for key, value := range wanted.Data {
			if !bytes.Equal(current.Data[key], value) {
				return errors.New("Pocket ID credentials differ from the original installation; refusing to replace its encryption key")
			}
		}
		return nil
	case *corev1.PersistentVolumeClaim:
		current := live.(*corev1.PersistentVolumeClaim)
		if !equality.Semantic.DeepDerivative(wanted.Spec, current.Spec) {
			return errors.New("Pocket ID persistent storage differs from the original installation")
		}
		return nil
	case *appsv1.Deployment:
		current := live.(*appsv1.Deployment)
		if current.Annotations["borq.io/pocket-id-definition"] == wanted.Annotations["borq.io/pocket-id-definition"] &&
			equality.Semantic.DeepDerivative(wanted.Spec, current.Spec) && pocketIdSamePodBoundary(wanted.Spec.Template.Spec, current.Spec.Template.Spec) {
			return nil
		}
		current.Spec = wanted.Spec
		maps.Copy(current.Annotations, wanted.Annotations)
	case *corev1.Service:
		current := live.(*corev1.Service)
		if current.Spec.Type != corev1.ServiceTypeClusterIP || len(current.Spec.ExternalIPs) != 0 || current.Spec.ExternalName != "" {
			return errors.New("Pocket ID service must remain private")
		}
		if equality.Semantic.DeepDerivative(wanted.Spec, current.Spec) {
			return nil
		}
		current.Spec.Selector, current.Spec.Ports = wanted.Spec.Selector, wanted.Spec.Ports
	case *certmanagerv1.Certificate:
		current := live.(*certmanagerv1.Certificate)
		if equality.Semantic.DeepEqual(wanted.Spec, current.Spec) {
			return nil
		}
		current.Spec = wanted.Spec
	case *gatewayv1.Gateway:
		current := live.(*gatewayv1.Gateway)
		if equality.Semantic.DeepDerivative(wanted.Spec, current.Spec) {
			return nil
		}
		current.Spec = wanted.Spec
	case *gatewayv1.HTTPRoute:
		current := live.(*gatewayv1.HTTPRoute)
		if equality.Semantic.DeepEqual(wanted.Spec, current.Spec) && equality.Semantic.DeepEqual(wanted.Annotations, current.Annotations) {
			return nil
		}
		current.Spec, current.Annotations = wanted.Spec, wanted.Annotations
	default:
		return errors.New("unsupported Pocket ID resource")
	}
	// Updating the object read above includes its resourceVersion. A concurrent
	// ownership change conflicts instead of force-taking another actor's object.
	if err := client.Update(ctx, live); err != nil {
		return fmt.Errorf("update Pocket ID resource %s: %w", desired.GetName(), err)
	}
	return nil
}

// DeepDerivative allows extra slice entries and treats omitted fields as wildcards.
// Those rules must not admit another container, credential source or listener.
func pocketIdSamePodBoundary(wanted, current corev1.PodSpec) bool {
	if len(wanted.Containers) != len(current.Containers) || len(wanted.InitContainers) != len(current.InitContainers) ||
		len(wanted.Volumes) != len(current.Volumes) || len(current.EphemeralContainers) != 0 ||
		wanted.ServiceAccountName != current.ServiceAccountName || !equality.Semantic.DeepEqual(wanted.SecurityContext, current.SecurityContext) {
		return false
	}
	wantedContainers := append(slices.Clone(wanted.Containers), wanted.InitContainers...)
	currentContainers := append(slices.Clone(current.Containers), current.InitContainers...)
	for index, expected := range wantedContainers {
		actual := currentContainers[index]
		if !equality.Semantic.DeepEqual(expected.Env, actual.Env) || !equality.Semantic.DeepEqual(expected.EnvFrom, actual.EnvFrom) ||
			!equality.Semantic.DeepEqual(expected.Command, actual.Command) || !equality.Semantic.DeepEqual(expected.Args, actual.Args) ||
			!equality.Semantic.DeepEqual(expected.SecurityContext, actual.SecurityContext) ||
			!equality.Semantic.DeepEqual(expected.VolumeMounts, actual.VolumeMounts) || len(actual.VolumeDevices) != 0 ||
			len(expected.Ports) != len(actual.Ports) || expected.WorkingDir != actual.WorkingDir {
			return false
		}
	}
	return true
}

func checkPocketIdOwner(live, desired ctrlclient.Object) error {
	for key, value := range desired.GetLabels() {
		if live.GetLabels()[key] != value {
			return fmt.Errorf("Pocket ID resource %s is not owned by this instance", desired.GetName())
		}
	}
	if live.GetAnnotations()["borq.io/pocket-id-issuer"] != desired.GetAnnotations()["borq.io/pocket-id-issuer"] ||
		len(live.GetOwnerReferences()) != 0 || live.GetDeletionTimestamp() != nil {
		return fmt.Errorf("Pocket ID resource %s has a different issuer, owner, or pending deletion", desired.GetName())
	}
	return nil
}
