package managementbootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/app"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

func TestDefaultRequest(t *testing.T) {
	is := is.New(t)

	request := DefaultRequest()

	is.Equal(request.ClusterName, defaultClusterName)
	is.Equal(request.HostPort, int32(defaultHostPort))
	is.Equal(request.Namespace, defaultNamespace)
	is.Equal(request.NodePort, int32(defaultNodePort))
	is.Equal(request.ProbeURL, probeURLForHostPort(defaultHostPort))
	is.Equal(request.Timeout, defaultTimeout)
	is.Equal(request.WhoamiImage, whoamiImage)
	is.Equal(request.VaultImage, vaultImage)
}

func TestRequestWithDefaults(t *testing.T) {
	is := is.New(t)

	request := Request{}.withDefaults()

	is.Equal(request.ClusterName, defaultClusterName)
	is.Equal(request.HostPort, int32(defaultHostPort))
	is.Equal(request.Namespace, defaultNamespace)
	is.Equal(request.NodePort, int32(defaultNodePort))
	is.Equal(request.ProbeURL, probeURLForHostPort(defaultHostPort))
	is.Equal(request.Timeout, defaultTimeout)
	is.Equal(request.WhoamiImage, whoamiImage)
	is.Equal(request.VaultImage, vaultImage)
}

func TestRequestValidate(t *testing.T) {
	testCases := []struct {
		name    string
		request Request
		wantErr string
	}{
		{
			name:    "empty cluster name",
			request: Request{HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "cluster name must not be empty",
		},
		{
			name:    "empty namespace",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "namespace must not be empty",
		},
		{
			name:    "empty probe url",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, Timeout: time.Minute, WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "probe URL must not be empty",
		},
		{
			name:    "non-positive host port",
			request: Request{ClusterName: defaultClusterName, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "host port must be greater than zero",
		},
		{
			name:    "non-positive node port",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "node port must be greater than zero",
		},
		{
			name:    "non-positive timeout",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), WhoamiImage: whoamiImage, VaultImage: vaultImage},
			wantErr: "timeout must be greater than zero",
		},
		{
			name:    "empty whoami image",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, VaultImage: vaultImage},
			wantErr: "whoami image must not be empty",
		},
		{
			name:    "empty vault image",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, WhoamiImage: whoamiImage},
			wantErr: "vault image must not be empty",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			is := is.New(t)
			err := testCase.request.validate()
			is.True(err != nil)
			is.Equal(err.Error(), testCase.wantErr)
		})
	}
}

func TestRenderDefaultKindConfigYAML(t *testing.T) {
	is := is.New(t)

	renderedConfig, err := RenderDefaultKindConfigYAML(defaultHostPort, defaultNodePort)
	is.NoErr(err)

	renderedText := string(renderedConfig)
	is.True(strings.Contains(renderedText, "kind.x-k8s.io/v1alpha4"))
	is.True(strings.Contains(renderedText, "hostPort: 7833"))
	is.True(strings.Contains(renderedText, "containerPort: 31833"))
}

func TestHasCluster(t *testing.T) {
	is := is.New(t)

	output := "default\nborq-management\nanother-cluster\n"

	is.True(hasCluster(output, "borq-management"))
	is.True(!hasCluster(output, "missing"))
}

func TestKindContextName(t *testing.T) {
	is := is.New(t)

	is.Equal(kindContextName("borq-management"), "kind-borq-management")
}

func TestProbeURLForHostPort(t *testing.T) {
	is := is.New(t)

	is.Equal(probeURLForHostPort(7833), "http://localhost:7833")
}

func TestWhoamiApp(t *testing.T) {
	is := is.New(t)

	request := DefaultRequest()
	managedApp := whoamiApp(request)

	is.Equal(managedApp.Workload.Kind, app.DeploymentWorkload)
	is.Equal(len(managedApp.Services), 1)
	is.Equal(managedApp.Services[0].Name, whoamiName)
	is.Equal(managedApp.Services[0].Type, kubernetes.NodePortService)
	is.Equal(managedApp.Workload.Deployment.Name, whoamiName)
	is.Equal(managedApp.Workload.Deployment.Pods.Containers[0].Image, whoamiImage)
}

func TestVaultApp(t *testing.T) {
	is := is.New(t)

	request := DefaultRequest()
	managedApp := vaultApp(request)

	is.Equal(managedApp.Workload.Kind, app.StatefulSetWorkload)
	is.Equal(len(managedApp.Services), 2)
	is.Equal(managedApp.Workload.StatefulSet.Name, vaultName)
	is.Equal(managedApp.Workload.StatefulSet.Pods.Containers[0].Image, vaultImage)
	is.Equal(managedApp.Workload.StatefulSet.Pods.Containers[0].Args[1], "-dev")
	is.True(managedApp.Services[1].Headless)
}

func TestManagementNamespace(t *testing.T) {
	is := is.New(t)

	namespace := managementNamespace("borq-system")
	is.Equal(namespace.Name, "borq-system")
	is.Equal(namespace.Labels["app.kubernetes.io/managed-by"], "borq")
}

func TestBootstrapApps(t *testing.T) {
	is := is.New(t)

	apps := bootstrapApps(DefaultRequest())
	is.Equal(len(apps), 2)
	is.Equal(apps[0].Name, whoamiName)
	is.Equal(apps[1].Name, vaultName)
}

func TestAppLabelsAndSelectors(t *testing.T) {
	is := is.New(t)

	labels := appLabels("whoami", "liveness")
	selector := selectorLabels("whoami")
	podLabelSet := podLabels(labels, "whoami")

	is.Equal(labels["app.kubernetes.io/component"], "liveness")
	is.Equal(selector["app.kubernetes.io/name"], "whoami")
	is.Equal(podLabelSet["app.kubernetes.io/name"], "whoami")
}

func TestProbeWhoami(t *testing.T) {
	is := is.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := probeWhoami(ctx, server.URL)
	is.NoErr(err)
}

func TestProbeWhoamiTimeout(t *testing.T) {
	is := is.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := probeWhoami(ctx, server.URL)
	is.True(err != nil)
}
