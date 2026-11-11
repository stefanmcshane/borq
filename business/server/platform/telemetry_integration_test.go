//go:build integration

package platform

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/observability"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestSelectiveTelemetryIntegration runs the actual catalog collector against real
// Kubernetes and management stores. It proves named metrics ports OR complete Service
// annotations opt in endpoints, while node totals survive and unselected pod data is dropped.
func TestSelectiveTelemetryIntegration(t *testing.T) {
	assert := is.New(t)
	kubeconfig := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	configuration, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(configuration)
	assert.NoErr(err)
	management := ManagementNamespace
	installed, err := kube.AppsV1().Deployments(SystemNamespace).Get(ctx, "alloy", metav1.GetOptions{})
	assert.NoErr(err)
	image := installed.Spec.Template.Spec.Containers[0].Image
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	namespace := "borq-telemetry-" + suffix
	cluster := "telemetry-test-" + suffix
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = kube.RbacV1().ClusterRoleBindings().Delete(cleanup, namespace, metav1.DeleteOptions{})
		_ = kube.RbacV1().ClusterRoles().Delete(cleanup, namespace, metav1.DeleteOptions{})
	})
	values, err := renderComponentValues(component{Name: "alloy", Values: "alloy.yaml"}, valuesInput{Settings: Settings{Namespace: namespace, LokiURL: "http://loki." + management + ".svc:3100/loki/api/v1/push", PrometheusURL: "http://prometheus-server." + management + ".svc/api/v1/write"}, ClusterID: cluster})
	assert.NoErr(err)
	content := values["alloy"].(map[string]any)["configMap"].(map[string]any)["content"].(string)
	_, err = kube.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "collector"}, Data: map[string]string{"config.alloy": content}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "telemetry"}, StringData: map[string]string{"TELEMETRY_TOKEN": "integration-test"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "collector"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: namespace}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "pods/log", "services", "nodes", "events", "namespaces"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, Verbs: []string{"get"}},
	}}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: namespace}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: namespace}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "collector", Namespace: namespace}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	for _, name := range []string{"selected", "annotated", "unselected"} {
		labels := map[string]string{"fixture": name}
		portName := "http"
		if name == "selected" {
			labels["borq.run/logs"], labels["borq.run/metrics"] = "true", "true"
			labels["borq.run/addon-name"], labels["borq.run/space-id"] = "fixture", "test-space"
			portName = "metrics"
		}
		metric := "addon_fixture_" + name
		command := "echo '# TYPE " + metric + " gauge' > /usr/share/nginx/html/metrics; echo '" + metric + " 1' >> /usr/share/nginx/html/metrics; echo addon-telemetry-marker; exec nginx -g 'daemon off;'"
		_, err := kube.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "nginx:1.27.5", Command: []string{"/bin/sh", "-ec", command}, Ports: []corev1.ContainerPort{{Name: portName, ContainerPort: 80}}}}}}, metav1.CreateOptions{})
		assert.NoErr(err)
	}
	for _, name := range []string{"annotated", "unselected"} {
		annotations := map[string]string{"prometheus.io/scrape": "true"}
		if name == "annotated" {
			annotations["prometheus.io/port"], annotations["prometheus.io/path"] = "80", "/metrics"
		}
		_, err := kube.CoreV1().Services(namespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}, Spec: corev1.ServiceSpec{Selector: map[string]string{"fixture": name}, Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}, metav1.CreateOptions{})
		assert.NoErr(err)
	}
	_, err = kube.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "collector"}, Spec: corev1.PodSpec{ServiceAccountName: "collector", Containers: []corev1.Container{{Name: "alloy", Image: image, Args: []string{"run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy"}, VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/alloy"}, {Name: "telemetry", MountPath: "/var/run/borq-telemetry", ReadOnly: true}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}}}, Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "collector"}}}}, {Name: "telemetry", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "telemetry"}}}}}}, metav1.CreateOptions{})
	assert.NoErr(err)
	store, err := observability.NewManagementClient(kubeconfig, management)
	assert.NoErr(err)
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	nodeNames := map[string]bool{}
	for _, node := range nodes.Items {
		nodeNames[node.Name] = true
	}
	nodeSelector := `{borq_cluster=` + strconv.Quote(cluster) + `,job="borq-cadvisor",id="/"}`
	selector := `{borq_cluster=` + strconv.Quote(cluster) + `,namespace=` + strconv.Quote(namespace) + `}`
	started := time.Now().Add(-time.Minute)
	ready := false
	for ctx.Err() == nil {
		end := time.Now()
		metricsReady := true
		for _, query := range []string{"sum(addon_fixture_selected" + selector + ")", "sum(addon_fixture_annotated" + selector + ")", "sum(container_cpu_usage_seconds_total" + selector + ")", "sum(container_memory_working_set_bytes" + selector + ")"} {
			samples, err := store.QueryRange(ctx, observability.RangeInput{Query: query, Start: started, End: end, Step: 15 * time.Second})
			if err != nil || len(samples) == 0 {
				metricsReady = false
			}
		}
		for _, query := range []string{
			`max by (node) (rate(container_cpu_usage_seconds_total` + strings.TrimSuffix(nodeSelector, "}") + `,cpu="total"}[2m]))`,
			`max by (node) (container_memory_working_set_bytes` + nodeSelector + `)`,
		} {
			samples, err := store.Query(ctx, observability.QueryInput{Query: query, Time: end})
			if err != nil || len(samples) != len(nodeNames) {
				metricsReady = false
			}
			for _, sample := range samples {
				assert.True(nodeNames[sample.Labels["node"]])
				assert.True(sample.Sample.Value >= 0)
			}
		}
		logs, err := store.Logs(ctx, observability.LogsInput{Query: `{borq_cluster=` + strconv.Quote(cluster) + `,namespace=` + strconv.Quote(namespace) + `,borq_addon="fixture",borq_space="test-space"} |= "addon-telemetry-marker"`, Start: started, End: end, Limit: 10})
		if metricsReady && err == nil && len(logs) > 0 {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	if !ready {
		// Bound diagnostics to the collector, without dumping secrets or all cluster pods.
		diagnostics, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		tailLines := int64(50) // Kubernetes requires a pointer for an explicit log tail limit.
		logs, _ := kube.CoreV1().Pods(namespace).GetLogs("collector", &corev1.PodLogOptions{TailLines: &tailLines}).DoRaw(diagnostics)
		t.Fatalf("selected metrics and logs did not arrive: %s", string(logs))
	}
	for _, query := range []string{
		"sum(addon_fixture_unselected" + selector + ")",
		`sum(container_memory_working_set_bytes{borq_cluster=` + strconv.Quote(cluster) + `,namespace=` + strconv.Quote(namespace) + `,pod=~"unselected|annotated|collector"})`,
	} {
		samples, err := store.QueryRange(ctx, observability.RangeInput{Query: query, Start: started, End: time.Now(), Step: 15 * time.Second})
		assert.NoErr(err)
		assert.Equal(len(samples), 0)
	}
	logs, err := store.Logs(ctx, observability.LogsInput{Query: `{borq_cluster=` + strconv.Quote(cluster) + `,namespace=` + strconv.Quote(namespace) + `,pod=~"unselected|annotated|collector"}`, Start: started, End: time.Now(), Limit: 10})
	assert.NoErr(err)
	assert.Equal(len(logs), 0)
	// Selected usage retains the addon/space labels required by the management APIs.
	samples, err := store.QueryRange(ctx, observability.RangeInput{Query: `sum(container_memory_working_set_bytes{borq_cluster=` + strconv.Quote(cluster) + `,namespace=` + strconv.Quote(namespace) + `,borq_addon="fixture",borq_space="test-space"})`, Start: started, End: time.Now(), Step: 15 * time.Second})
	assert.NoErr(err)
	assert.True(len(samples) > 0)
}
