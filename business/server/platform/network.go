package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ReconcileManagementNetwork observes private administration and configures
// cert-manager's native Cloudflare solver without a Borq certificate broker.
func ReconcileManagementNetwork(ctx context.Context, input ManagementInput) (NetworkState, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return NetworkState{}, err
	}
	if settings.Network.Tailscale == nil && settings.Network.Cloudflare == nil {
		return NetworkState{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return NetworkState{}, err
	}
	client, err := networkClient(connection)
	if err != nil {
		return NetworkState{}, err
	}
	var state NetworkState
	if settings.Network.Tailscale != nil {
		reportManagementNetworkProgress(ctx, input, "waiting for the Tailscale management hostname")
		if err := ensureNetworkIngress(ctx, client, settings, managementNetworkIngress(settings)); err != nil {
			return NetworkState{}, err
		}
		err = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
			var readErr error
			state, readErr = readManagementNetwork(ctx, client, settings, false)
			return state.ManagementURL != "", readErr
		})
		if err != nil {
			return NetworkState{}, fmt.Errorf("wait for management tailnet hostname: %w", err)
		}
		reportManagementNetworkProgress(ctx, input, "verifying Tailscale management HTTPS connectivity")
		if err := probeManagementAdmin(ctx, connection, settings, state.ManagementURL); err != nil {
			return NetworkState{}, err
		}
	}
	if settings.Network.Cloudflare != nil {
		reportManagementNetworkProgress(ctx, input, "configuring Cloudflare DNS and the public management route")
		credentials, err := readNetworkBootstrapCredentials(ctx, client, settings)
		if err != nil {
			return NetworkState{}, err
		}
		if err := reconcileManagementCloudflare(ctx, client, *settings.Network.Cloudflare, credentials.Data["cloudflareApiToken"]); err != nil {
			return NetworkState{}, err
		}
		if err := retireBorqDns01(ctx, client, settings); err != nil {
			return NetworkState{}, err
		}
		if err := reconcileManagementPublicRoute(ctx, client, settings); err != nil {
			return NetworkState{}, err
		}
		message := "checking direct public exposure"
		if settings.Network.Cloudflare.ExposureMode() == "tunnel" {
			message = "provisioning the Cloudflare tunnel and waiting for connector readiness"
		}
		reportManagementNetworkProgress(ctx, input, message)
		if err := reconcileManagementTunnel(ctx, connection, settings); err != nil {
			return NetworkState{}, err
		}
	}
	if err := recordNetworkProof(ctx, client, settings, state); err != nil {
		return NetworkState{}, err
	}
	return state, nil
}

func reportManagementNetworkProgress(ctx context.Context, input ManagementInput, message string) {
	slog.InfoContext(ctx, "management_network_progress", "namespace", input.Settings.Namespace, "message", message)
	if input.ReportProgress != nil {
		input.ReportProgress(message)
	}
}

// ReadManagementNetwork returns assignments already verified over HTTPS, without changing resources.
func ReadManagementNetwork(ctx context.Context, input ManagementInput) (NetworkState, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return NetworkState{}, err
	}
	if settings.Network.Tailscale == nil {
		return NetworkState{}, nil
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return NetworkState{}, err
	}
	client, err := networkClient(connection)
	if err != nil {
		return NetworkState{}, err
	}
	return readManagementNetwork(ctx, client, settings, true)
}

func readManagementNetwork(ctx context.Context, client ctrlclient.Client, settings Settings, proven bool) (NetworkState, error) {
	if settings.Network.Tailscale == nil {
		return NetworkState{}, nil
	}
	managementURL, err := readNetworkIngressURL(ctx, client, settings, "borq-management", proven)
	return NetworkState{ManagementURL: managementURL}, err
}

func readNetworkIngressURL(ctx context.Context, client ctrlclient.Client, settings Settings, name string, proven bool) (string, error) {
	ingress := &networkingv1.Ingress{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: name}, ingress); err != nil {
		return "", err
	}
	if err := checkNetworkOwner(ingress); err != nil {
		return "", err
	}
	desired := managementNetworkIngress(settings)
	if !reflect.DeepEqual(ingress.Spec.TLS, desired.Spec.TLS) {
		return "", nil
	}
	assigned, err := networkIngressURL(ingress, settings)
	if err != nil {
		return "", err
	}
	if assigned == ingress.Annotations["borq.run/previous-tailnet-url"] {
		return "", nil
	}
	if proven && assigned != ingress.Annotations["borq.run/proven-tailnet-url"] {
		return "", nil
	}
	return assigned, nil
}

func recordNetworkProof(ctx context.Context, client ctrlclient.Client, settings Settings, state NetworkState) error {
	for name, origin := range map[string]string{"borq-management": state.ManagementURL} {
		if origin == "" {
			continue
		}
		current := &networkingv1.Ingress{}
		if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: name}, current); err != nil {
			return err
		}
		if err := checkNetworkOwner(current); err != nil {
			return err
		}
		desired := managementNetworkIngress(settings)
		assigned, err := networkIngressURL(current, settings)
		if err != nil {
			return err
		}
		if assigned != origin || !reflect.DeepEqual(current.Spec, desired.Spec) {
			return fmt.Errorf("management network route changed while verifying HTTPS; retry")
		}
		if current.Annotations["borq.run/proven-tailnet-url"] == origin {
			continue
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["borq.run/proven-tailnet-url"] = origin
		if err := client.Update(ctx, current, ctrlclient.FieldOwner("borq-network")); err != nil {
			return err
		}
	}
	return nil
}

func networkIngressURL(ingress *networkingv1.Ingress, settings Settings) (string, error) {
	for _, address := range ingress.Status.LoadBalancer.Ingress {
		if address.Hostname == "" {
			continue
		}
		assigned := settings.Network
		assigned.ManagementURL = "https://" + strings.TrimSuffix(address.Hostname, ".")
		if err := ValidateNetworkSettings(assigned); err != nil {
			return "", fmt.Errorf("management Ingress returned an invalid tailnet hostname")
		}
		return assigned.ManagementURL, nil
	}
	return "", nil
}

func ensureNetworkIngress(ctx context.Context, client ctrlclient.Client, settings Settings, desired *networkingv1.Ingress) error {
	current := &networkingv1.Ingress{}
	err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		return client.Create(ctx, desired, ctrlclient.FieldOwner("borq-network"))
	}
	if err != nil {
		return err
	}
	if err := checkNetworkOwner(current); err != nil {
		return err
	}
	updated := current.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	if !reflect.DeepEqual(current.Spec.TLS, desired.Spec.TLS) {
		assigned, err := networkIngressURL(current, settings)
		if err != nil {
			return err
		}
		if assigned == "" {
			return fmt.Errorf("management tailnet hostname must be assigned before requesting a change")
		}
		if previous, pending := current.Annotations["borq.run/previous-tailnet-url"]; pending && previous == assigned {
			return fmt.Errorf("management tailnet hostname change is still pending")
		}
		// Store the old assignment atomically with the request. A restart after
		// applying the new spec must not accept the controller's stale status.
		updated.Annotations["borq.run/previous-tailnet-url"] = assigned
	}
	maps.Copy(updated.Annotations, desired.Annotations)
	updated.Spec = desired.Spec
	if reflect.DeepEqual(current.Spec, updated.Spec) && reflect.DeepEqual(current.Annotations, updated.Annotations) {
		return nil
	}
	// The retained UID and resourceVersion fence both replacement and concurrent edits.
	return client.Update(ctx, updated, ctrlclient.FieldOwner("borq-network"))
}

func networkValuesUpgrade(definition component, inspection helm.ReleaseInspection, desired map[string]any) (bool, error) {
	if definition.Name != "tailscale-operator" && definition.Name != "external-dns" && definition.Name != "traefik" || inspection.State.Status == "absent" {
		return false, nil
	}
	// Public networking settings take effect on bootstrap retries. Chart version
	// changes still require an explicit component upgrade.
	if strings.TrimPrefix(inspection.State.Version, "v") == strings.TrimPrefix(definition.Version, "v") {
		return true, nil
	}
	stored, _ := json.Marshal(inspection.Values)
	wanted, _ := json.Marshal(desired)
	if string(stored) != string(wanted) {
		return false, fmt.Errorf("network settings require an explicit %s component upgrade because its installed chart version differs", definition.Name)
	}
	return false, nil
}

func managementNetworkIngress(settings Settings) *networkingv1.Ingress {
	class := "tailscale"
	pathType := networkingv1.PathTypePrefix
	metadata := networkMetadata(settings.Namespace, "borq-management")
	metadata.Annotations = map[string]string{"tailscale.com/tags": settings.Network.Tailscale.AdminTag}
	return &networkingv1.Ingress{ObjectMeta: metadata, Spec: networkingv1.IngressSpec{
		IngressClassName: &class, TLS: []networkingv1.IngressTLS{{Hosts: []string{settings.Network.Tailscale.ManagementHostname}}},
		Rules: []networkingv1.IngressRule{{IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "borq", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}},
	}}
}

func readNetworkBootstrapCredentials(ctx context.Context, client ctrlclient.Client, settings Settings) (corev1.Secret, error) {
	secret := corev1.Secret{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "borq-network-credentials"}, &secret); err != nil {
		return corev1.Secret{}, fmt.Errorf("management network credentials are unavailable")
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "borq" {
		return corev1.Secret{}, fmt.Errorf("management network credentials are not owned by Borq")
	}
	keys := []string{}
	if settings.Network.Tailscale != nil {
		keys = append(keys, "tailscaleClientId", "tailscaleClientSecret")
	}
	if settings.Network.Cloudflare != nil {
		keys = append(keys, "cloudflareApiToken")
	}
	for _, key := range keys {
		if len(secret.Data[key]) == 0 {
			return corev1.Secret{}, fmt.Errorf("management network credential %s is missing", key)
		}
	}
	return secret, nil
}

func (input componentRun) prepareNetworkComponent(ctx context.Context, definition component) error {
	if definition.Name != "tailscale-operator" && definition.Name != "external-dns" {
		return nil
	}
	client, err := networkClient(input.connection)
	if err != nil {
		return err
	}
	if definition.Name == "external-dns" {
		if _, err := readNetworkBootstrapCredentials(ctx, client, input.values.Settings); err != nil {
			return err
		}
		return EnsureNetworkDnsEndpoint(ctx, input.connection)
	}
	// Check before Helm selects this default; a foreign class must never control Borq proxies.
	proxyClass := networkProxyClass()
	if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(proxyClass), proxyClass); err == nil {
		if err := checkNetworkOwner(proxyClass); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("check tailscale proxy class: %w", err)
	}
	clientID, clientSecret := input.target.TailscaleClientID, input.target.TailscaleClientSecret
	if input.scope == "management" {
		credentials, err := readNetworkBootstrapCredentials(ctx, client, input.values.Settings)
		if err != nil {
			return err
		}
		clientID, clientSecret = string(credentials.Data["tailscaleClientId"]), string(credentials.Data["tailscaleClientSecret"])
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("Tailscale operator requires private OAuth credentials")
	}
	return ensureNetworkOperatorCredentials(ctx, client, input.scope, clientID, clientSecret)
}

func networkOwnerID(ctx context.Context, connection kubernetes.Connection) (string, error) {
	client, err := networkClient(connection)
	if err != nil {
		return "", err
	}
	namespace := &corev1.Namespace{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Name: "kube-system"}, namespace); err != nil {
		return "", fmt.Errorf("read physical cluster identity: %w", err)
	}
	if namespace.UID == "" {
		return "", fmt.Errorf("physical cluster identity is missing")
	}
	return "borq-" + string(namespace.UID), nil
}

func refreshNetworkControllerCredentials(ctx context.Context, connection kubernetes.Connection, definition component, settings Settings) error {
	namespace, secretName, deploymentName := SystemNamespace, "operator-oauth", "operator"
	if definition.Name == "external-dns" {
		namespace, secretName, deploymentName = settings.Namespace, "borq-network-credentials", "external-dns"
	}
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: secretName}, secret); err != nil {
		return fmt.Errorf("read network controller credential version")
	}
	if definition.Name == "tailscale-operator" {
		if err := checkNetworkOwner(secret); err != nil {
			return err
		}
	} else if secret.Labels["app.kubernetes.io/managed-by"] != "borq" {
		return fmt.Errorf("network credentials are not owned by Borq")
	}
	deployment := &appsv1.Deployment{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: deploymentName}, deployment); err != nil {
		return fmt.Errorf("read network controller Deployment")
	}
	if deployment.Annotations["meta.helm.sh/release-name"] != definition.Name || deployment.Annotations["meta.helm.sh/release-namespace"] != namespace {
		return fmt.Errorf("network controller Deployment is outside the managed release")
	}
	const annotation = "borq.run/network-credentials-version"
	if deployment.Spec.Template.Annotations[annotation] != secret.ResourceVersion {
		annotations := maps.Clone(deployment.Spec.Template.Annotations)
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[annotation] = secret.ResourceVersion
		deployment.Spec.Template.Annotations = annotations
		if err := client.Update(ctx, deployment); err != nil {
			return fmt.Errorf("roll out network controller credential rotation: %w", err)
		}
	}
	return connection.Deployments().WaitForRollout(ctx, namespace, deploymentName)
}

func ensureNetworkOperatorCredentials(ctx context.Context, client ctrlclient.Client, scope, clientID, clientSecret string) error {
	if scope != "management" && scope != "target" {
		return fmt.Errorf("network operator installation scope is invalid")
	}
	current := &corev1.Secret{}
	err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: SystemNamespace, Name: "operator-oauth"}, current)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read operator credential ownership")
	}
	if err == nil {
		if err := checkNetworkOwner(current); err != nil {
			return err
		}
		previous := current.Labels["borq.run/operator-scope"]
		if previous != "management" && previous != "target" {
			return fmt.Errorf("operator credential installation scope is missing")
		}
		if previous != scope {
			if previous == "management" && scope == "target" {
				if len(current.Data["client_id"]) == 0 || len(current.Data["client_secret"]) == 0 {
					return fmt.Errorf("management operator credentials must be restored before convergence")
				}
				return nil
			}
			return fmt.Errorf("existing target operator cannot be adopted as management")
		}
	}
	if apierrors.IsNotFound(err) {
		deployment := &appsv1.Deployment{}
		installedErr := client.Get(ctx, ctrlclient.ObjectKey{Namespace: SystemNamespace, Name: "operator"}, deployment)
		if installedErr == nil {
			return fmt.Errorf("installed operator credentials are missing; restore their original installation scope")
		}
		if !apierrors.IsNotFound(installedErr) {
			return fmt.Errorf("read existing operator installation")
		}
		records := &corev1.SecretList{}
		if err := client.List(ctx, records, ctrlclient.InNamespace(SystemNamespace), ctrlclient.MatchingLabels{"owner": "helm", "name": "tailscale-operator"}); err != nil {
			return fmt.Errorf("read existing operator release")
		}
		if len(records.Items) > 0 {
			return fmt.Errorf("installed operator credentials are missing; restore their original installation scope")
		}
	}
	secret := &corev1.Secret{ObjectMeta: networkMetadata(SystemNamespace, "operator-oauth"), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"client_id": []byte(clientID), "client_secret": []byte(clientSecret)}}
	secret.Labels["borq.run/operator-scope"] = scope
	return ensureNetworkResource(ctx, client, secret)
}

func networkOperatorTag(ctx context.Context, connection kubernetes.Connection, input valuesInput) (string, error) {
	tag := input.Network.Tailscale.OperatorTag
	if input.ClusterID == "" {
		tag = input.Network.Tailscale.ManagementOperatorTag
	}
	client, err := networkClient(connection)
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{}
	err = client.Get(ctx, ctrlclient.ObjectKey{Namespace: SystemNamespace, Name: "operator-oauth"}, secret)
	if apierrors.IsNotFound(err) {
		return tag, nil
	}
	if err != nil {
		return "", fmt.Errorf("read operator installation scope")
	}
	if err := checkNetworkOwner(secret); err != nil {
		return "", err
	}
	switch secret.Labels["borq.run/operator-scope"] {
	case "management":
		return input.Network.Tailscale.ManagementOperatorTag, nil
	case "target":
		return input.Network.Tailscale.OperatorTag, nil
	default:
		return "", fmt.Errorf("operator credential installation scope is missing")
	}
}
