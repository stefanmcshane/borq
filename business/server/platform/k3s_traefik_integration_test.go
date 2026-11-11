//go:build integration

package platform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestK3sGatewayIntegration proves real Gateway/HTTPRoute acceptance, actual TLS
// issuance and HTTPS traffic, rejected backend references, and controller isolation
// beside K3s's untouched bundled Traefik. It does not fabricate controller status.
func TestK3sGatewayIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cluster := fmt.Sprintf("borq-traefik-%d", time.Now().UnixNano())
	command := func(ctx context.Context, binary string, args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", binary, args[0], err, output)
		}
		return output
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		output, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", cluster).CombinedOutput()
		if err != nil {
			t.Errorf("remove disposable cluster: %v\n%s", err, output)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	is.NoErr(err)
	apiAddress := listener.Addr().String()
	is.NoErr(listener.Close())
	command(ctx, "k3d", "cluster", "create", cluster, "--image", "rancher/k3s:v1.35.3-k3s1", "--servers", "1", "--no-lb", "--api-port", apiAddress, "--volume", "/dev/null:/dev/kmsg@server:*", "--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*", "--k3s-arg", "--kube-proxy-arg=conntrack-max-per-core=0@server:*", "--k3s-arg", "--kubelet-arg=eviction-hard=nodefs.available<1%,imagefs.available<1%,nodefs.inodesFree<1%@server:*", "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--timeout", "120s")
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	is.NoErr(os.WriteFile(kubeconfig, command(ctx, "k3d", "kubeconfig", "get", cluster), 0600))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	is.NoErr(err)
	client, err := networkClient(connection)
	is.NoErr(err)
	command(ctx, "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "testdata/k3s-traefik-route.yaml")
	is.NoErr(connection.Deployments().WaitForRollout(ctx, "kube-system", "traefik"))
	is.NoErr(connection.Deployments().WaitForRollout(ctx, "default", "existing-route"))
	// The HTTP check uses ServiceLB on the actual K3s node, not a forwarded
	// backend pod or a fabricated Ingress status.
	nodes := &corev1.NodeList{}
	is.NoErr(client.List(ctx, nodes))
	var nodeIP string
	for _, address := range nodes.Items[0].Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			nodeIP = address.Address
		}
	}
	is.True(nodeIP != "")
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := http.Client{Transport: transport, Timeout: 3 * time.Second}
	checkRoute := func(port, host string, status int) {
		t.Helper()
		is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(nodeIP, port), nil)
			if err != nil {
				return false, err
			}
			request.Host = host
			response, err := httpClient.Do(request)
			if err != nil {
				return false, nil
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			matched := err == nil && response.StatusCode == status && (status != http.StatusOK || strings.Contains(string(body), "Hostname: existing-route-"))
			if port != "80" && status == http.StatusOK {
				matched = matched && strings.Contains(string(body), "X-Forwarded-Proto: https")
			}
			return matched, nil
		}))
	}
	checkRoute("80", "existing.borq.test", http.StatusOK)

	var bundled appsv1.Deployment
	key := ctrlclient.ObjectKey{Namespace: "kube-system", Name: "traefik"}
	is.NoErr(client.Get(ctx, key, &bundled))
	identity, generation := bundled.UID, bundled.Generation
	settings, err := normalizeSettings(Settings{Profile: "production", Timeout: 3 * time.Minute})
	is.NoErr(err)
	is.NoErr(prepareNamespace(ctx, connection, SystemNamespace))
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfig})
	is.NoErr(err)
	defer releases.Close()
	_, err = reconcileComponent(ctx, releases, sharedCatalog()[0], valuesInput{Settings: settings}, false, false)
	is.NoErr(err)
	definition := sharedCatalog()[1]
	run := componentRun{connection: connection, kubeconfigPath: kubeconfig, values: valuesInput{Settings: settings}}
	_, err = run.reconcileTraefik(ctx, releases, definition)
	is.NoErr(err)
	client, err = networkClient(connection)
	is.NoErr(err)
	issuer := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "gateway-test"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{SelfSigned: &certmanagerv1.SelfSignedIssuer{}}}}
	is.NoErr(client.Create(ctx, &issuer))
	input := pocketIdTestInput(t)
	input.Namespace, input.IssuerURL, input.ClusterIssuer = "default", "https://gateway.borq.test", issuer.Name
	input, address, err := normalizePocketId(input)
	is.NoErr(err)
	certificate := pocketIdCertificate(input, address.Hostname())
	certificate.Labels["borq.run/target-id"] = "gateway-integration"
	is.NoErr(client.Create(ctx, certificate))
	gateway := pocketIdGateway(input, address.Hostname(), true)
	route := pocketIdHttpRoute(input, address.Hostname())
	gateway.Labels["borq.run/target-id"] = "gateway-integration"
	route.Labels["borq.run/target-id"] = "gateway-integration"
	route.Spec.Rules[0].BackendRefs[0].Name = "existing-route"
	port := gatewayv1.PortNumber(80)
	route.Spec.Rules[0].BackendRefs[0].Port = &port // Gateway API models optional backend ports as pointers.
	is.NoErr(client.Create(ctx, gateway))
	is.NoErr(client.Create(ctx, route))
	condition := func(conditions []metav1.Condition, name string, generation int64, status metav1.ConditionStatus) bool {
		value := meta.FindStatusCondition(conditions, name)
		return value != nil && value.Status == status && value.ObservedGeneration == generation
	}
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(route), route); err != nil {
			return false, err
		}
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(gateway), gateway); err != nil {
			return false, err
		}
		for _, parent := range route.Status.Parents {
			if parent.ControllerName == "traefik.io/gateway-controller" && condition(parent.Conditions, "Accepted", route.Generation, metav1.ConditionTrue) && condition(parent.Conditions, "ResolvedRefs", route.Generation, metav1.ConditionTrue) && condition(gateway.Status.Conditions, "Accepted", gateway.Generation, metav1.ConditionTrue) {
				return true, nil
			}
		}
		return false, nil
	}))
	applicationClient, err := kubeapps.New(kubeapps.NewInput{KubeconfigPath: kubeconfig})
	is.NoErr(err)
	var manifests []map[string]any
	for _, object := range []ctrlclient.Object{certificate, gateway, route} {
		desired, err := networkObject(object)
		is.NoErr(err)
		manifests = append(manifests, desired.Object)
	}
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		status, err := applicationClient.Status(ctx, manifests, "gateway-integration")
		return status.State == "succeeded", err
	}))
	var secret corev1.Secret
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: certificate.Spec.SecretName}, &secret))
	roots := x509.NewCertPool()
	is.True(roots.AppendCertsFromPEM(secret.Data["tls.crt"]))
	tlsTransport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: address.Hostname(), MinVersion: tls.VersionTLS12}}
	t.Cleanup(tlsTransport.CloseIdleConnections)
	tlsClient := http.Client{Transport: tlsTransport, Timeout: 3 * time.Second}
	var service corev1.Service
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: SystemNamespace, Name: borqTraefikName}, &service))
	var httpsPort int32
	for _, port := range service.Spec.Ports {
		if port.Name == "websecure" {
			httpsPort = port.NodePort
		}
	}
	is.True(httpsPort != 0)
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(nodeIP, fmt.Sprint(httpsPort)), nil)
		if err != nil {
			return false, err
		}
		request.Host = address.Hostname()
		response, err := tlsClient.Do(request)
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		return err == nil && response.StatusCode == 200 && strings.Contains(string(body), "Hostname: existing-route-"), nil
	}))
	// Exercise the management hostname through the actual Traefik controller.
	// The local fixture issuer proves TLS plumbing, not public DNS01 issuance.
	managementIssuer := issuer.DeepCopy()
	managementIssuer.Name = CloudflareCertificateIssuer
	managementIssuer.ResourceVersion, managementIssuer.UID = "", ""
	is.NoErr(client.Create(ctx, managementIssuer))
	var backend corev1.Service
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: "existing-route"}, &backend))
	managementService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "borq"}, Spec: corev1.ServiceSpec{Selector: backend.Spec.Selector, Ports: backend.Spec.Ports}}
	is.NoErr(client.Create(ctx, managementService))
	publicSettings := Settings{Namespace: "default", ClusterName: "production", Network: NetworkSettings{Cloudflare: &CloudflareSettings{WildcardDomain: "borq.test"}}}
	is.NoErr(reconcileManagementPublicRoute(ctx, client, publicSettings))
	managementHost := ManagementPublicHostname(publicSettings)
	var managementCertificate certmanagerv1.Certificate
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: managementPublicRouteName + "-tls"}, &managementCertificate); err != nil {
			return false, err
		}
		for _, status := range managementCertificate.Status.Conditions {
			if status.Type == certmanagerv1.CertificateConditionReady && string(status.Status) == "True" && status.ObservedGeneration == managementCertificate.Generation {
				return true, nil
			}
		}
		return false, nil
	}))
	var managementSecret corev1.Secret
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: managementPublicRouteName + "-tls"}, &managementSecret))
	managementRoots := x509.NewCertPool()
	is.True(managementRoots.AppendCertsFromPEM(managementSecret.Data["tls.crt"]))
	managementTransport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: managementRoots, ServerName: managementHost, MinVersion: tls.VersionTLS12}}
	t.Cleanup(managementTransport.CloseIdleConnections)
	managementClient := http.Client{Transport: managementTransport, Timeout: 3 * time.Second}
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(nodeIP, fmt.Sprint(httpsPort)), nil)
		if err != nil {
			return false, err
		}
		request.Host = managementHost
		response, err := managementClient.Do(request)
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		return err == nil && response.StatusCode == http.StatusOK && strings.Contains(string(body), "Hostname: existing-route-"), nil
	}))
	is.NoErr(reconcileManagementPublicRoute(ctx, client, publicSettings))
	checkRoute("80", address.Hostname(), http.StatusNotFound)
	checkRoute("80", "existing.borq.test", http.StatusOK)
	route.Spec.Rules[0].BackendRefs[0].Name = "missing-backend"
	is.NoErr(client.Update(ctx, route))
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(route), route); err != nil {
			return false, err
		}
		for _, parent := range route.Status.Parents {
			if condition(parent.Conditions, "ResolvedRefs", route.Generation, metav1.ConditionFalse) {
				return true, nil
			}
		}
		return false, nil
	}))
	route.Spec.Rules[0].BackendRefs[0].Name = "existing-route"
	is.NoErr(client.Update(ctx, route))
	// Tunnel routes use HTTP even though their browser-facing URLs stay HTTPS.
	// No certificate fixture or fabricated readiness is supplied for these hosts.
	var httpPort int32
	for _, port := range service.Spec.Ports {
		if port.Name == "web" {
			httpPort = port.NodePort
		}
	}
	is.True(httpPort != 0)
	is.NoErr(prepareNamespace(ctx, connection, "tunnel-test"))
	tunnelService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "tunnel-test", Name: "borq"}, Spec: corev1.ServiceSpec{Selector: backend.Spec.Selector, Ports: backend.Spec.Ports}}
	// Run the same backend fixture in the tunnel namespace.
	fixture := &appsv1.Deployment{}
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: "existing-route"}, fixture))
	fixture.Namespace, fixture.ResourceVersion, fixture.UID = "tunnel-test", "", ""
	fixture.Status = appsv1.DeploymentStatus{}
	is.NoErr(client.Create(ctx, fixture))
	is.NoErr(connection.Deployments().WaitForRollout(ctx, "tunnel-test", "existing-route"))
	is.NoErr(client.Create(ctx, tunnelService))
	tunnelSettings := Settings{Namespace: "tunnel-test", ClusterName: "tunnel", Network: NetworkSettings{Cloudflare: &CloudflareSettings{Exposure: "tunnel", WildcardDomain: "borq.test"}}}
	is.NoErr(reconcileManagementPublicRoute(ctx, client, tunnelSettings))
	checkRoute(fmt.Sprint(httpPort), ManagementPublicHostname(tunnelSettings), http.StatusOK)
	tunnelInput := pocketIdTestInput(t)
	tunnelInput.Connection, tunnelInput.Namespace = connection, "tunnel-test"
	tunnelInput.Tunnel, tunnelInput.IssuerURL = true, "https://id.tunnel.borq.test"
	pocketService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "tunnel-test", Name: "pocketid-" + tunnelInput.InstanceID}, Spec: corev1.ServiceSpec{Selector: backend.Spec.Selector, Ports: []corev1.ServicePort{{Port: 1411, TargetPort: backend.Spec.Ports[0].TargetPort}}}}
	is.NoErr(client.Create(ctx, pocketService))
	is.NoErr(PublishPocketId(ctx, tunnelInput))
	is.NoErr(PublishPocketId(ctx, tunnelInput))
	checkRoute(fmt.Sprint(httpPort), "id.tunnel.borq.test", http.StatusOK)
	var tunnelCertificates certmanagerv1.CertificateList
	var tunnelRequests certmanagerv1.CertificateRequestList
	is.NoErr(client.List(ctx, &tunnelCertificates, ctrlclient.InNamespace("tunnel-test")))
	is.NoErr(client.List(ctx, &tunnelRequests, ctrlclient.InNamespace("tunnel-test")))
	is.Equal(len(tunnelCertificates.Items), 0)
	is.Equal(len(tunnelRequests.Items), 0)
	// Changing an existing HTTPS management route stops its old renewal resource.
	publicSettings.Network.Cloudflare.Exposure = "tunnel"
	is.NoErr(reconcileManagementPublicRoute(ctx, client, publicSettings))
	is.NoErr(reconcileManagementPublicRoute(ctx, client, publicSettings))
	checkRoute(fmt.Sprint(httpPort), managementHost, http.StatusOK)
	is.True(apierrors.IsNotFound(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&managementCertificate), &certmanagerv1.Certificate{})))
	_, err = run.reconcileTraefik(ctx, releases, definition)
	is.NoErr(err)
	is.NoErr(client.Get(ctx, key, &bundled))
	is.Equal(bundled.UID, identity)
	is.Equal(bundled.Generation, generation)
}
