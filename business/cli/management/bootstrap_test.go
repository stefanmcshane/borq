package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"k8s.io/client-go/kubernetes/fake"
)

// TestDefaultRequest leaves credential resolution to bootstrap and sets lightweight server defaults.
func TestDefaultRequest(t *testing.T) {
	is := is.New(t)
	request := DefaultRequest()
	is.Equal(request.Mode, "prod")
	is.Equal(request.ClusterName, "management")
	is.Equal(request.Namespace, "borq")
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
		{"namespace", func(r Request) Request { r.Namespace = "borq-system"; return r }, "borq namespace"},
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
		})
	}
	request := DefaultRequest()
	request.KubeconfigPath = "/existing/kubeconfig"
	request.Mode = "prod"
	request.OpenbaoUrl = "https://vault.example.com"
	request.TelemetryUrl = "https://telemetry.example.com"
	is.New(t).NoErr(request.validate())
	request.ProbeURL = ""
	is.New(t).Equal(request.withDefaults().ProbeURL, "http://localhost:31833/api/healthz")
}

// TestApiUrlDerivesHealth checks API paths and production defaults without
// requiring callers to provide a separate probe or component endpoint.
func TestApiUrlDerivesHealth(t *testing.T) {
	assert := is.New(t)
	request := Request{ApiUrl: "https://borq.example.test/platform/", KubeconfigPath: "/existing/kubeconfig", Mode: "prod"}.withDefaults()
	assert.Equal(request.ProbeURL, "https://borq.example.test/platform/api/healthz")
	assert.NoErr(request.validate())
	defaultProduction := Request{Mode: "prod", KubeconfigPath: "/existing/kubeconfig"}.withDefaults()
	assert.Equal(defaultProduction.ProbeURL, "http://localhost:31833/api/healthz")
	assert.NoErr(defaultProduction.validate())
	for _, endpoint := range []string{"file:///tmp/borq", "https://user:secret@borq.test", "https://borq.test?token=secret", "https://borq.test/#fragment"} {
		request.ApiUrl = endpoint
		assert.True(request.validate() != nil)
	}
}

// TestManagementAddressDiscovery keeps a CLI loopback URL separate from the
// node address that managed charts need, and requires no Docker executable.
func TestManagementAddressDiscovery(t *testing.T) {
	assert := is.New(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/nodes")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","items":[{"metadata":{"name":"local"},"status":{"addresses":[{"type":"InternalIP","address":"172.21.0.2"}]}}]}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: management\n  cluster:\n    server: %s\ncontexts:\n- name: management\n  context:\n    cluster: management\n    user: management\ncurrent-context: management\nusers:\n- name: management\n  user: {}\n", server.URL)
	assert.NoErr(os.WriteFile(path, []byte(document), 0o600))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path}, logger.Logger{})
	assert.NoErr(err)
	request := Request{ApiUrl: "http://127.0.0.1:17833", KubeconfigPath: path, NodePort: 31835}.withDefaults()
	resolved, err := resolveManagementAddresses(context.Background(), request, connection)
	assert.NoErr(err)
	assert.Equal(resolved.ApiUrl, "http://127.0.0.1:17833")
	assert.Equal(resolved.advertisedApiUrl, "http://172.21.0.2:31835")
	assert.Equal(resolved.OpenbaoUrl, "")
	assert.Equal(resolved.TelemetryUrl, "")
	assert.Equal(calls, 1)
	request.ApiUrl = "https://borq.example.test/prefix/"
	resolved, err = resolveManagementAddresses(context.Background(), request, connection)
	assert.NoErr(err)
	assert.Equal(resolved.advertisedApiUrl, "https://borq.example.test/prefix")
	assert.Equal(calls, 1)
	request.TelemetryNodePort = 31836
	resolved, err = resolveManagementAddresses(context.Background(), request, connection)
	assert.NoErr(err)
	assert.Equal(resolved.TelemetryUrl, "http://borq.example.test:31836")
	assert.Equal(calls, 1)
}

// TestManagementNodeApiUrl handles IPv6 and missing addresses when discovering
// target-reachable defaults, preferring private node addresses over external ones.
func TestManagementNodeApiUrl(t *testing.T) {
	assert := is.New(t)
	nodes := []corev1.Node{{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeExternalIP, Address: "192.0.2.2"},
		{Type: corev1.NodeInternalIP, Address: "fd00::2"},
	}}}}
	address, err := managementNodeApiUrl(nodes, 31833)
	assert.NoErr(err)
	assert.Equal(address, "http://[fd00::2]:31833")
	_, err = managementNodeApiUrl(nil, 31833)
	assert.True(err != nil)
	_, err = managementNodeApiUrl([]corev1.Node{{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}}}}, 31833)
	assert.True(err != nil)
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
			client := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-management", Namespace: "borq"}, Data: map[string]string{"state.json": test.state}})
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
	request.OpenbaoUrl = "http://management.example:8200"
	request.TelemetryUrl = "http://management.example:7833"
	deployment := borqDeployment(request)
	is.Equal(deployment.Namespace, "borq")
	is.Equal(string(deployment.Spec.Strategy.Type), "Recreate")
	is.Equal(deployment.Spec.Template.Spec.ServiceAccountName, borqServiceAccountName)
	is.True(deployment.Spec.Template.Spec.EnableServiceLinks != nil)
	is.True(!*deployment.Spec.Template.Spec.EnableServiceLinks)
	container := deployment.Spec.Template.Spec.Containers[0]
	is.Equal(container.Image, borqImage)
	args := strings.Join(container.Args, " ")
	for _, expected := range []string{"--management-enabled", "--infrastructure-enabled", "--telemetry-port=7834", "--infrastructure-profile=prod", "--management-openbao-url=http://management.example:8200", "--management-telemetry-url=http://management.example:7833", "--infrastructure-storage-class=borq-local"} {
		is.True(strings.Contains(args, expected))
	}
	is.True(!strings.Contains(args, "--management-kubeconfig"))
	is.Equal(container.ReadinessProbe.HTTPGet.Path, "/api/readyz")
	is.Equal(container.Ports[1].Name, "telemetry")
	is.Equal(container.Ports[1].ContainerPort, int32(7834))
	is.Equal(container.VolumeMounts[0].MountPath, borqDataMountPath)
	is.Equal(container.VolumeMounts[1].MountPath, "/tmp")
	is.True(deployment.Spec.Template.Spec.Volumes[1].EmptyDir != nil)
	is.Equal(deployment.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, borqDataClaimName)
	is.Equal(container.Resources.Requests.Memory().String(), "64Mi")
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
