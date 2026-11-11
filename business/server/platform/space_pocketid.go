package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"go.opentelemetry.io/otel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// SpacePocketIdInput identifies a space's retained SQLite identity provider.
type SpacePocketIdInput struct {
	// Connection selects the space's target cluster.
	Connection kubernetes.Connection
	// Namespace is the existing namespace assigned to the space.
	Namespace string
	// ID is the immutable 32-character lowercase hexadecimal addon identity.
	ID string
	// IssuerURL is the permanent browser-visible HTTPS origin.
	IssuerURL string
	// CertificateIssuer selects an existing cert-manager ClusterIssuer.
	CertificateIssuer string
	// Storage is the initial SQLite volume capacity; empty uses 1Gi.
	Storage string
	// StorageClass selects persistent local storage; empty uses the cluster default.
	StorageClass string
	// Image optionally selects a digest-pinned official Pocket ID image.
	Image string
	// EncryptionKey must be durably recorded before the first installation.
	EncryptionKey string `json:"-"`
	// ApiKey authorizes private administrator provisioning.
	ApiKey string `json:"-"`
	// ArchiveKey encrypts the workload's SQLite backups and remains stable in OpenBao.
	ArchiveKey sqliteworkload.ArchiveKey `json:"-"`
	// BackupDestination selects the owning space's scoped archive; empty disables replication.
	BackupDestination sqliteworkload.Destination
	// BackupCredentials access only this provider's archive in that destination.
	BackupCredentials objectstore.Credentials `json:"-"`
	// RequireExistingStorage forbids creating fresh storage during recovery.
	RequireExistingStorage bool
	// Timeout bounds workload readiness; empty uses three minutes.
	Timeout time.Duration
}

// InstallSpacePocketId starts a private single-writer StatefulSet, retaining its
// database and encryption key across upgrades and removal. RollingUpdate stops
// ordinal zero before replacing it; SQLite therefore has a brief service gap.
func InstallSpacePocketId(ctx context.Context, input SpacePocketIdInput) (PocketIdEndpoint, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.install_space_pocket_id")
	defer span.End()
	input, _, err := normalizeSpacePocketId(input)
	if err != nil {
		return PocketIdEndpoint{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	client, err := networkClient(input.Connection)
	if err != nil {
		return PocketIdEndpoint{}, err
	}
	resources := spacePocketIdResources(input)
	if err := checkSpacePocketIdStorage(ctx, client, input, resources); err != nil {
		return PocketIdEndpoint{}, err
	}
	for _, object := range resources[:2] {
		if err := ensurePocketIdResource(ctx, client, object); err != nil {
			return PocketIdEndpoint{}, err
		}
	}
	if err := bindSpacePocketIdStorage(ctx, client, input); err != nil {
		return PocketIdEndpoint{}, err
	}
	remaining := slices.Clone(resources[2:])
	slices.SortStableFunc(remaining, func(left, right ctrlclient.Object) int {
		order := func(object ctrlclient.Object) int {
			switch object.(type) {
			case *appsv1.StatefulSet, *corev1.Service:
				return 1
			default:
				return 0
			}
		}
		return order(left) - order(right)
	})
	for _, object := range remaining {
		if err := ensureSpacePocketIdResource(ctx, client, object); err != nil {
			return PocketIdEndpoint{}, err
		}
	}
	name := "pocketid-" + input.ID
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		var current appsv1.StatefulSet
		if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: input.Namespace, Name: name}, &current); err != nil {
			return false, err
		}
		return current.Status.ObservedGeneration >= current.Generation && current.Status.ReadyReplicas == 1 && current.Status.UpdatedReplicas == 1 && current.Status.CurrentRevision == current.Status.UpdateRevision, nil
	}); err != nil {
		return PocketIdEndpoint{}, fmt.Errorf("wait for space Pocket ID rollover: %w", err)
	}
	return PocketIdEndpoint{Namespace: input.Namespace, ServiceName: name, Port: 1411}, nil
}

// PublishSpacePocketId exposes the provider only after the caller has established
// its first administrator through the private service.
func PublishSpacePocketId(ctx context.Context, input SpacePocketIdInput) error {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.publish_space_pocket_id")
	defer span.End()
	input, address, err := normalizeSpacePocketId(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	client, err := networkClient(input.Connection)
	if err != nil {
		return err
	}
	input.RequireExistingStorage = true
	if err := checkSpacePocketIdStorage(ctx, client, input, spacePocketIdResources(input)); err != nil {
		return err
	}
	legacy := spacePocketIdRoutingInput(input)
	if err := checkPocketIdRouteConflicts(ctx, client, legacy, address.Hostname()); err != nil {
		return err
	}
	if address.Scheme == "https" {
		if err := checkPocketIdCertificate(ctx, client, legacy); err != nil {
			return err
		}
	}
	routing := spacePocketIdRoutingResources(input, address)
	slices.Reverse(routing)
	for _, object := range routing {
		if err := ensureSpacePocketIdResource(ctx, client, object); err != nil {
			return err
		}
	}
	key := ctrlclient.ObjectKey{Namespace: input.Namespace, Name: "pocketid-" + input.ID}
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if address.Scheme == "https" {
			var certificate certmanagerv1.Certificate
			if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: key.Namespace, Name: key.Name + "-tls"}, &certificate); err != nil {
				return false, err
			}
			ready := slices.ContainsFunc(certificate.Status.Conditions, func(condition certmanagerv1.CertificateCondition) bool {
				return condition.Type == certmanagerv1.CertificateConditionReady && condition.Status == "True" && condition.ObservedGeneration == certificate.Generation
			})
			if !ready {
				return false, nil
			}
		}
		var gateway gatewayv1.Gateway
		if err := client.Get(ctx, key, &gateway); err != nil {
			return false, err
		}
		if !spacePocketIdCondition(gateway.Status.Conditions, "Accepted", gateway.Generation) {
			return false, nil
		}
		var route gatewayv1.HTTPRoute
		if err := client.Get(ctx, key, &route); err != nil {
			return false, err
		}
		for _, parent := range route.Status.Parents {
			if parent.ControllerName != "traefik.io/gateway-controller" || string(parent.ParentRef.Name) != key.Name ||
				(parent.ParentRef.Namespace != nil && string(*parent.ParentRef.Namespace) != key.Namespace) {
				continue
			}
			if spacePocketIdCondition(parent.Conditions, "Accepted", route.Generation) && spacePocketIdCondition(parent.Conditions, "ResolvedRefs", route.Generation) {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("wait for space Pocket ID certificate and route readiness: %w", err)
	}
	return nil
}

// RemoveSpacePocketId removes traffic and waits for the database writer to stop.
// The explicitly owned PVC and immutable credentials Secret remain for recovery.
func RemoveSpacePocketId(ctx context.Context, input SpacePocketIdInput) error {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.remove_space_pocket_id")
	defer span.End()
	input, address, err := normalizeSpacePocketId(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	client, err := networkClient(input.Connection)
	if err != nil {
		return err
	}
	resources := spacePocketIdResources(input)
	objects := append(spacePocketIdRoutingResources(input, address), resources[2:]...)
	if input.BackupDestination.Bucket == "" {
		metadata := pocketIdMetadata(spacePocketIdRoutingInput(input))
		metadata.Name += "-backup"
		configuration := &corev1.ConfigMap{ObjectMeta: metadata}
		spacePocketIdOwner(configuration, input)
		objects = append(objects, configuration)
	}
	objects = slices.DeleteFunc(objects, func(object ctrlclient.Object) bool { _, secret := object.(*corev1.Secret); return secret })
	// Validate every ownership boundary before removing anything.
	for _, desired := range objects {
		live := desired.DeepCopyObject().(ctrlclient.Object)
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := checkPocketIdOwner(live, desired); err != nil {
			return err
		}
	}
	// Revoke the route first. Background StatefulSet deletion keeps its pods
	// visible until termination, which the wait below observes before returning.
	for _, desired := range objects {
		live := desired.DeepCopyObject().(ctrlclient.Object)
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := checkPocketIdOwner(live, desired); err != nil {
			return err
		}
		uid, version := live.GetUID(), live.GetResourceVersion()
		if err := client.Delete(ctx, live, ctrlclient.PropagationPolicy(metav1.DeletePropagationBackground), ctrlclient.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := client.List(ctx, &pods, ctrlclient.InNamespace(input.Namespace), ctrlclient.MatchingLabels{"borq.io/space-pocket-id": input.ID}); err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})
}

func normalizeSpacePocketId(input SpacePocketIdInput) (SpacePocketIdInput, url.URL, error) {
	if input.Namespace == "" || len(validation.IsDNS1123Label(input.Namespace)) != 0 {
		return input, url.URL{}, errors.New("space Pocket ID namespace is invalid")
	}
	identity, err := hex.DecodeString(input.ID)
	if err != nil || len(identity) != 16 || strings.ToLower(input.ID) != input.ID {
		return input, url.URL{}, errors.New("space Pocket ID requires an immutable 32-character lowercase hexadecimal identity")
	}
	address, err := url.Parse(input.IssuerURL)
	if err != nil || address == nil || address.Hostname() == "" || address.User != nil || address.Opaque != "" ||
		(address.Path != "" && address.Path != "/") || address.RawQuery != "" || address.ForceQuery || strings.ContainsAny(input.IssuerURL, "#\\\r\n\t ") ||
		net.ParseIP(address.Hostname()) != nil || len(validation.IsDNS1123Subdomain(address.Hostname())) != 0 ||
		(address.Scheme != "https" && !(address.Scheme == "http" && address.Hostname() == "localhost")) {
		return input, url.URL{}, errors.New("space Pocket ID address must be an HTTPS origin, or HTTP localhost for development")
	}
	if address.Port() != "" {
		port, err := strconv.Atoi(address.Port())
		if err != nil || port < 1 || port > 65535 {
			return input, url.URL{}, errors.New("space Pocket ID address has an invalid port")
		}
	}
	input.IssuerURL = strings.TrimSuffix(input.IssuerURL, "/")
	if len(input.EncryptionKey) < 32 || len(input.ApiKey) < 32 {
		return input, url.URL{}, errors.New("space Pocket ID encryption and API keys must each contain at least 32 characters")
	}
	if input.BackupDestination.Bucket != "" {
		if err := input.ArchiveKey.Validate(); err != nil {
			return input, url.URL{}, err
		}
		if input.BackupCredentials.AccessKey == "" || input.BackupCredentials.SecretKey == "" || input.BackupCredentials.SessionToken != "" {
			return input, url.URL{}, errors.New("space Pocket ID backup credentials are missing")
		}
	}
	if input.CertificateIssuer == "" {
		input.CertificateIssuer = "borq"
	}
	if len(validation.IsDNS1123Subdomain(input.CertificateIssuer)) != 0 || (input.StorageClass != "" && len(validation.IsDNS1123Subdomain(input.StorageClass)) != 0) {
		return input, url.URL{}, errors.New("space Pocket ID certificate issuer or storage class is invalid")
	}
	if input.Storage == "" {
		input.Storage = "1Gi"
	}
	capacity, err := resource.ParseQuantity(input.Storage)
	if err != nil || capacity.Sign() <= 0 {
		return input, url.URL{}, errors.New("space Pocket ID storage must be a positive Kubernetes capacity")
	}
	if input.Image != "" {
		reference, digest, ok := strings.Cut(input.Image, "@sha256:")
		decoded, err := hex.DecodeString(digest)
		if !ok || err != nil || len(decoded) != 32 || !strings.HasPrefix(reference, "ghcr.io/pocket-id/pocket-id:") || strings.ContainsAny(reference, "\r\n\t ") {
			return input, url.URL{}, errors.New("space Pocket ID image must pin an official Pocket ID image by SHA-256 digest")
		}
	}
	if input.Timeout <= 0 {
		input.Timeout = 3 * time.Minute
	}
	return input, *address, nil
}

func spacePocketIdRoutingInput(input SpacePocketIdInput) PocketIdInput {
	return PocketIdInput{Connection: input.Connection, Namespace: input.Namespace, InstanceID: input.ID, IssuerURL: input.IssuerURL, ClusterIssuer: input.CertificateIssuer, ArchiveKey: input.ArchiveKey, BackupCredentials: input.BackupCredentials, StorageClass: input.StorageClass, EncryptionKey: input.EncryptionKey, ApiKey: input.ApiKey}
}

func spacePocketIdResources(input SpacePocketIdInput) []ctrlclient.Object {
	// Reuse the permanent setup guard and restricted upstream container. Space
	// providers own local SQLite storage; management backup credentials do not apply.
	base := pocketIdManagedResources(spacePocketIdRoutingInput(input))
	secret := base[0].(*corev1.Secret)
	claim := base[1].(*corev1.PersistentVolumeClaim)
	claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(input.Storage)
	deployment := base[2].(*appsv1.Deployment)
	template := deployment.Spec.Template
	template.Spec.InitContainers = nil
	template.Spec.Volumes = slices.DeleteFunc(template.Spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == "backup-config" })
	template.Spec.Containers[0].Env = append(template.Spec.Containers[0].Env, corev1.EnvVar{Name: "ALLOW_USER_SIGNUPS", Value: "disabled"}, corev1.EnvVar{Name: "LDAP_ENABLED", Value: "false"})
	if input.Image != "" {
		template.Spec.Containers[0].Image = input.Image
	}
	replicas := int32(1)
	workload := &appsv1.StatefulSet{ObjectMeta: deployment.ObjectMeta, Spec: appsv1.StatefulSetSpec{
		Replicas: &replicas, ServiceName: deployment.Name, Selector: deployment.Spec.Selector, Template: template,
		PodManagementPolicy: appsv1.OrderedReadyPodManagement, UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
	}}
	objects := []ctrlclient.Object{secret, claim, base[4], base[3], workload}
	if input.BackupDestination.Bucket != "" {
		template.Spec.InitContainers = []corev1.Container{pocketIdBackupSidecar(spacePocketIdRoutingInput(input))}
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{Name: "backup-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: deployment.Name + "-backup"}}}})
		workload.Spec.Template = template
		backup := pocketIdBackupResources(spacePocketIdRoutingInput(input))
		// Space recovery may rebind S3 credentials while retaining the same archive identity.
		backup[0].(*corev1.Secret).Immutable = nil
		backup[1].(*corev1.ConfigMap).Immutable = nil
		credentials, _ := json.Marshal(backup[0].(*corev1.Secret).Data)
		encoded, _ := json.Marshal(sqliteworkload.Config("/app/data/pocket-id.db", input.BackupDestination))
		backup[1].(*corev1.ConfigMap).Data["litestream.yml"] = string(encoded)
		// Secrets and Litestream configuration are captured only when a new pod starts.
		material := append(credentials, encoded...)
		workload.Spec.Template.Annotations = map[string]string{"borq.io/pocket-id-archive-configuration": fmt.Sprintf("%x", sha256.Sum256(material))}
		objects = append(objects, backup...)
	}
	for _, object := range objects {
		spacePocketIdOwner(object, input)
	}
	workload.Spec.Selector.MatchLabels = maps.Clone(workload.Labels)
	workload.Spec.Template.Labels = maps.Clone(workload.Labels)
	base[3].(*corev1.Service).Spec.Selector = maps.Clone(workload.Labels)
	return objects
}

func spacePocketIdOwner(object ctrlclient.Object, input SpacePocketIdInput) {
	labels := maps.Clone(object.GetLabels())
	labels["borq.io/space-pocket-id"] = input.ID
	object.SetLabels(labels)
}

func spacePocketIdRoutingResources(input SpacePocketIdInput, address url.URL) []ctrlclient.Object {
	legacy := spacePocketIdRoutingInput(input)
	// Route is first so removal revokes public traffic before stopping the writer.
	objects := []ctrlclient.Object{pocketIdHttpRoute(legacy, address.Hostname()), pocketIdGateway(legacy, address.Hostname(), address.Scheme == "https")}
	if address.Scheme == "https" {
		objects = append(objects, pocketIdCertificate(legacy, address.Hostname()))
	}
	for _, object := range objects {
		spacePocketIdOwner(object, input)
		if certificate, ok := object.(*certmanagerv1.Certificate); ok {
			certificate.Spec.SecretTemplate.Labels = maps.Clone(certificate.Labels)
		}
	}
	return objects
}

func checkSpacePocketIdStorage(ctx context.Context, client ctrlclient.Client, input SpacePocketIdInput, resources []ctrlclient.Object) error {
	key := ctrlclient.ObjectKey{Namespace: input.Namespace, Name: "pocketid-" + input.ID}
	var deployment appsv1.Deployment
	if err := client.Get(ctx, key, &deployment); err == nil {
		return errors.New("space Pocket ID refuses a concurrent Deployment writer")
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	address, _ := url.Parse(input.IssuerURL)
	objects := append(slices.Clone(resources), spacePocketIdRoutingResources(input, *address)...)
	var secret corev1.Secret
	var claim corev1.PersistentVolumeClaim
	secretExists, claimExists, active := false, false, false
	var workloadUid string
	for _, desired := range objects {
		live := desired.DeepCopyObject().(ctrlclient.Object)
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := checkPocketIdOwner(live, desired); err != nil {
			return err
		}
		switch current := live.(type) {
		case *corev1.Secret:
			wanted := desired.(*corev1.Secret)
			if current.Name == key.Name {
				secret, secretExists = *current, true
				if current.Immutable == nil || !*current.Immutable || current.Type != wanted.Type || !equality.Semantic.DeepEqual(current.Data, wanted.Data) {
					return errors.New("space Pocket ID credentials differ from the original immutable Secret")
				}
			} else if current.Type != wanted.Type || !bytes.Equal(current.Data["LITESTREAM_AGE_IDENTITY"], wanted.Data["LITESTREAM_AGE_IDENTITY"]) || !bytes.Equal(current.Data["LITESTREAM_AGE_RECIPIENT"], wanted.Data["LITESTREAM_AGE_RECIPIENT"]) {
				return errors.New("space Pocket ID archive encryption identity differs from the original vault entry")
			}
		case *corev1.PersistentVolumeClaim:
			claim, claimExists = *current, true
			if !equality.Semantic.DeepDerivative(desired.(*corev1.PersistentVolumeClaim).Spec, current.Spec) {
				return errors.New("space Pocket ID persistent storage differs from the original installation")
			}
		case *appsv1.StatefulSet:
			active = true
			workloadUid = string(current.UID)
			if current.Spec.Replicas == nil || *current.Spec.Replicas != 1 || len(current.Spec.VolumeClaimTemplates) != 0 || current.Spec.PodManagementPolicy != appsv1.OrderedReadyPodManagement || current.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
				return errors.New("space Pocket ID requires one ordered StatefulSet writer")
			}
			wanted := desired.(*appsv1.StatefulSet)
			if current.Spec.ServiceName != wanted.Spec.ServiceName || !equality.Semantic.DeepEqual(current.Spec.Selector, wanted.Spec.Selector) {
				return errors.New("space Pocket ID StatefulSet ownership boundary was changed")
			}
			if rolling := current.Spec.UpdateStrategy.RollingUpdate; rolling != nil && ((rolling.Partition != nil && *rolling.Partition != 0) || (rolling.MaxUnavailable != nil && rolling.MaxUnavailable.String() != "1")) {
				return errors.New("space Pocket ID requires an unpartitioned single-writer rollover")
			}
			if !spacePocketIdPodBoundary(wanted.Spec.Template.Spec, current.Spec.Template.Spec) {
				return errors.New("space Pocket ID workload runtime boundary was changed")
			}
		case *corev1.Service, *corev1.ConfigMap, *gatewayv1.HTTPRoute, *gatewayv1.Gateway, *certmanagerv1.Certificate:
			active = true
		}
	}
	if (input.RequireExistingStorage || claimExists || active) && !secretExists {
		return errors.New("space Pocket ID original credentials Secret is missing; restore it before retrying")
	}
	if (input.RequireExistingStorage || active || secret.Annotations["borq.io/pocket-id-pvc-uid"] != "") && !claimExists {
		return errors.New("space Pocket ID original persistent volume claim is missing; restore its identity data before retrying")
	}
	if secretExists && claimExists {
		if secret.Annotations["borq.io/pocket-id-pvc-uid"] != "" && secret.Annotations["borq.io/pocket-id-pvc-uid"] != string(claim.UID) {
			return errors.New("space Pocket ID persistent volume claim was replaced; restore the original identity volume")
		}
		if claim.Annotations["borq.io/pocket-id-secret-uid"] != "" && claim.Annotations["borq.io/pocket-id-secret-uid"] != string(secret.UID) {
			return errors.New("space Pocket ID credentials Secret was replaced; restore the original credentials")
		}
		if (active || input.RequireExistingStorage) && (secret.Annotations["borq.io/pocket-id-pvc-uid"] == "" || claim.Annotations["borq.io/pocket-id-secret-uid"] == "") {
			return errors.New("space Pocket ID original storage identity binding is missing")
		}
	}
	var pods corev1.PodList
	if err := client.List(ctx, &pods, ctrlclient.InNamespace(input.Namespace)); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != key.Name {
				continue
			}
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.Kind != "StatefulSet" || owner.Name != key.Name || string(owner.UID) != workloadUid || pod.Name != key.Name+"-0" || pod.Labels["borq.io/space-pocket-id"] != input.ID {
				return errors.New("space Pocket ID storage is mounted by an unexpected writer")
			}
		}
	}
	return nil
}

func bindSpacePocketIdStorage(ctx context.Context, client ctrlclient.Client, input SpacePocketIdInput) error {
	key := ctrlclient.ObjectKey{Namespace: input.Namespace, Name: "pocketid-" + input.ID}
	var secret corev1.Secret
	var claim corev1.PersistentVolumeClaim
	if err := client.Get(ctx, key, &secret); err != nil {
		return err
	}
	if err := client.Get(ctx, key, &claim); err != nil {
		return err
	}
	for _, binding := range []struct {
		object   ctrlclient.Object
		key, uid string
	}{
		{&secret, "borq.io/pocket-id-pvc-uid", string(claim.UID)}, {&claim, "borq.io/pocket-id-secret-uid", string(secret.UID)},
	} {
		annotations := binding.object.GetAnnotations()
		if annotations[binding.key] == binding.uid {
			continue
		}
		if annotations[binding.key] != "" {
			return errors.New("space Pocket ID original storage binding changed")
		}
		annotations[binding.key] = binding.uid
		binding.object.SetAnnotations(annotations)
		if err := client.Update(ctx, binding.object); err != nil {
			return fmt.Errorf("bind space Pocket ID retained storage: %w", err)
		}
	}
	return nil
}

func spacePocketIdPodBoundary(wanted, current corev1.PodSpec) bool {
	return pocketIdSamePodBoundary(wanted, current) && wanted.HostNetwork == current.HostNetwork && wanted.HostPID == current.HostPID && wanted.HostIPC == current.HostIPC &&
		equality.Semantic.DeepEqual(wanted.ShareProcessNamespace, current.ShareProcessNamespace) && equality.Semantic.DeepDerivative(wanted.Volumes, current.Volumes) &&
		equality.Semantic.DeepEqual(wanted.AutomountServiceAccountToken, current.AutomountServiceAccountToken)
}

func ensureSpacePocketIdResource(ctx context.Context, client ctrlclient.Client, desired ctrlclient.Object) error {
	if wanted, ok := desired.(*corev1.ConfigMap); ok && strings.HasSuffix(wanted.Name, "-backup") {
		var current corev1.ConfigMap
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(wanted), &current); apierrors.IsNotFound(err) {
			return client.Create(ctx, wanted)
		} else if err != nil {
			return err
		}
		if err := checkPocketIdOwner(&current, wanted); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(current.Data, wanted.Data) {
			return nil
		}
		current.Data = wanted.Data
		return client.Update(ctx, &current)
	}
	if wanted, ok := desired.(*corev1.Secret); ok && strings.HasSuffix(wanted.Name, "-backup") {
		var current corev1.Secret
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(wanted), &current); apierrors.IsNotFound(err) {
			return client.Create(ctx, wanted)
		} else if err != nil {
			return err
		}
		if err := checkPocketIdOwner(&current, wanted); err != nil {
			return err
		}
		if current.Type != wanted.Type || !bytes.Equal(current.Data["LITESTREAM_AGE_IDENTITY"], wanted.Data["LITESTREAM_AGE_IDENTITY"]) || !bytes.Equal(current.Data["LITESTREAM_AGE_RECIPIENT"], wanted.Data["LITESTREAM_AGE_RECIPIENT"]) {
			return errors.New("space Pocket ID archive encryption identity differs from the original vault entry")
		}
		if equality.Semantic.DeepEqual(current.Data, wanted.Data) {
			return nil
		}
		current.Data = wanted.Data
		return client.Update(ctx, &current)
	}
	wanted, ok := desired.(*appsv1.StatefulSet)
	if !ok {
		return ensurePocketIdResource(ctx, client, desired)
	}
	var current appsv1.StatefulSet
	if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(wanted), &current); apierrors.IsNotFound(err) {
		return client.Create(ctx, wanted)
	} else if err != nil {
		return err
	}
	if err := checkPocketIdOwner(&current, wanted); err != nil {
		return err
	}
	if equality.Semantic.DeepDerivative(wanted.Spec, current.Spec) {
		return nil
	}
	current.Spec.Template = wanted.Spec.Template
	current.Spec.UpdateStrategy = wanted.Spec.UpdateStrategy
	return client.Update(ctx, &current)
}

func spacePocketIdCondition(conditions []metav1.Condition, kind string, generation int64) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == kind && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
	})
}
