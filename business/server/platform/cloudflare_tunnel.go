package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/cloudflare"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const managementTunnelName = "borq-cloudflared"

func reconcileManagementTunnel(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	if settings.Network.Cloudflare == nil {
		return nil
	}
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	option := settings.Network.Cloudflare.Tunnel
	if settings.Network.Cloudflare.ExposureMode() == "direct" {
		var deployment appsv1.Deployment
		err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementTunnelName}, &deployment)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := checkNetworkOwner(&deployment); err != nil {
			return err
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
			return nil
		}
		if err := checkManagementDirectTarget(ctx, client, settings); err != nil {
			return err
		}
		zero := int32(0)
		deployment.Spec.Replicas = &zero // Kubernetes uses a pointer for optional replicas.
		return client.Update(ctx, &deployment)
	}
	if option == nil || !option.Enabled {
		return fmt.Errorf("tunnel exposure requires enabled connector settings")
	}
	credentials, err := readNetworkBootstrapCredentials(ctx, client, settings)
	if err != nil {
		return err
	}
	api, err := cloudflare.New(cloudflare.Config{ZoneID: settings.Network.Cloudflare.ZoneID, ApiToken: string(credentials.Data["cloudflareApiToken"])})
	if err != nil {
		return fmt.Errorf("configure Cloudflare tunnel API")
	}
	owner, err := networkOwnerID(ctx, connection)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(owner + "/" + settings.Namespace + "/" + settings.Network.Cloudflare.ZoneID))
	name := "borq-" + hex.EncodeToString(digest[:16])
	state := corev1.ConfigMap{}
	err = client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementTunnelName}, &state)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if err := checkNetworkOwner(&state); err != nil {
			return err
		}
		if state.Data["name"] != name || state.Data["domain"] != settings.Network.Cloudflare.WildcardDomain {
			return fmt.Errorf("stored Cloudflare tunnel belongs to different network settings; explicit migration is required")
		}
	}
	tunnel, err := api.EnsureTunnel(ctx, name, state.Data["tunnelId"])
	if err != nil {
		return fmt.Errorf("provision Cloudflare tunnel (API token requires Account Cloudflare Tunnel Edit): %w", err)
	}
	state = corev1.ConfigMap{ObjectMeta: networkMetadata(settings.Namespace, managementTunnelName), Data: map[string]string{"name": name, "domain": settings.Network.Cloudflare.WildcardDomain, "tunnelId": tunnel.ID, "accountId": tunnel.AccountID, "ready": "false"}}
	if err := ensureNetworkResource(ctx, client, &state); err != nil {
		return err
	}
	if err := api.ConfigureTunnel(ctx, tunnel, settings.Network.Cloudflare.WildcardDomain); err != nil {
		return err
	}
	token, err := api.TunnelToken(ctx, tunnel)
	if err != nil {
		return err
	}
	secret := corev1.Secret{ObjectMeta: networkMetadata(settings.Namespace, managementTunnelName), Data: map[string][]byte{"token": []byte(token)}}
	if err := ensureNetworkResource(ctx, client, &secret); err != nil {
		return err
	}
	deployment := managementTunnelDeployment(settings, token)
	if err := ensureNetworkResource(ctx, client, &deployment); err != nil {
		return err
	}
	if err := connection.Deployments().WaitForRollout(ctx, settings.Namespace, managementTunnelName); err != nil {
		return fmt.Errorf("wait for Cloudflare connector: %w", err)
	}
	state.Data["ready"] = "true"
	return ensureNetworkResource(ctx, client, &state)
}

func managementTunnelDeployment(settings Settings, token string) appsv1.Deployment {
	replicas := int32(2)
	disabled := false
	nonroot := true
	labels := networkLabels()
	labels["app.kubernetes.io/name"] = managementTunnelName
	digest := sha256.Sum256([]byte(token))
	return appsv1.Deployment{ObjectMeta: networkMetadata(settings.Namespace, managementTunnelName), Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": managementTunnelName}}, // Kubernetes requires pointers for optional replicas and selectors.
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"borq.run/tunnel-token-sha256": hex.EncodeToString(digest[:])}}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &disabled, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &nonroot}, // Kubernetes security options are optional pointer fields.
			Containers: []corev1.Container{{Name: "cloudflared", Image: settings.Network.Cloudflare.Tunnel.Image,
				Args:            []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run", "--token-file", "/etc/cloudflared/token"},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &disabled, ReadOnlyRootFilesystem: &nonroot, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 2000}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(2000)}}, PeriodSeconds: 5},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "tunnel-token", MountPath: "/etc/cloudflared", ReadOnly: true}},
			}}, Volumes: []corev1.Volume{{Name: "tunnel-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: managementTunnelName}}}},
		}},
	}}
}

// ReadManagementTunnel returns a verified management tunnel's public DNS target.
// It never returns the API credential or the connector's run token.
func ReadManagementTunnel(ctx context.Context, input ManagementInput) (string, error) {
	if input.Settings.Network.Cloudflare == nil || input.Settings.Network.Cloudflare.ExposureMode() != "tunnel" {
		return "", nil
	}
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return "", err
	}
	if settings.Network.Cloudflare == nil || settings.Network.Cloudflare.ExposureMode() != "tunnel" {
		return "", nil
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return "", err
	}
	client, err := networkClient(connection)
	if err != nil {
		return "", err
	}
	var state corev1.ConfigMap
	err = client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: managementTunnelName}, &state)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := checkNetworkOwner(&state); err != nil {
		return "", err
	}
	if state.Data["domain"] != settings.Network.Cloudflare.WildcardDomain {
		return "", fmt.Errorf("Cloudflare tunnel domain does not match management configuration")
	}
	if state.Data["ready"] != "true" {
		return "", nil
	}
	id := state.Data["tunnelId"]
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		return "", fmt.Errorf("stored Cloudflare tunnel ID is invalid")
	}
	if _, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")); err != nil {
		return "", fmt.Errorf("stored Cloudflare tunnel ID is invalid")
	}
	return id + ".cfargotunnel.com", nil
}
