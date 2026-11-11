//go:build integration

package management

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestBootstrapKindAccessIntegration verifies real kind image import and service
// proxy access before pod readiness, using an isolated cluster and a local image.
// It does not install Borq's platform controllers or claim their readiness.
func TestBootstrapKindAccessIntegration(t *testing.T) {
	file, archive := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG"), os.Getenv("BORQ_BOOTSTRAP_TEST_IMAGE_ARCHIVE")
	if file == "" || archive == "" {
		t.Skip("isolated kind kubeconfig and nginx image archive are required")
	}
	assert := is.New(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	request, err := resolveBootstrapFiles(Request{ClusterName: "installation-name", KubeconfigPath: file, ServerImageArchivePath: archive}, "unused")
	assert.NoErr(err)
	assert.Equal(request.localRuntime, "kind")
	wrongConfig, err := clientcmd.LoadFromFile(file)
	assert.NoErr(err)
	wrongConfig.Clusters[wrongConfig.Contexts[wrongConfig.CurrentContext].Cluster].CertificateAuthorityData = []byte("different-cluster")
	wrongPath := filepath.Join(t.TempDir(), "different-cluster")
	assert.NoErr(clientcmd.WriteToFile(*wrongConfig, wrongPath))
	wrongRequest := request
	wrongRequest.KubeconfigPath = wrongPath
	err = importServerImage(ctx, wrongRequest)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "does not match the local kind cluster"))
	assert.NoErr(importServerImage(ctx, request))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: file}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	// Create, never adopt, the fixture namespace so an existing installation is untouched.
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "borq-mgmt"}}, metav1.CreateOptions{})
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assert.NoErr(client.CoreV1().Namespaces().Delete(cleanup, "borq-mgmt", metav1.DeleteOptions{}))
	})
	request, err = prepareAdminAuthentication(ctx, request, connection)
	assert.NoErr(err)
	firstToken := request.adminToken
	request, err = prepareAdminAuthentication(ctx, request, connection)
	assert.NoErr(err)
	assert.True(request.adminToken == firstToken)
	secret, err := client.CoreV1().Secrets("borq-mgmt").Get(ctx, "borq-admin-auth", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(string(secret.Data["token"]) == firstToken)
	_, err = client.CoreV1().ConfigMaps("borq-mgmt").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-backend"},
		Data:       map[string]string{"default.conf": `server { listen 7833; location = /api/mounted { alias /var/run/borq/admin-auth/token; } location /api/ { return 200 "bootstrap:$http_authorization:$http_x_borq_admin_token"; } }`},
	}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Pods("borq-mgmt").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-backend", Labels: map[string]string{"app": "bootstrap-backend"}},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "admin-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "borq-admin-auth"}}}, {Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "bootstrap-backend"}}}}},
			Containers: []corev1.Container{{Name: "backend", Image: request.BorqImage, ImagePullPolicy: corev1.PullNever,
				VolumeMounts:   []corev1.VolumeMount{{Name: "config", MountPath: "/etc/nginx/conf.d"}, {Name: "admin-auth", MountPath: "/var/run/borq/admin-auth", ReadOnly: true}},
				ReadinessProbe: &corev1.Probe{InitialDelaySeconds: 600, ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: borqHealthPath, Port: intstr.FromInt32(7833)}}},
			}},
		},
	}, metav1.CreateOptions{})
	assert.NoErr(err)
	_, err = client.CoreV1().Services("borq-mgmt").Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "borq"},
		Spec:       corev1.ServiceSpec{PublishNotReadyAddresses: true, Selector: map[string]string{"app": "bootstrap-backend"}, Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(7833)}}},
	}, metav1.CreateOptions{})
	assert.NoErr(err)
	request, err = connectBootstrapApi(request, connection)
	assert.NoErr(err)
	assert.NoErr(probeBorq(ctx, request.ProbeURL, request.probeClient))
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, request.ProbeURL, nil)
	assert.NoErr(err)
	// Kind's client certificate authenticates this request; the API server must
	// still strip an accompanying Authorization header before reaching Borq.
	probe.Header.Set("Authorization", "Bearer bootstrap-forwarding-marker")
	response, err := request.probeClient.Do(probe)
	assert.NoErr(err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	assert.NoErr(err)
	assert.True(string(body) == "bootstrap::"+firstToken)
	mounted, err := request.probeClient.Get(request.ApiUrl + "/api/mounted")
	assert.NoErr(err)
	mountedToken, err := io.ReadAll(mounted.Body)
	assert.NoErr(err)
	assert.NoErr(mounted.Body.Close())
	assert.True(string(mountedToken) == firstToken)
	pod, err := client.CoreV1().Pods("borq-mgmt").Get(ctx, "bootstrap-backend", metav1.GetOptions{})
	assert.NoErr(err)
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			assert.Equal(condition.Status, corev1.ConditionFalse)
		}
	}
}
