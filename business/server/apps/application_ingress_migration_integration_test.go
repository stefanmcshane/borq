//go:build integration

package apps

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Real controllers must accept the replacement and issue its certificate before
// retirement. Kubernetes' garbage collector must preserve that TLS identity.
func TestApplicationIngressMigrationIntegration(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	defer cancel()
	clusterName := fmt.Sprintf("borq-ingress-%d", time.Now().UnixNano())
	command := func(args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", args[0], args[1], err, output)
		}
		return output
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if output, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", clusterName).CombinedOutput(); err != nil {
			t.Errorf("remove migration fixture: %v\n%s", err, output)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	address := listener.Addr().String()
	assert.NoErr(listener.Close())
	command("k3d", "cluster", "create", clusterName, "--image", "rancher/k3s:v1.35.3-k3s1", "--servers", "1", "--no-lb", "--api-port", address,
		"--volume", "/dev/null:/dev/kmsg@server:*", "--k3s-arg", "--disable=traefik@server:*", "--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*",
		"--k3s-arg", "--kube-proxy-arg=conntrack-max-per-core=0@server:*", "--k3s-arg", "--kubelet-arg=eviction-hard=nodefs.available<1%,imagefs.available<1%,nodefs.inodesFree<1%@server:*",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--timeout", "120s")
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(kubeconfig, command("k3d", "kubeconfig", "get", clusterName), 0600))
	command("kubectl", "--kubeconfig", kubeconfig, "apply", "--server-side", "-f", "../platform/values/gateway-api.yaml")
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfig})
	assert.NoErr(err)
	defer releases.Close()
	assert.NoErr(releases.InstallOrUpgrade(ctx, helm.InstallOrUpgradeInput{Namespace: "cert-manager", ReleaseName: "cert-manager", ChartRef: "cert-manager", RepositoryUrl: "https://charts.jetstack.io", Version: "v1.21.1", Timeout: 3 * time.Minute, Values: map[string]any{"crds": map[string]any{"enabled": true}}}))
	assert.NoErr(releases.InstallOrUpgrade(ctx, helm.InstallOrUpgradeInput{Namespace: "traefik", ReleaseName: "borq-traefik", ChartRef: "traefik", RepositoryUrl: "https://traefik.github.io/charts", Version: "42.0.0", Timeout: 3 * time.Minute, Values: map[string]any{
		"gateway": map[string]any{"enabled": false}, "gatewayClass": map[string]any{"enabled": false},
		"providers": map[string]any{"kubernetesIngress": map[string]any{"enabled": false}, "kubernetesCRD": map[string]any{"enabled": false}, "kubernetesGateway": map[string]any{"enabled": true}},
	}}))
	target := Cluster{KubeconfigPath: kubeconfig}
	client, err := networkDnsClient(target)
	assert.NoErr(err)
	assert.NoErr(client.Create(ctx, &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "borq-traefik"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "traefik.io/gateway-controller"}}))
	assert.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}))
	assert.NoErr(client.Create(ctx, &certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "migration"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{SelfSigned: &certmanagerv1.SelfSignedIssuer{}}}}))
	ingress := legacyApplicationIngress("apps", "space-one")
	assert.NoErr(client.Create(ctx, &ingress))
	configuration, err := Merge(Configuration{}, patchFromJson(t, applicationRouteTestPatch("web.example.com")))
	assert.NoErr(err)
	rendered, err := RenderForPlan(configuration, "apps", "space-one", "migration", "migration")
	assert.NoErr(err)
	certificate := decodeResource[certmanagerv1.Certificate](t, findManifest(t, rendered, "Certificate", "web-public-tls"))
	certificate.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&ingress, networkingv1.SchemeGroupVersion.WithKind("Ingress"))}
	assert.NoErr(client.Create(ctx, &certificate))
	certificateUID := certificate.UID
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
	stored := spaceRecord{Space: Space{ID: "space-one"}, Manifests: []map[string]any{applicationTunnelManifest(t, legacyApplicationIngress("apps", "space-one"))}}
	_, retired, err := manager.captureApplicationIngressRetirements(ctx, stored, target, rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(len(retired), 1)
	saved := planRecord{Plan: Plan{Space: stored.Space, Manifests: rendered.Manifests}, Connection: target, RetiredIngresses: retired}
	assert.True(manager.retireApplicationIngresses(ctx, saved) != nil)
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&ingress), &networkingv1.Ingress{}))
	applications, err := newClusterClient(target)
	assert.NoErr(err)
	assert.NoErr(applications.Apply(ctx, rendered.Manifests, "space-one"))
	waitApplicationTunnelResources(t, ctx, applications, rendered.Manifests, "space-one")
	key := ctrlclient.ObjectKey{Namespace: "apps", Name: "web-public-tls"}
	var secret corev1.Secret
	assert.NoErr(client.Get(ctx, key, &secret))
	secretUID := secret.UID
	assert.NoErr(manager.retireApplicationIngresses(ctx, saved))
	assert.NoErr(wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(&ingress), &networkingv1.Ingress{})
		return apierrors.IsNotFound(err), nil
	}))
	assert.NoErr(client.Get(ctx, key, &certificate))
	assert.Equal(certificate.UID, certificateUID)
	assert.Equal(len(certificate.OwnerReferences), 0)
	assert.NoErr(client.Get(ctx, key, &secret))
	assert.Equal(secret.UID, secretUID)
	assert.NoErr(manager.retireApplicationIngresses(ctx, saved))
	replacement := ingress.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "", ""
	assert.NoErr(client.Create(ctx, replacement))
	assert.True(manager.retireApplicationIngresses(ctx, saved) != nil)
	assert.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(replacement), &networkingv1.Ingress{}))
}
