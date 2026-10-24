package management

import (
	"context"
	"encoding/json"
	"fmt"

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
	if err := connection.PersistentVolumeClaims().Apply(ctx, kubernetes.PersistentVolumeClaim{
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
	if err := connection.Services().Apply(ctx, kubernetes.Service{
		Name: borqName, Namespace: request.Namespace, Labels: appLabels(borqName, "server"),
		PublishNotReadyAddresses: true,
		Selector:                 selectorLabels(borqName), Type: kubernetes.NodePortService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: borqContainerPort, NodePort: request.NodePort}},
	}); err != nil {
		return err
	}
	if err := connection.Services().Apply(ctx, kubernetes.Service{
		Name: "borq-telemetry", Namespace: request.Namespace, Labels: appLabels(borqName, "telemetry"),
		Selector: selectorLabels(borqName), Type: kubernetes.NodePortService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: borqTelemetryContainerPort, NodePort: request.TelemetryNodePort}},
	}); err != nil {
		return err
	}
	if err := connection.Objects().Apply(ctx, borqDeployment(request)); err != nil {
		return fmt.Errorf("apply application manager server: %w", err)
	}
	log.Info("waiting_for_management_server")
	return waitForManagementServer(ctx, log, request, connection)
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
	for _, checkedNamespace := range []string{namespace, "borq-system", "flux-system"} {
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

// Kubernetes APIs require shared object pointers to perform resource updates.
func borqDeployment(request Request) *appsv1.Deployment {
	replicas := int32(1)
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
			Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(borqName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(borqName)},
				Spec: corev1.PodSpec{
					ServiceAccountName: borqServiceAccountName,
					EnableServiceLinks: &enableServiceLinks,
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: borqDataClaimName}}},
						{Name: "temporary", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
					Containers: []corev1.Container{{
						Name: borqName, Image: request.BorqImage, ImagePullPolicy: pullPolicy,
						Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}},
						Args:         borqServerArguments(request),
						Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: borqContainerPort}, {Name: "telemetry", ContainerPort: borqTelemetryContainerPort}},
						VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: borqDataMountPath}, {Name: "temporary", MountPath: "/tmp"}},
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
		"--management-enabled", "--infrastructure-enabled", "--infrastructure-profile=" + request.Mode,
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
	return arguments
}

func borqClusterRole() kubernetes.ClusterRole {
	// Borq installs and upgrades upstream charts, including their CRDs, RBAC and hooks.
	// It therefore needs management-cluster administrator privileges, like bootstrap.
	return kubernetes.ClusterRole{Name: borqRoleName, Labels: appLabels(borqName, "rbac"), Rules: []kubernetes.PolicyRule{
		{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
	}}
}
