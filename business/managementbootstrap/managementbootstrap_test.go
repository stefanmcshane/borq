package managementbootstrap

import (
	"context"
	"io"
	"net/http"
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
	is.Equal(request.BorqImage, borqImage)
	is.Equal(request.StorageSize, defaultStorageSize)
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
	is.Equal(request.BorqImage, borqImage)
	is.Equal(request.StorageSize, defaultStorageSize)
}

func TestRequestValidate(t *testing.T) {
	testCases := []struct {
		name            string
		request         Request
		wantErr         string
		wantErrContains string
	}{
		{
			name:    "empty cluster name",
			request: Request{HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "cluster name must not be empty",
		},
		{
			name:    "empty namespace",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "namespace must not be empty",
		},
		{
			name:    "empty probe url",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, Timeout: time.Minute, BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "probe URL must not be empty",
		},
		{
			name:    "non-positive host port",
			request: Request{ClusterName: defaultClusterName, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "host port must be greater than zero",
		},
		{
			name:    "non-positive node port",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "node port must be greater than zero",
		},
		{
			name:    "non-positive timeout",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), BorqImage: borqImage, StorageSize: defaultStorageSize},
			wantErr: "timeout must be greater than zero",
		},
		{
			name:    "empty borq image",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, StorageSize: defaultStorageSize},
			wantErr: "borq image must not be empty",
		},
		{
			name:    "empty storage size",
			request: Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage},
			wantErr: "storage size must not be empty",
		},
		{
			name:            "invalid storage size",
			request:         Request{ClusterName: defaultClusterName, HostPort: defaultHostPort, Namespace: defaultNamespace, NodePort: defaultNodePort, ProbeURL: probeURLForHostPort(defaultHostPort), Timeout: time.Minute, BorqImage: borqImage, StorageSize: "not-a-size"},
			wantErrContains: "parse storage size: quantities must match the regular expression",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			is := is.New(t)
			err := testCase.request.validate()
			is.True(err != nil)
			if testCase.wantErr != "" {
				is.Equal(err.Error(), testCase.wantErr)
			}
			if testCase.wantErrContains != "" {
				is.True(strings.Contains(err.Error(), testCase.wantErrContains))
			}
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
	is.True(!strings.Contains(renderedText, "KubeletInUserNamespace"))
}

func TestRenderKindConfigYAMLWithLXCUserNamespace(t *testing.T) {
	is := is.New(t)

	renderedConfig, err := renderKindConfigYAML(defaultHostPort, defaultNodePort, true)
	is.NoErr(err)

	renderedText := string(renderedConfig)
	is.True(strings.Contains(renderedText, "kubeadmConfigPatches:"))
	is.True(strings.Contains(renderedText, "KubeletInUserNamespace: true"))
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

func TestContainsLXCSignal(t *testing.T) {
	is := is.New(t)

	is.True(containsLXCSignal("lxc"))
	is.True(containsLXCSignal("LXC"))
	is.True(containsLXCSignal("container=lxc"))
	is.True(!containsLXCSignal("docker"))
	is.True(!containsLXCSignal(""))
}

func TestRunningInLXCContainerWithSignals(t *testing.T) {
	testCases := []struct {
		name                  string
		containerEnvValue     string
		systemdContainerValue string
		initEnviron           []byte
		initCgroup            []byte
		want                  bool
	}{
		{
			name:              "container env",
			containerEnvValue: "lxc",
			want:              true,
		},
		{
			name:                  "systemd container file",
			systemdContainerValue: "lxc",
			want:                  true,
		},
		{
			name:        "proc 1 environ",
			initEnviron: []byte("PATH=/usr/bin\x00container=lxc\x00HOME=/root"),
			want:        true,
		},
		{
			name:       "proc 1 cgroup",
			initCgroup: []byte("12:memory:/lxc/container"),
			want:       true,
		},
		{
			name:       "proc 1 cgroup exact boundary match",
			initCgroup: []byte("1:name=systemd:/foo/lxc/bar"),
			want:       true,
		},
		{
			name:       "proc 1 cgroup liblxc",
			initCgroup: []byte("12:memory:/liblxc/container"),
			want:       true,
		},
		{
			name:       "non lxc",
			initCgroup: []byte("12:memory:/lxc.payload/container"),
			want:       false,
		},
		{
			name:       "non lxc docker",
			initCgroup: []byte("12:memory:/docker/container"),
			want:       false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			is := is.New(t)
			is.Equal(
				runningInLXCContainerWithSignals(
					testCase.containerEnvValue,
					testCase.systemdContainerValue,
					testCase.initEnviron,
					testCase.initCgroup,
				),
				testCase.want,
			)
		})
	}
}

func TestProbeURLForHostPort(t *testing.T) {
	is := is.New(t)

	is.Equal(probeURLForHostPort(7833), "http://localhost:7833/healthz")
}

func TestBorqDataClaim(t *testing.T) {
	is := is.New(t)

	request := DefaultRequest()
	persistentVolumeClaim := borqDataClaim(request)

	is.Equal(persistentVolumeClaim.Name, borqDataClaimName)
	is.Equal(persistentVolumeClaim.AccessModes, []kubernetes.PersistentVolumeAccessMode{kubernetes.ReadWriteOnceVolume})
	is.Equal(persistentVolumeClaim.RequestedStorage, defaultStorageSize)
}

func TestBorqApp(t *testing.T) {
	is := is.New(t)

	request := DefaultRequest()
	managedApp := borqApp(request)

	is.Equal(managedApp.Workload.Kind, app.DeploymentWorkload)
	is.Equal(len(managedApp.ConfigMaps), 1)
	is.Equal(managedApp.ConfigMaps[0].Name, borqConfigName)
	is.Equal(len(managedApp.ServiceAccounts), 1)
	is.Equal(managedApp.ServiceAccounts[0].Name, borqServiceAccountName)
	is.Equal(len(managedApp.Roles), 1)
	is.Equal(managedApp.Roles[0].Name, borqRoleName)
	is.Equal(managedApp.Roles[0].Rules[0].APIGroups, []string{"*"})
	is.Equal(managedApp.Roles[0].Rules[0].Resources, []string{"*"})
	is.Equal(managedApp.Roles[0].Rules[0].Verbs, []string{"*"})
	is.Equal(len(managedApp.RoleBindings), 1)
	is.Equal(managedApp.RoleBindings[0].Name, borqRoleBindingName)
	is.Equal(len(managedApp.Services), 1)
	is.Equal(managedApp.Services[0].Name, borqName)
	is.Equal(managedApp.Services[0].Type, kubernetes.NodePortService)
	is.Equal(managedApp.Workload.Deployment.Name, borqName)
	is.Equal(managedApp.Workload.Deployment.Pods.ServiceAccountName, borqServiceAccountName)
	is.Equal(managedApp.Workload.Deployment.Pods.Containers[0].Image, borqImage)
	is.Equal(managedApp.Workload.Deployment.Pods.Volumes[0].PersistentVolumeClaimName, borqDataClaimName)
	is.Equal(managedApp.Workload.Deployment.Pods.Containers[0].VolumeMounts[0].MountPath, borqDataMountPath)
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
	is.Equal(len(apps), 1)
	is.Equal(apps[0].Name, borqName)
}

func TestRunBootstrapPhasesOrder(t *testing.T) {
	is := is.New(t)

	events := make([]string, 0, 6)
	bootstrapApps := []app.App{{Name: "borq"}}

	err := runBootstrapPhases(context.Background(), bootstrapApps, bootstrapPhaseRunner{
		applyNamespace: func(context.Context) error {
			events = append(events, "namespace")
			return nil
		},
		applyPersistentVolumeClaim: func(context.Context) error {
			events = append(events, "pvc-apply")
			return nil
		},
		applyApp: func(_ context.Context, managedApp app.App) error {
			events = append(events, "app-apply:"+managedApp.Name)
			return nil
		},
		waitForPersistentVolumeClaimBound: func(context.Context) error {
			events = append(events, "pvc-bound")
			return nil
		},
		waitForAppReady: func(_ context.Context, managedApp app.App) error {
			events = append(events, "app-ready:"+managedApp.Name)
			return nil
		},
	})
	is.NoErr(err)
	is.Equal(events, []string{
		"namespace",
		"pvc-apply",
		"app-apply:borq",
		"pvc-bound",
		"app-ready:borq",
	})
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

func TestProbeBorq(t *testing.T) {
	is := is.New(t)

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := probeBorqWithClient(ctx, client, "http://borq.test/healthz")
	is.NoErr(err)
}

func TestProbeBorqTimeout(t *testing.T) {
	is := is.New(t)

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("bad gateway")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := probeBorqWithClient(ctx, client, "http://borq.test/healthz")
	is.True(err != nil)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
