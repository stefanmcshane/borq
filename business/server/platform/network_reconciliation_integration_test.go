//go:build integration

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestNetworkIngressTransitionIntegration proves that an interrupted hostname
// request retains its stale-status fence through a fresh connection and retry.
// Assigned statuses are fixture input to the persistence guard, not evidence of
// Tailscale controller success; this test never contacts Tailscale.
func TestNetworkIngressTransitionIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, client, settings, kubeconfig := networkReconciliationFixture(t, ctx)
	settings.Network.Tailscale.ManagementHostname = "original"
	for _, desired := range []*networkingv1.Ingress{managementNetworkIngress(settings)} {
		is.NoErr(ensureNetworkIngress(ctx, client, settings, desired))
		premature := desired.DeepCopy()
		premature.Spec.TLS[0].Hosts = []string{"premature"}
		err := ensureNetworkIngress(ctx, client, settings, premature)
		is.True(err != nil && strings.Contains(err.Error(), "must be assigned"))
		is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), desired))
		desired.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{Hostname: desired.Spec.TLS[0].Hosts[0] + ".test.ts.net"}}
		is.NoErr(client.Status().Update(ctx, desired))
	}
	settings.Network.Tailscale.ManagementHostname = "replacement"
	settings.Timeout = 500 * time.Millisecond
	_, err := ReconcileManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.True(errors.Is(err, context.DeadlineExceeded))
	// Start with a new API connection after the request was persisted, as a
	// restarted worker would. The unchanged old statuses must remain unreadable.
	reopened, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	is.NoErr(err)
	client, err = networkClient(reopened)
	is.NoErr(err)
	state, err := ReadManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.NoErr(err)
	is.Equal(state, NetworkState{})
	_, err = ReconcileManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.True(errors.Is(err, context.DeadlineExceeded))
	third, err := normalizeSettings(settings)
	is.NoErr(err)
	third.Network.Tailscale.ManagementHostname = "third"
	err = ensureNetworkIngress(ctx, client, third, managementNetworkIngress(third))
	is.True(err != nil && strings.Contains(err.Error(), "still pending"))
	for _, desired := range []*networkingv1.Ingress{managementNetworkIngress(settings)} {
		is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), desired))
		is.Equal(desired.Spec.TLS[0].Hosts[0], map[string]string{"borq-management": "replacement"}[desired.Name])
		is.True(strings.Contains(desired.Annotations["borq.run/previous-tailnet-url"], "original"))
		desired.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{Hostname: desired.Spec.TLS[0].Hosts[0] + ".test.ts.net"}}
		is.NoErr(client.Status().Update(ctx, desired))
	}
	state, err = ReadManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.NoErr(err)
	is.Equal(state, NetworkState{}) // Assignment alone is not verified HTTPS reachability.
	state, err = readManagementNetwork(ctx, client, settings, false)
	is.NoErr(err)
	is.Equal(state, NetworkState{ManagementURL: "https://replacement.test.ts.net"})
	is.NoErr(ensureNetworkIngress(ctx, client, third, managementNetworkIngress(third)))
	state, err = ReadManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: third})
	is.NoErr(err)
	is.Equal(state.ManagementURL, "")
	foreign := managementNetworkIngress(settings)
	foreign.Name, foreign.Labels = "unowned", nil
	is.NoErr(client.Create(ctx, foreign))
	desired := managementNetworkIngress(third)
	desired.Name = foreign.Name
	err = ensureNetworkIngress(ctx, client, third, desired)
	is.True(err != nil && strings.Contains(err.Error(), "not owned"))
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(foreign), foreign))
	is.Equal(foreign.Spec.TLS[0].Hosts, []string{"replacement"})
	is.Equal(foreign.Annotations["borq.run/previous-tailnet-url"], "")
}

// TestNetworkProxyClassIntegration verifies the official CRD preserves resource
// settings through admission and repeated apply. It does not assert tailnet routing.
func TestNetworkProxyClassIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, client, settings, _ := networkReconciliationFixture(t, ctx)
	desired := networkProxyClass()
	desired.SetName(settings.Namespace)
	is.NoErr(ensureNetworkResource(ctx, client, desired))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := client.Delete(cleanup, desired); err != nil {
			t.Errorf("remove isolated proxy class: %v", err)
		}
	})
	live := networkProxyClass()
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live))
	identity := live.GetUID()
	is.NoErr(ensureNetworkResource(ctx, client, desired))
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(desired), live))
	is.Equal(live.GetUID(), identity)
	for _, resource := range []struct{ container, request, limit string }{
		{"tailscaleContainer", "64Mi", "256Mi"},
		{"tailscaleInitContainer", "16Mi", "64Mi"},
	} {
		for field, expected := range map[string]string{"requests": resource.request, "limits": resource.limit} {
			value, found, err := unstructured.NestedString(live.Object, "spec", "statefulSet", "pod", resource.container, "resources", field, "memory")
			is.NoErr(err)
			is.True(found)
			is.Equal(value, expected)
		}
	}
}

// TestNetworkComponentValuesIntegration owns automatic network configuration
// updates against real Helm storage and live manifests. Its ConfigMap-only chart
// proves settings application without pretending to run external controllers.
func TestNetworkComponentValuesIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection, client, settings, kubeconfig := networkReconciliationFixture(t, ctx)
	chart := t.TempDir()
	is.NoErr(os.Mkdir(filepath.Join(chart, "templates"), 0700))
	is.NoErr(os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: network-fixture\nversion: 0.1.0\n"), 0600))
	is.NoErr(os.WriteFile(filepath.Join(chart, "templates", "configuration.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\ndata:\n  values: {{ .Values | toJson | quote }}\n"), 0600))
	for _, definition := range []component{externalDnsComponent(), tailscaleComponent()} {
		t.Run(definition.Name, func(t *testing.T) {
			is := is.New(t)
			definition.Chart, definition.Repository, definition.Version, definition.Namespace = chart, "", "0.1.0", settings.Namespace
			releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfig})
			is.NoErr(err)
			copied, err := normalizeSettings(settings)
			is.NoErr(err)
			input := valuesInput{Settings: copied, NetworkOwner: "fixture-owner"}
			first, err := reconcileComponent(ctx, releases, definition, input, false, false)
			is.NoErr(err)
			is.Equal(first.Revision, 1)
			input.Network.Cloudflare.WildcardDomain, input.Network.Cloudflare.ZoneID = "changed.example.com", strings.Repeat("b", 32)
			input.Network.Tailscale.ManagementOperatorTag, input.Network.Tailscale.ProxyTag = "tag:changed-operator", "tag:changed-proxy"
			values, err := renderComponentValues(definition, input)
			is.NoErr(err)
			inspection, err := releases.InspectWithValues(ctx, helm.InstallOrUpgradeInput{Namespace: settings.Namespace, ReleaseName: definition.Name, ChartRef: chart, Version: definition.Version, Values: values, PodLabels: componentPodLabels()})
			is.NoErr(err)
			change, err := componentChange(definition, inspection, values, false)
			is.NoErr(err)
			is.Equal(change.Action, "upgrade")
			changed, err := reconcileComponent(ctx, releases, definition, input, false, false)
			is.NoErr(err)
			is.Equal(changed.Revision, 2)
			is.True(changed.Changed)
			is.NoErr(releases.Close())
			releases, err = helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfig})
			is.NoErr(err)
			defer releases.Close()
			repeated, err := reconcileComponent(ctx, releases, definition, input, false, false)
			is.NoErr(err)
			is.Equal(repeated.Revision, 2)
			is.True(!repeated.Changed)
			live := &corev1.ConfigMap{}
			is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: definition.Name}, live))
			values = map[string]any{}
			is.NoErr(json.Unmarshal([]byte(live.Data["values"]), &values))
			if definition.Name == "external-dns" {
				is.Equal(values["domainFilters"], []any{"changed.example.com"})
				is.Equal(values["extraArgs"].(map[string]any)["zone-id-filter"], strings.Repeat("b", 32))
			} else {
				is.Equal(values["operatorConfig"].(map[string]any)["defaultTags"], []any{"tag:changed-operator"})
				is.Equal(values["proxyConfig"].(map[string]any)["defaultTags"], "tag:changed-proxy")
			}
			definition.Version = "0.2.0"
			deferred, err := reconcileComponent(ctx, releases, definition, input, false, false)
			is.NoErr(err)
			is.True(deferred.UpgradeAvailable && !deferred.Changed)
			input.Network.Cloudflare.WildcardDomain, input.Network.Tailscale.ProxyTag = "third.example.com", "tag:third-proxy"
			values, err = renderComponentValues(definition, input)
			is.NoErr(err)
			inspection, err = releases.InspectWithValues(ctx, helm.InstallOrUpgradeInput{Namespace: settings.Namespace, ReleaseName: definition.Name, ChartRef: chart, Version: definition.Version, Values: values, PodLabels: componentPodLabels()})
			is.NoErr(err)
			change, err = componentChange(definition, inspection, values, false)
			is.NoErr(err)
			is.Equal(change.Action, "conflict")
			_, err = reconcileComponent(ctx, releases, definition, input, false, false)
			is.True(err != nil && strings.Contains(err.Error(), "explicit "+definition.Name+" component upgrade"))
			unchanged := &corev1.ConfigMap{}
			is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(live), unchanged))
			is.Equal(unchanged.Data, live.Data)
		})
	}
}

func networkReconciliationFixture(t *testing.T, ctx context.Context) (kubernetes.Connection, ctrlclient.Client, Settings, string) {
	t.Helper()
	kubeconfig := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	is := is.New(t)
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	is.NoErr(err)
	client, err := networkClient(connection)
	is.NoErr(err)
	namespace := fmt.Sprintf("borq-network-retry-%d", time.Now().UnixNano())
	is.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Errorf("remove isolated networking namespace: %v", err)
		}
	})
	settings, err := normalizeSettings(Settings{Namespace: namespace, Network: networkTestSettings(), Timeout: 20 * time.Second})
	is.NoErr(err)
	return connection, client, settings, kubeconfig
}
