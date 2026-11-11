package management

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const borqDataClaimName = "borq-data"

// runManagementBootstrapWorkflow installs only the resources needed to run Borq.
// Management charts are reconciled by the running server, never by this CLI.
func runManagementBootstrapWorkflow(ctx context.Context, log logger.Logger, request Request, connection kubernetes.Connection) error {
	if err := rejectLegacyBootstrap(ctx, connection, request.Namespace); err != nil {
		return err
	}
	if err := checkManagementMode(ctx, connection, request); err != nil {
		return err
	}
	if err := connection.Namespaces().Apply(ctx, managementNamespace(request.Namespace)); err != nil {
		return fmt.Errorf("apply management namespace: %w", err)
	}
	if request.StorageClass == "borq-local" {
		if err := connection.Objects().Apply(ctx, localStorageClass()); err != nil {
			return fmt.Errorf("apply Borq local disk storage class: %w", err)
		}
	}
	claim, err := connection.PersistentVolumeClaims().Get(ctx, request.Namespace, borqDataClaimName)
	if err == nil {
		if claim.Labels["app.kubernetes.io/managed-by"] != "borq" {
			return fmt.Errorf("management state volume is owned outside Borq")
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read Borq state volume: %w", err)
	} else if err := connection.PersistentVolumeClaims().Apply(ctx, kubernetes.PersistentVolumeClaim{
		Name: borqDataClaimName, Namespace: request.Namespace, Labels: appLabels(borqName, "state"),
		AccessModes: []kubernetes.PersistentVolumeAccessMode{"ReadWriteOnce"}, RequestedStorage: "1Gi", StorageClassName: request.StorageClass,
	}); err != nil {
		return fmt.Errorf("apply Borq state volume: %w", err)
	}
	if err := connection.ServiceAccounts().Apply(ctx, kubernetes.ServiceAccount{
		Name: borqServiceAccountName, Namespace: request.Namespace, Labels: appLabels(borqName, "rbac"),
	}); err != nil {
		return err
	}
	if err := connection.ClusterRoles().Apply(ctx, borqClusterRole()); err != nil {
		return err
	}
	if err := connection.ClusterRoleBindings().Apply(ctx, kubernetes.ClusterRoleBinding{
		Name: borqRoleBindingName, Labels: appLabels(borqName, "rbac"),
		RoleRef:  kubernetes.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: borqRoleName},
		Subjects: []kubernetes.Subject{{Kind: "ServiceAccount", Name: borqServiceAccountName, Namespace: request.Namespace}},
	}); err != nil {
		return err
	}
	for _, service := range bootstrapServices(request) {
		existing, err := connection.Services().Get(ctx, service.Namespace, service.Name)
		if err == nil {
			if existing.Labels["app.kubernetes.io/managed-by"] != "borq" {
				return fmt.Errorf("management Service is owned outside Borq")
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read management Service: %w", err)
		}
		if err := connection.Services().Apply(ctx, service); err != nil {
			return err
		}
	}
	deployment := borqDeployment(request)
	live, err := connection.Objects().Get(ctx, deployment)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read application manager server: %w", err)
	}
	if err == nil {
		if err := retainInstalledServer(deployment, live.(*appsv1.Deployment)); err != nil {
			return err
		}
	}
	if err := connection.Objects().Apply(ctx, deployment); err != nil {
		return fmt.Errorf("apply application manager server: %w", err)
	}
	log.Info("waiting_for_management_server")
	return waitForManagementServer(ctx, log, request, connection)
}

func bootstrapServices(request Request) []kubernetes.Service {
	return []kubernetes.Service{{
		Name: borqName, Namespace: request.Namespace, Labels: appLabels(borqName, "server"),
		Selector: selectorLabels(borqName), Type: kubernetes.ClusterIPService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: borqContainerPort}},
	}}
}

// Preserve installation settings while updating the image and required authentication wiring.
func retainInstalledServer(deployment, installed *appsv1.Deployment) error {
	if installed.Labels["app.kubernetes.io/managed-by"] != "borq" {
		return fmt.Errorf("management server Deployment is owned outside Borq")
	}
	image := deployment.Spec.Template.Spec.Containers[0].Image
	pullPolicy := deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy
	deployment.Spec.Template = *installed.Spec.Template.DeepCopy()
	for index := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[index]
		if container.Name != borqName {
			continue
		}
		container.Image, container.ImagePullPolicy = image, pullPolicy
		arguments := []string{}
		authenticationArgument := "--admin-token-file=/var/run/borq/admin-auth/token"
		for index := 0; index < len(container.Args); index++ {
			argument := container.Args[index]
			if argument == "--admin-token-file" {
				index++ // The separate-value form consumes the following argument.
			}
			if argument == "--admin-token-file" || strings.HasPrefix(argument, "--admin-token-file=") {
				if !slices.Contains(arguments, authenticationArgument) {
					arguments = append(arguments, authenticationArgument)
				}
				continue
			}
			arguments = append(arguments, argument)
		}
		if !slices.Contains(arguments, authenticationArgument) {
			arguments = append(arguments, authenticationArgument)
		}
		container.Args = arguments
		mount := corev1.VolumeMount{Name: "admin-auth", MountPath: "/var/run/borq/admin-auth", ReadOnly: true}
		container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, func(existing corev1.VolumeMount) bool {
			return existing.Name == mount.Name || existing.MountPath == mount.MountPath
		})
		container.VolumeMounts = append(container.VolumeMounts, mount)
		volumes := slices.DeleteFunc(deployment.Spec.Template.Spec.Volumes, func(existing corev1.Volume) bool { return existing.Name == "admin-auth" })
		deployment.Spec.Template.Spec.Volumes = append(volumes, corev1.Volume{Name: "admin-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "borq-admin-auth"}}})
		if !slices.Contains(container.Args, "--management-bootstrap-only") {
			container.Args = append(container.Args, "--management-bootstrap-only")
		}
		return nil
	}
	return fmt.Errorf("installed management Deployment has no Borq server container")
}

func checkManagementMode(ctx context.Context, connection kubernetes.Connection, request Request) error {
	stored, err := connection.ConfigMaps().Get(ctx, request.Namespace, "borq-platform-management")
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read management profile before bootstrap: %w", err)
	}
	var previous struct {
		Mode string `json:"profile"`
	}
	if err := json.Unmarshal([]byte(stored.Data["state.json"]), &previous); err != nil {
		return fmt.Errorf("read management profile before bootstrap: invalid saved state")
	}
	if previous.Mode == "production" {
		previous.Mode = "prod"
	}
	if previous.Mode != request.Mode {
		return fmt.Errorf("management component mode change from %q to %q requires a separate installation or data migration", previous.Mode, request.Mode)
	}
	return nil
}

// Kubernetes object APIs require pointers for mutable resource metadata.
func localStorageClass() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": "borq-local", "labels": map[string]any{"app.kubernetes.io/managed-by": "borq"}},
		"provisioner": "rancher.io/local-path", "reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer",
	}}
}

// rejectLegacyBootstrap leaves old repositories and volumes intact instead of
// racing their reconcilers. Migrating that installation is a separate operation.
func rejectLegacyBootstrap(ctx context.Context, connection kubernetes.Connection, namespace string) error {
	// The old default namespace may hold a running server or only retained state.
	// Refuse before changing cluster-wide RBAC or creating a replacement volume.
	if _, err := connection.Deployments().Get(ctx, "borq", borqName); !apierrors.IsNotFound(err) {
		return legacyManagementResourceError("Deployment", borqName, err)
	}
	if _, err := connection.PersistentVolumeClaims().Get(ctx, "borq", borqDataClaimName); !apierrors.IsNotFound(err) {
		return legacyManagementResourceError("PersistentVolumeClaim", borqDataClaimName, err)
	}
	if _, err := connection.ConfigMaps().Get(ctx, "borq", "borq-platform-management"); !apierrors.IsNotFound(err) {
		return legacyManagementResourceError("ConfigMap", "borq-platform-management", err)
	}
	for _, checkedNamespace := range []string{namespace, "borq", "borq-system", "flux-system"} {
		for _, name := range []string{"flux-operator", "source-controller", "helm-controller", "kustomize-controller", "gitea"} {
			_, err := connection.Deployments().Get(ctx, checkedNamespace, name)
			if err == nil {
				return fmt.Errorf("legacy management deployment %q exists in %s; use a fresh cluster or migrate the legacy installation before bootstrapping; existing repositories and volumes have been left unchanged", name, checkedNamespace)
			}
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("check legacy management deployment: %w", err)
			}
		}
		instance := &unstructured.Unstructured{}
		instance.SetAPIVersion("fluxcd.controlplane.io/v1")
		instance.SetKind("FluxInstance")
		instance.SetNamespace(checkedNamespace)
		instance.SetName("flux")
		_, err := connection.Objects().Get(ctx, instance)
		if err == nil {
			return fmt.Errorf("legacy FluxInstance exists in %s; use a fresh cluster or migrate the legacy installation before bootstrapping; existing repositories and volumes have been left unchanged", checkedNamespace)
		}
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("check legacy Flux installation: %w", err)
		}
	}
	return nil
}

func legacyManagementResourceError(kind, name string, err error) error {
	if err != nil {
		return fmt.Errorf("check legacy management %s borq/%s: %w", kind, name, err)
	}
	return fmt.Errorf("legacy management %s borq/%s exists; migration is required before bootstrapping; existing resources have been left unchanged", kind, name)
}

// Kubernetes APIs require shared object pointers to perform resource updates.
func borqDeployment(request Request) *appsv1.Deployment {
	replicas := int32(1)
	// Avoid requesting an extra replica against Borq's single SQLite volume.
	maxSurge, maxUnavailable := intstr.FromInt32(0), intstr.FromInt32(1)
	pullPolicy := corev1.PullIfNotPresent
	if request.ServerImageArchivePath == "" {
		image, err := reference.ParseNormalizedNamed(request.BorqImage)
		tagged, hasTag := image.(reference.Tagged)
		_, pinned := image.(reference.Digested)
		if err == nil && !pinned && (!hasTag || tagged.Tag() == "latest") {
			pullPolicy = corev1.PullAlways
		}
	}
	// Kubernetes needs a pointer to distinguish disabled service links from its enabled default.
	enableServiceLinks := false
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: borqName, Namespace: request.Namespace, Labels: appLabels(borqName, "server")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &maxSurge, MaxUnavailable: &maxUnavailable}},
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(borqName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(borqName)},
				Spec: corev1.PodSpec{
					ServiceAccountName: borqServiceAccountName,
					EnableServiceLinks: &enableServiceLinks,
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: borqDataClaimName}}},
						{Name: "temporary", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "admin-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "borq-admin-auth"}}},
					},
					Containers: []corev1.Container{{
						Name: borqName, Image: request.BorqImage, ImagePullPolicy: pullPolicy,
						Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}},
						Args:         borqServerArguments(request),
						Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: borqContainerPort}, {Name: "telemetry", ContainerPort: borqTelemetryContainerPort}},
						VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: borqDataMountPath}, {Name: "temporary", MountPath: "/tmp"}, {Name: "admin-auth", MountPath: "/var/run/borq/admin-auth", ReadOnly: true}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/api/readyz", Port: intstr.FromInt32(borqContainerPort),
						}}, InitialDelaySeconds: 1, PeriodSeconds: 2},
					}},
				},
			},
		},
	}
}

func borqServerArguments(request Request) []string {
	arguments := []string{"server", "--listen=0.0.0.0", "--port=7833", "--telemetry-port=7834", "--data-dir=" + borqDataMountPath,
		"--admin-token-file=/var/run/borq/admin-auth/token", "--management-enabled", "--management-bootstrap-only", "--management-cluster-name=" + request.ClusterName, "--infrastructure-enabled", "--infrastructure-profile=" + request.Mode,
		"--infrastructure-storage-class=" + request.StorageClass}
	if request.advertisedApiUrl != "" {
		arguments = append(arguments, "--api-url="+request.advertisedApiUrl)
	}
	if request.OpenbaoUrl != "" {
		arguments = append(arguments, "--management-openbao-url="+request.OpenbaoUrl)
	}
	if request.TelemetryUrl != "" {
		arguments = append(arguments, "--management-telemetry-url="+request.TelemetryUrl)
	}
	if request.LokiUrl != "" {
		arguments = append(arguments, "--management-loki-url="+request.LokiUrl)
	}
	if request.PrometheusUrl != "" {
		arguments = append(arguments, "--management-prometheus-url="+request.PrometheusUrl)
	}
	if request.S3Endpoint != "" {
		arguments = append(arguments, "--management-s3-endpoint="+request.S3Endpoint)
	}
	return arguments
}

func borqClusterRole() kubernetes.ClusterRole {
	// Borq installs and upgrades upstream charts, including their CRDs, RBAC and hooks.
	// It therefore needs management-cluster administrator privileges, like bootstrap.
	return kubernetes.ClusterRole{Name: borqRoleName, Labels: appLabels(borqName, "rbac"), Rules: []kubernetes.PolicyRule{
		{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
	}}
}
