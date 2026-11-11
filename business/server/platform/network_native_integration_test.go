//go:build integration

package platform

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// This test proves native issuer persistence, authoritative route reconciliation,
// ownership refusal and UID-fenced retirement against Kubernetes. It installs
// CRDs only and never fabricates certificate readiness or contacts a public
// certificate authority or DNS provider.
func TestNetworkNativeIntegration(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cluster := fmt.Sprintf("borq-native-%d", time.Now().UnixNano())
	command := func(args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, output)
		}
		return output
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if output, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", cluster).CombinedOutput(); err != nil {
			t.Errorf("remove isolated native DNS cluster: %v\n%s", err, output)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	address := listener.Addr().String()
	assert.NoErr(listener.Close())
	command("k3d", "cluster", "create", cluster, "--image", "rancher/k3s:v1.35.3-k3s1", "--servers", "1", "--no-lb", "--api-port", address,
		"--volume", "/dev/null:/dev/kmsg@server:*", "--k3s-arg", "--disable=traefik@server:*", "--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*",
		"--k3s-arg", "--kube-proxy-arg=conntrack-max-per-core=0@server:*", "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--timeout", "120s")
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(kubeconfig, command("k3d", "kubeconfig", "get", cluster), 0600))
	command("kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "../../../zarf/crds/certmanager/v1.20.2.yaml")
	command("kubectl", "--kubeconfig", kubeconfig, "wait", "--for=condition=Established", "crd/clusterissuers.cert-manager.io", "crd/certificates.cert-manager.io", "crd/challenges.acme.cert-manager.io", "--timeout=60s")
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	assert.NoErr(prepareNamespace(ctx, connection, SystemNamespace))
	assert.NoErr(prepareNamespace(ctx, connection, ManagementNamespace))
	client, err := networkClient(connection)
	assert.NoErr(err)
	settings := Settings{ClusterName: "production", Namespace: ManagementNamespace, Network: networkTestSettings()}
	assert.NoErr(ensureGatewayApi(ctx, connection))
	assert.NoErr(reconcileManagementPublicRoute(ctx, client, settings))
	assert.NoErr(reconcileManagementPublicRoute(ctx, client, settings))
	t.Run("manual gateway edits are reconciled", func(t *testing.T) {
		assert := is.New(t)
		var gateway gatewayv1.Gateway
		key := ctrlclient.ObjectKey{Namespace: ManagementNamespace, Name: managementPublicRouteName}
		assert.NoErr(client.Get(ctx, key, &gateway))
		identity := gateway.UID
		gateway.Spec.Listeners[0].Port = 8080
		gateway.Spec.Listeners[0].Protocol = gatewayv1.HTTPProtocolType
		gateway.Spec.Listeners[0].TLS = nil
		from := gatewayv1.NamespacesFromAll
		gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &from}}
		gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{Name: "manual", Port: 9080, Protocol: gatewayv1.HTTPProtocolType})
		gateway.Annotations = map[string]string{"example.com/manual-note": "keep"}
		assert.NoErr(client.Update(ctx, &gateway, ctrlclient.FieldOwner("kubectl-edit")))
		assert.NoErr(client.Get(ctx, key, &gateway))
		assert.Equal(gateway.Spec.Listeners[0].Port, gatewayv1.PortNumber(8080))
		assert.Equal(gateway.Spec.Listeners[0].Protocol, gatewayv1.HTTPProtocolType)
		// The address is controller-owned fixture data, not evidence of readiness.
		addressType := gatewayv1.IPAddressType
		gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Type: &addressType, Value: "192.0.2.1"}}
		assert.NoErr(client.Status().Update(ctx, &gateway, ctrlclient.FieldOwner("gateway-controller")))
		for range 2 {
			if err := reconcileManagementPublicRoute(ctx, client, settings); err != nil {
				t.Fatalf("reconcile manually edited Gateway: %v", err)
			}
			assert.NoErr(client.Get(ctx, key, &gateway))
			assert.Equal(gateway.UID, identity)
			assert.Equal(len(gateway.Spec.Listeners), 1)
			assert.Equal(gateway.Spec.Listeners[0].Port, gatewayv1.PortNumber(8443))
			assert.Equal(gateway.Spec.Listeners[0].Protocol, gatewayv1.HTTPSProtocolType)
			assert.True(gateway.Spec.Listeners[0].TLS != nil)
			assert.Equal(gateway.Spec.Listeners[0].TLS.CertificateRefs[0].Name, gatewayv1.ObjectName(managementPublicRouteName+"-tls"))
			assert.Equal(*gateway.Spec.Listeners[0].AllowedRoutes.Namespaces.From, gatewayv1.NamespacesFromSame)
			assert.Equal(gateway.Annotations["example.com/manual-note"], "keep")
			assert.Equal(gateway.Status.Addresses, []gatewayv1.GatewayStatusAddress{{Type: &addressType, Value: "192.0.2.1"}})
		}
	})
	t.Run("manual route edits are reconciled", func(t *testing.T) {
		assert := is.New(t)
		var route gatewayv1.HTTPRoute
		key := ctrlclient.ObjectKey{Namespace: ManagementNamespace, Name: managementPublicRouteName}
		assert.NoErr(client.Get(ctx, key, &route))
		port := gatewayv1.PortNumber(8080)
		route.Spec.Rules[0].BackendRefs[0].Port = &port
		assert.NoErr(client.Update(ctx, &route, ctrlclient.FieldOwner("kubectl-edit")))
		assert.NoErr(reconcileManagementPublicRoute(ctx, client, settings))
		assert.NoErr(client.Get(ctx, key, &route))
		assert.Equal(*route.Spec.Rules[0].BackendRefs[0].Port, gatewayv1.PortNumber(80))
	})
	t.Run("foreign gateway is not adopted", func(t *testing.T) {
		assert := is.New(t)
		foreign := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: ManagementNamespace, Name: "foreign"}, Spec: gatewayv1.GatewaySpec{
			GatewayClassName: borqTraefikName,
			Listeners:        []gatewayv1.Listener{{Name: "web", Port: 8080, Protocol: gatewayv1.HTTPProtocolType}},
		}}
		assert.NoErr(client.Create(ctx, foreign, ctrlclient.FieldOwner("kubectl-edit")))
		desired := foreign.DeepCopy()
		desired.Labels = networkLabels()
		desired.Spec.Listeners[0].Port = 8000
		err := ensureNetworkResource(ctx, client, desired)
		assert.True(err != nil && strings.Contains(err.Error(), "not owned"))
		var observed gatewayv1.Gateway
		assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(foreign), &observed))
		assert.Equal(observed.Spec, foreign.Spec)
		assert.Equal(observed.Labels, foreign.Labels)
		assert.Equal(observed.ResourceVersion, foreign.ResourceVersion)
	})
	var managementCertificate certmanagerv1.Certificate
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: ManagementNamespace, Name: managementPublicRouteName + "-tls"}, &managementCertificate))
	assert.Equal(managementCertificate.Spec.DNSNames, []string{"production.example.com"})
	assert.Equal(managementCertificate.Spec.IssuerRef.Name, "letsencrypt-cloudflare")
	issuer := managementCloudflareIssuer(*settings.Network.Cloudflare)
	assert.NoErr(ensureNetworkResource(ctx, client, issuer))
	t.Run("tunnel setup does not wait for ACME", func(t *testing.T) {
		assert := is.New(t)
		// No cert-manager controller runs here: tunnel setup must not wait for an
		// ACME Ready condition or require a fabricated issuance result.
		tunnelSettings := *settings.Network.Cloudflare
		tunnelSettings.Exposure = "tunnel"
		tunnelContext, stopTunnel := context.WithTimeout(ctx, 10*time.Second)
		assert.NoErr(reconcileManagementCloudflare(tunnelContext, client, tunnelSettings, []byte("fixture-zone-token")))
		stopTunnel()
	})
	var observed certmanagerv1.ClusterIssuer
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Name: issuer.Name}, &observed))
	assert.True(observed.Spec.ACME.Solvers[0].DNS01.Cloudflare != nil)
	assert.True(observed.Spec.ACME.Solvers[0].DNS01.Webhook == nil)
	legacy := issuer.DeepCopy()
	legacy.Name = "letsencrypt"
	legacy.Spec.ACME.Solvers[0].DNS01 = &acmev1.ACMEChallengeSolverDNS01{Webhook: &acmev1.ACMEIssuerDNS01ProviderWebhook{GroupName: "dns01.borq.io", SolverName: "borq"}}
	assert.NoErr(client.Create(ctx, legacy))
	secret := &corev1.Secret{ObjectMeta: networkMetadata(SystemNamespace, "borq-dns01-ca")}
	assert.NoErr(client.Create(ctx, secret))
	wrongIdentity := types.UID("replacement-must-not-be-deleted")
	assert.True(apierrors.IsConflict(client.Delete(ctx, secret, ctrlclient.Preconditions{UID: &wrongIdentity})))
	foreign := &corev1.Service{ObjectMeta: networkMetadata(SystemNamespace, "borq-dns01-webhook"), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443}}}}
	foreign.Labels = nil
	assert.NoErr(client.Create(ctx, foreign))
	assert.True(retireBorqDns01(ctx, client, settings) != nil)
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(secret), secret))
	foreign.Labels = networkLabels()
	assert.NoErr(client.Update(ctx, foreign))
	assert.NoErr(retireBorqDns01(ctx, client, settings))
	assert.True(apierrors.IsNotFound(client.Get(ctx, ctrlclient.ObjectKeyFromObject(secret), &corev1.Secret{})))
	assert.True(apierrors.IsNotFound(client.Get(ctx, ctrlclient.ObjectKey{Name: legacy.Name}, &certmanagerv1.ClusterIssuer{})))
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Name: issuer.Name}, &observed))
	assert.True(observed.Spec.ACME.Solvers[0].DNS01.Cloudflare != nil)

	// A paused Deployment is an API persistence fixture: it does not create
	// connector pods or claim a real Cloudflare connection. Switching to direct
	// must preserve its replicas until a public target is configured.
	replicas := int32(2)
	labels := map[string]string{"fixture": "connector-switch"}
	connector := &appsv1.Deployment{ObjectMeta: networkMetadata(ManagementNamespace, managementTunnelName), Spec: appsv1.DeploymentSpec{
		Paused: true, Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unused", Image: "registry.k8s.io/pause:3.10"}}}},
	}}
	assert.NoErr(client.Create(ctx, connector))
	assert.True(reconcileManagementTunnel(ctx, connection, settings) != nil)
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(connector), connector))
	assert.Equal(*connector.Spec.Replicas, int32(2))
	settings.Network.Cloudflare.DirectTarget = "8.8.8.8"
	assert.NoErr(reconcileManagementTunnel(ctx, connection, settings))
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(connector), connector))
	assert.Equal(*connector.Spec.Replicas, int32(0))
}
