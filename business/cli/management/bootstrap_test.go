package management

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

// TestDefaultRequest leaves credential resolution to bootstrap and sets lightweight server defaults.
func TestDefaultRequest(t *testing.T) {
	is := is.New(t)
	request := DefaultRequest()
	is.Equal(request.Mode, "prod")
	is.Equal(request.ClusterName, "management")
	is.Equal(request.Namespace, "borq-mgmt")
	is.Equal(request.KubeconfigPath, "")
	is.Equal(request.StorageClass, "borq-local")
	is.Equal(request.HostPort, int32(7833))
	is.Equal(request.OpenbaoHostPort, int32(8200))
	is.Equal(request.TelemetryHostPort, int32(7834))
	is.Equal(request.TelemetryNodePort, int32(31834))
	is.True(strings.Contains(request.validate().Error(), "kubeconfig path must not be empty"))
}

// TestRequestValidate rejects invalid server settings before Kubernetes is changed.
func TestRequestValidate(t *testing.T) {
	cases := []struct {
		name   string
		change func(Request) Request
		want   string
	}{
		{"runtime", func(r Request) Request { r.localRuntime = "other"; return r }, "runtime must be"},
		{"profile", func(r Request) Request { r.Mode = "other"; return r }, "component mode must be"},
		{"cluster", func(r Request) Request { r.ClusterName = "../other"; return r }, "paths or spaces"},
		{"namespace", func(r Request) Request { r.Namespace = "borq-system"; return r }, "borq-mgmt namespace"},
		{"host port", func(r Request) Request { r.HostPort = -1; return r }, "host port must"},
		{"vault port", func(r Request) Request { r.OpenbaoHostPort = 65536; return r }, "OpenBao host port"},
		{"node port", func(r Request) Request { r.NodePort = 80; return r }, "node port must"},
		{"telemetry host port", func(r Request) Request { r.TelemetryHostPort = 65536; return r }, "telemetry host port must"},
		{"telemetry node port", func(r Request) Request { r.TelemetryNodePort = 80; return r }, "telemetry node port must"},
		{"duplicate host ports", func(r Request) Request { r.TelemetryHostPort = r.HostPort; return r }, "host ports must be distinct"},
		{"duplicate node ports", func(r Request) Request { r.TelemetryNodePort = r.NodePort; return r }, "node ports must be distinct"},
		{"timeout", func(r Request) Request { r.Timeout = -time.Second; return r }, "timeout must"},
		{"image", func(r Request) Request { r.BorqImage = " "; return r }, "image must"},
		{"invalid image", func(r Request) Request { r.BorqImage = "invalid image:tag"; return r }, "valid image reference"},
		{"endpoint", func(r Request) Request { r.OpenbaoUrl = "ftp://vault"; return r }, "invalid management endpoint"},
		{"S3 endpoint", func(r Request) Request { r.S3Endpoint = "ftp://storage"; return r }, "invalid management endpoint"},
		{"S3 credentials", func(r Request) Request { r.S3Endpoint = "https://key:secret@storage.test"; return r }, "invalid management endpoint"},
		{"S3 path", func(r Request) Request { r.S3Endpoint = "https://storage.test/bucket"; return r }, "origin without a path"},
		{"kubeconfig path", func(r Request) Request { r.KubeconfigPath = ""; return r }, "kubeconfig path must not be empty"},
	}
	for _, current := range cases {
		t.Run(current.name, func(t *testing.T) {
			is := is.New(t)
			request := DefaultRequest()
			request.KubeconfigPath = "/existing/kubeconfig"
			err := current.change(request).validate()
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), current.want))
			if current.name == "S3 credentials" {
				is.True(!strings.Contains(err.Error(), "key:secret"))
			}
		})
	}
	request := DefaultRequest()
	request.KubeconfigPath = "/existing/kubeconfig"
	request.Mode = "prod"
	request.OpenbaoUrl = "https://vault.example.com"
	request.TelemetryUrl = "https://telemetry.example.com"
	is.New(t).NoErr(request.validate())
	request.ProbeURL = ""
	is.New(t).Equal(request.withDefaults().ProbeURL, "http://localhost:31833/api/readyz")
}

// TestApiUrlDerivesHealth checks API paths and production defaults without
// requiring callers to provide a separate probe or component endpoint.
func TestApiUrlDerivesHealth(t *testing.T) {
	assert := is.New(t)
	request := Request{ApiUrl: "https://borq.example.test/platform/", KubeconfigPath: "/existing/kubeconfig", Mode: "prod"}.withDefaults()
	assert.Equal(request.ProbeURL, "https://borq.example.test/platform/api/readyz")
	assert.NoErr(request.validate())
	defaultProduction := Request{Mode: "prod", KubeconfigPath: "/existing/kubeconfig"}.withDefaults()
	assert.Equal(defaultProduction.ProbeURL, "http://localhost:31833/api/readyz")
	assert.NoErr(defaultProduction.validate())
	for _, endpoint := range []string{"file:///tmp/borq", "https://user:secret@borq.test", "https://borq.test?token=secret", "https://borq.test/#fragment"} {
		request.ApiUrl = endpoint
		assert.True(request.validate() != nil)
	}
}

// TestBootstrapProfileGuard prevents replacing a running server with settings
// that would change the management stack's storage profile on its next startup.
func TestBootstrapProfileGuard(t *testing.T) {
	for _, test := range []struct {
		name, state, profile string
		wantError            bool
	}{
		{"same dev", `{"profile":"dev"}`, "dev", false},
		{"same prod", `{"profile":"production"}`, "prod", false},
		{"different", `{"profile":"production"}`, "dev", true},
		{"invalid state", "invalid json", "dev", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			client := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-management", Namespace: "borq-mgmt"}, Data: map[string]string{"state.json": test.state}})
			request := DefaultRequest()
			request.Mode = test.profile
			err := checkManagementMode(context.Background(), kubernetes.NewConnectionForTest(client), request)
			assert.Equal(err != nil, test.wantError)
			assert.Equal(len(client.Actions()), 1)
			assert.Equal(client.Actions()[0].GetVerb(), "get")
		})
	}
	assert := is.New(t)
	assert.NoErr(checkManagementMode(context.Background(), kubernetes.NewConnectionForTest(fake.NewSimpleClientset()), DefaultRequest()))
}

// TestBorqDeployment persists SQLite and enables component management using in-cluster credentials.
func TestBorqDeployment(t *testing.T) {
	is := is.New(t)
	request := DefaultRequest()
	request.ClusterName = "custom-management"
	request.OpenbaoUrl = "http://management.example:8200"
	request.TelemetryUrl = "http://management.example:7833"
	request.S3Endpoint = "http://storage.example:9000"
	deployment := borqDeployment(request)
	is.Equal(deployment.Namespace, "borq-mgmt")
	is.Equal(deployment.Spec.Strategy.Type, appsv1.RollingUpdateDeploymentStrategyType)
	is.Equal(*deployment.Spec.Strategy.RollingUpdate.MaxSurge, intstr.FromInt32(0))
	is.Equal(*deployment.Spec.Strategy.RollingUpdate.MaxUnavailable, intstr.FromInt32(1))
	is.Equal(deployment.Spec.Template.Spec.ServiceAccountName, borqServiceAccountName)
	is.True(deployment.Spec.Template.Spec.EnableServiceLinks != nil)
	is.True(!*deployment.Spec.Template.Spec.EnableServiceLinks)
	container := deployment.Spec.Template.Spec.Containers[0]
	is.Equal(container.Image, borqImage)
	args := strings.Join(container.Args, " ")
	for _, expected := range []string{"--management-enabled", "--management-bootstrap-only", "--management-cluster-name=custom-management", "--infrastructure-enabled", "--telemetry-port=7834", "--infrastructure-profile=prod", "--management-openbao-url=http://management.example:8200", "--management-telemetry-url=http://management.example:7833", "--infrastructure-storage-class=borq-local"} {
		is.True(strings.Contains(args, expected))
	}
	is.True(strings.Contains(args, "--management-s3-endpoint=http://storage.example:9000"))
	is.True(!strings.Contains(args, "--management-kubeconfig"))
	is.Equal(container.ReadinessProbe.HTTPGet.Path, "/api/readyz")
	is.Equal(container.Ports[1].Name, "telemetry")
	is.Equal(container.Ports[1].ContainerPort, int32(7834))
	is.Equal(container.VolumeMounts[0].MountPath, borqDataMountPath)
	is.Equal(container.VolumeMounts[1].MountPath, "/tmp")
	is.True(deployment.Spec.Template.Spec.Volumes[1].EmptyDir != nil)
	is.Equal(deployment.Spec.Template.Spec.Volumes[2].Secret.SecretName, "borq-admin-auth")
	is.Equal(container.VolumeMounts[2].MountPath, "/var/run/borq/admin-auth")
	is.True(container.VolumeMounts[2].ReadOnly)
	is.True(strings.Contains(args, "--admin-token-file=/var/run/borq/admin-auth/token"))
	is.Equal(deployment.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, borqDataClaimName)
	is.Equal(container.Resources.Requests.Memory().String(), "256Mi")
	is.Equal(localStorageClass().Object["provisioner"], "rancher.io/local-path")
	is.Equal(localStorageClass().Object["reclaimPolicy"], "Retain")
	is.Equal(borqClusterRole().Rules[0].Resources, []string{"*"})
}

// TestManagementLabels identifies Borq-owned management resources consistently.
func TestManagementLabels(t *testing.T) {
	is := is.New(t)
	namespace := managementNamespace("borq")
	is.Equal(namespace.Name, "borq")
	is.Equal(namespace.Labels["app.kubernetes.io/managed-by"], "borq")
	labels := appLabels("example", "server")
	is.Equal(selectorLabels("example")["app.kubernetes.io/name"], "example")
	is.Equal(podLabels(labels, "example")["app.kubernetes.io/component"], "server")
}

// TestBootstrapServices keeps the initial API internal until authenticated server configuration provisions access.
func TestBootstrapServices(t *testing.T) {
	assert := is.New(t)
	services := bootstrapServices(DefaultRequest())
	assert.Equal(len(services), 1)
	assert.Equal(services[0].Name, "borq")
	assert.Equal(services[0].Type, kubernetes.ClusterIPService)
	assert.Equal(services[0].Ports[0].NodePort, int32(0))
	assert.Equal(services[0].Ports[0].TargetPort, int32(7833))
	assert.True(!services[0].PublishNotReadyAddresses)
}

// TestBootstrapRetainsInstalledServer protects server-owned networking, credentials and storage when updating the server image.
func TestBootstrapRetainsInstalledServer(t *testing.T) {
	assert := is.New(t)
	installed := borqDeployment(DefaultRequest())
	container := &installed.Spec.Template.Spec.Containers[0]
	container.Image = "registry.test/borq:previous"
	container.Env = []corev1.EnvVar{{Name: "NETWORK_SETTING", Value: "preserved"}}
	container.Args = append(container.Args, "--network-config=/etc/borq/network/network.json")
	installed.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "retained-registry"}}
	request := DefaultRequest()
	request.BorqImage = "registry.test/borq:next"
	desired := borqDeployment(request)
	assert.NoErr(retainInstalledServer(desired, installed))
	updated := desired.Spec.Template.Spec.Containers[0]
	assert.Equal(updated.Image, request.BorqImage)
	assert.Equal(updated.Args, container.Args)
	assert.Equal(updated.Env, container.Env)
	assert.Equal(updated.VolumeMounts, container.VolumeMounts)
	assert.Equal(desired.Spec.Template.Spec.Volumes, installed.Spec.Template.Spec.Volumes)
	assert.Equal(desired.Spec.Template.Spec.ImagePullSecrets, installed.Spec.Template.Spec.ImagePullSecrets)
	assert.Equal(installed.Spec.Template.Spec.Containers[0].Image, "registry.test/borq:previous")
	installed.Labels = nil
	assert.True(retainInstalledServer(desired, installed) != nil)
}

func TestBootstrapAddsAdminAuthenticationToInstalledServer(t *testing.T) {
	assert := is.New(t)
	installed := borqDeployment(DefaultRequest())
	installed.Spec.Template.Spec.Volumes = installed.Spec.Template.Spec.Volumes[:2]
	container := &installed.Spec.Template.Spec.Containers[0]
	container.VolumeMounts = container.VolumeMounts[:2]
	container.Args = []string{"server", "--data-dir=/data", "--network-config=/etc/borq/network/network.json"}
	desired := borqDeployment(DefaultRequest())
	assert.NoErr(retainInstalledServer(desired, installed))
	updated := desired.Spec.Template.Spec.Containers[0]
	if !slices.Contains(updated.Args, "--admin-token-file=/var/run/borq/admin-auth/token") {
		t.Fatal("upgraded server does not read the bootstrap administrator token")
	}
	assert.True(slices.Contains(updated.Args, "--network-config=/etc/borq/network/network.json"))
	assert.True(slices.Contains(updated.VolumeMounts, corev1.VolumeMount{Name: "admin-auth", MountPath: "/var/run/borq/admin-auth", ReadOnly: true}))
	assert.Equal(len(desired.Spec.Template.Spec.Volumes), 3)
	assert.Equal(desired.Spec.Template.Spec.Volumes[2].Secret.SecretName, "borq-admin-auth")
	repeated := borqDeployment(DefaultRequest())
	assert.NoErr(retainInstalledServer(repeated, desired))
	assert.Equal(repeated.Spec.Template, desired.Spec.Template)
}

// TestBootstrapPreservesLegacyManagement protects running servers and retained
// state before bootstrap can replace their cluster-wide service account binding.
func TestBootstrapPreservesLegacyManagement(t *testing.T) {
	for _, input := range []struct {
		name   string
		object runtime.Object
	}{
		{"server", &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "borq", Namespace: "borq"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "borq"}}}}},
		{"volume", &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "borq-data", Namespace: "borq"}}},
		{"state", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-management", Namespace: "borq"}}},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			client := fake.NewSimpleClientset(input.object)
			err := runManagementBootstrapWorkflow(context.Background(), logger.Logger{}, DefaultRequest(), kubernetes.NewConnectionForTest(client))
			assert.True(err != nil)
			assert.True(strings.Contains(err.Error(), "migration is required before bootstrapping"))
			for _, action := range client.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
}
