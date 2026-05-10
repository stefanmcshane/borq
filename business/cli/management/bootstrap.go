package management

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"

	kindconfig "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/yaml"
)

const (
	defaultClusterName     = "borq-management"
	defaultNamespace       = "borq-system"
	defaultHostPort        = 7833
	defaultNodePort        = 31833
	defaultTimeout         = 10 * time.Minute
	defaultFieldOwner      = "borq-management-bootstrap"
	borqImage              = "ghcr.io/stefanmcshane/borq:latest"
	borqName               = "borq"
	borqConfigName         = "borq-config"
	borqServiceAccountName = "borq"
	borqRoleName           = "borq"
	borqRoleBindingName    = "borq"
	borqDataMountPath      = "/var/lib/borq"
	borqContainerPort      = 7833
	borqHealthPath         = "/healthz"
	lxcKubeadmConfigPatch  = `kind: KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
featureGates:
  KubeletInUserNamespace: true
`
)

// Request describes a management bootstrap run.
type Request struct {
	ClusterName    string
	HostPort       int32
	Namespace      string
	NodePort       int32
	ProbeURL       string
	Timeout        time.Duration
	BorqImage      string
	KubeconfigPath string
}

// Result captures the outcome of a management bootstrap run.
type Result struct {
	ClusterName string
	ProbeURL    string
	Reused      bool
}

// DefaultRequest returns the default management bootstrap request.
func DefaultRequest() Request {
	return Request{
		ClusterName: defaultClusterName,
		HostPort:    defaultHostPort,
		Namespace:   defaultNamespace,
		NodePort:    defaultNodePort,
		ProbeURL:    probeURLForHostPort(defaultHostPort),
		Timeout:     defaultTimeout,
		BorqImage:   borqImage,
	}
}

// Execute runs the local management bootstrap workflow.
func Execute(ctx context.Context, log logger.Logger, request Request) (Result, error) {
	request = request.withDefaults()
	if err := request.validate(); err != nil {
		return Result{}, err
	}

	log.Info("starting_management_bootstrap", logger.WithFields(
		logger.String("cluster_name", request.ClusterName),
		logger.String("namespace", request.Namespace),
		logger.String("probe_url", request.ProbeURL),
		logger.Duration("timeout", request.Timeout),
	))

	timeoutContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()

	clusterExists, err := kindClusterExists(timeoutContext, request.ClusterName)
	if err != nil {
		return Result{}, err
	}

	if clusterExists {
		log.Info("reusing_existing_kind_management_cluster", logger.WithFields(
			logger.String("cluster_name", request.ClusterName),
		))
	} else {
		if runningInLXCContainer() {
			log.Info("enabling_kind_kubelet_user_namespace_support")
		}

		log.Info("creating_kind_management_cluster", logger.WithFields(
			logger.String("cluster_name", request.ClusterName),
		))
		if err := createKindCluster(timeoutContext, request); err != nil {
			return Result{}, err
		}
	}

	connection, err := kubernetes.NewConnection(
		kubernetes.Config{KubeconfigPath: kubeconfigPath(request)},
		log,
		kubernetes.WithFieldOwner(defaultFieldOwner),
	)
	if err != nil {
		return Result{}, err
	}
	bootstrapApps := bootstrapApps(request)
	if err := runBootstrapPhases(timeoutContext, bootstrapApps, bootstrapPhaseRunner{
		applyNamespace: func(ctx context.Context) error {
			return connection.Namespaces().Apply(ctx, managementNamespace(request.Namespace))
		},
		applyApp: func(ctx context.Context, managedApp App) error {
			return managedApp.Apply(ctx, connection)
		},
		waitForAppReady: func(ctx context.Context, managedApp App) error {
			return managedApp.WaitReady(ctx, connection)
		},
	}); err != nil {
		return Result{}, err
	}

	log.Info("management_apps_ready")
	log.Info("probing_management_liveness_endpoint", logger.WithFields(
		logger.String("probe_url", request.ProbeURL),
	))
	if err := probeBorq(timeoutContext, request.ProbeURL); err != nil {
		return Result{}, err
	}

	log.Info("management_bootstrap_completed", logger.WithFields(
		logger.String("cluster_name", request.ClusterName),
		logger.String("probe_url", request.ProbeURL),
		logger.Bool("reused", clusterExists),
	))

	return Result{ClusterName: request.ClusterName, ProbeURL: request.ProbeURL, Reused: clusterExists}, nil
}

type bootstrapPhaseRunner struct {
	applyNamespace  func(context.Context) error
	applyApp        func(context.Context, App) error
	waitForAppReady func(context.Context, App) error
}

func runBootstrapPhases(ctx context.Context, bootstrapApps []App, runner bootstrapPhaseRunner) error {
	if err := runner.applyNamespace(ctx); err != nil {
		return err
	}
	for _, managedApp := range bootstrapApps {
		if err := runner.applyApp(ctx, managedApp); err != nil {
			return err
		}
	}
	for _, managedApp := range bootstrapApps {
		if err := runner.waitForAppReady(ctx, managedApp); err != nil {
			return err
		}
	}

	return nil
}

func (request Request) withDefaults() Request {
	if request.ClusterName == "" {
		request.ClusterName = defaultClusterName
	}
	if request.HostPort == 0 {
		request.HostPort = defaultHostPort
	}
	if request.Namespace == "" {
		request.Namespace = defaultNamespace
	}
	if request.NodePort == 0 {
		request.NodePort = defaultNodePort
	}
	if request.ProbeURL == "" {
		request.ProbeURL = probeURLForHostPort(request.HostPort)
	}
	if request.Timeout == 0 {
		request.Timeout = defaultTimeout
	}
	if request.BorqImage == "" {
		request.BorqImage = borqImage
	}

	return request
}

func (request Request) validate() error {
	if strings.TrimSpace(request.ClusterName) == "" {
		return errors.New("cluster name must not be empty")
	}
	if strings.TrimSpace(request.Namespace) == "" {
		return errors.New("namespace must not be empty")
	}
	if request.HostPort <= 0 {
		return errors.New("host port must be greater than zero")
	}
	if request.NodePort <= 0 {
		return errors.New("node port must be greater than zero")
	}
	if request.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	if strings.TrimSpace(request.ProbeURL) == "" {
		return errors.New("probe URL must not be empty")
	}
	if strings.TrimSpace(request.BorqImage) == "" {
		return errors.New("borq image must not be empty")
	}

	return nil
}

func kindClusterExists(ctx context.Context, clusterName string) (bool, error) {
	output, err := runCommand(ctx, "kind", "get", "clusters")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, errors.New("kind binary is required but was not found on PATH")
		}
		return false, fmt.Errorf("get kind clusters: %w", err)
	}

	return hasCluster(output, clusterName), nil
}

func createKindCluster(ctx context.Context, request Request) error {
	renderedConfig, err := renderKindConfigYAML(request.HostPort, request.NodePort, runningInLXCContainer())
	if err != nil {
		return err
	}

	temporaryFile, err := os.CreateTemp("", "borq-management-kind-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary kind config: %w", err)
	}
	defer temporaryFile.Close()

	if _, err := temporaryFile.Write(renderedConfig); err != nil {
		return fmt.Errorf("write temporary kind config: %w", err)
	}

	configPath := temporaryFile.Name()
	defer func() {
		_ = os.Remove(configPath)
	}()

	_, err = runCommand(ctx, "kind", "create", "cluster", "--name", request.ClusterName, "--config", configPath)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("kind binary is required but was not found on PATH")
		}
		return fmt.Errorf("create kind cluster %q: %w", request.ClusterName, err)
	}

	return nil
}

// DefaultKindClusterConfig returns the borq default management cluster config.
func DefaultKindClusterConfig(hostPortValue int32, nodePortValue int32) kindconfig.Cluster {
	return defaultKindClusterConfig(hostPortValue, nodePortValue, false)
}

func defaultKindClusterConfig(hostPortValue int32, nodePortValue int32, enableLXCUserNamespace bool) kindconfig.Cluster {
	cluster := kindconfig.Cluster{
		TypeMeta: kindconfig.TypeMeta{
			Kind:       "Cluster",
			APIVersion: "kind.x-k8s.io/v1alpha4",
		},
		Nodes: []kindconfig.Node{
			{
				Role: kindconfig.ControlPlaneRole,
				ExtraPortMappings: []kindconfig.PortMapping{
					{
						ContainerPort: nodePortValue,
						HostPort:      hostPortValue,
						ListenAddress: "0.0.0.0",
						Protocol:      kindconfig.PortMappingProtocolTCP,
					},
				},
			},
		},
	}

	if enableLXCUserNamespace {
		cluster.KubeadmConfigPatches = []string{lxcKubeadmConfigPatch}
	}

	return cluster
}

// RenderDefaultKindConfigYAML renders the default management cluster config as YAML.
func RenderDefaultKindConfigYAML(hostPortValue int32, nodePortValue int32) ([]byte, error) {
	return renderKindConfigYAML(hostPortValue, nodePortValue, false)
}

func renderKindConfigYAML(hostPortValue int32, nodePortValue int32, enableLXCUserNamespace bool) ([]byte, error) {
	cluster := defaultKindClusterConfig(hostPortValue, nodePortValue, enableLXCUserNamespace)
	renderedConfig, err := yaml.Marshal(cluster)
	if err != nil {
		return nil, fmt.Errorf("render default kind config: %w", err)
	}

	return renderedConfig, nil
}

func probeURLForHostPort(hostPort int32) string {
	return fmt.Sprintf("http://localhost:%d%s", hostPort, borqHealthPath)
}

func kubeconfigPath(request Request) string {
	if request.KubeconfigPath != "" {
		return request.KubeconfigPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

func kindContextName(clusterName string) string {
	return fmt.Sprintf("kind-%s", clusterName)
}

func bootstrapApps(request Request) []App {
	return []App{
		borqApp(request),
	}
}

func managementNamespace(namespace string) kubernetes.Namespace {
	return kubernetes.Namespace{
		Name: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":       "borq-management-bootstrap",
			"app.kubernetes.io/managed-by": "borq",
		},
	}
}

func borqApp(request Request) App {
	labels := appLabels(borqName, "server")
	selector := selectorLabels(borqName)

	return App{
		Name:      borqName,
		Namespace: request.Namespace,
		Labels:    labels,
		ConfigMaps: []kubernetes.ConfigMap{
			{
				Name:      borqConfigName,
				Namespace: request.Namespace,
				Labels:    appLabels(borqName, "config"),
				Data: map[string]string{
					"health_path": borqHealthPath,
					"data_dir":    borqDataMountPath,
				},
			},
		},
		ServiceAccounts: []kubernetes.ServiceAccount{
			{
				Name:      borqServiceAccountName,
				Namespace: request.Namespace,
				Labels:    appLabels(borqName, "rbac"),
			},
		},
		Roles: []kubernetes.Role{
			{
				Name:      borqRoleName,
				Namespace: request.Namespace,
				Labels:    appLabels(borqName, "rbac"),
				Rules: []kubernetes.PolicyRule{
					{
						APIGroups: []string{"*"},
						Resources: []string{"*"},
						Verbs:     []string{"*"},
					},
				},
			},
		},
		RoleBindings: []kubernetes.RoleBinding{
			{
				Name:      borqRoleBindingName,
				Namespace: request.Namespace,
				Labels:    appLabels(borqName, "rbac"),
				RoleRef: kubernetes.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "Role",
					Name:     borqRoleName,
				},
				Subjects: []kubernetes.Subject{
					{
						Kind:      "ServiceAccount",
						Name:      borqServiceAccountName,
						Namespace: request.Namespace,
					},
				},
			},
		},
		Services: []kubernetes.Service{
			{
				Name:      borqName,
				Namespace: request.Namespace,
				Labels:    labels,
				Selector:  selector,
				Type:      kubernetes.NodePortService,
				Ports: []kubernetes.ServicePort{
					{Name: "http", Port: 80, TargetPort: borqContainerPort, NodePort: request.NodePort},
				},
			},
		},
		Workload: Workload{
			Kind: DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name:      borqName,
				Namespace: request.Namespace,
				Labels:    labels,
				Selector:  selector,
				Replicas:  1,
				Pods: kubernetes.PodTemplate{
					Labels:             podLabels(labels, borqName),
					ServiceAccountName: borqServiceAccountName,
					Containers: []kubernetes.Container{
						{
							Name:  borqName,
							Image: request.BorqImage,
							Args:  []string{"management", "server", "--port=7833", "--data-dir=" + borqDataMountPath},
							Ports: []kubernetes.ContainerPort{{Name: "http", Port: borqContainerPort}},
							ReadinessProbe: kubernetes.HttpProbe{
								Path:                borqHealthPath,
								Port:                borqContainerPort,
								InitialDelaySeconds: 1,
								PeriodSeconds:       2,
							},
						},
					},
				},
			},
		},
	}
}

func appLabels(name string, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "borq",
	}
}

func selectorLabels(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": name}
}

func podLabels(labels map[string]string, name string) map[string]string {
	clonedLabels := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		clonedLabels[key] = value
	}
	clonedLabels["app.kubernetes.io/name"] = name
	return clonedLabels
}

func probeBorq(ctx context.Context, probeURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	return probeBorqWithClient(ctx, client, probeURL)
}

func probeBorqWithClient(ctx context.Context, client *http.Client, probeURL string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return fmt.Errorf("build probe request: %w", err)
		}

		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("probe %s did not succeed before timeout: %w", probeURL, ctx.Err())
		case <-ticker.C:
		}
	}
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", err
		}

		commandLine := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText == "" {
			return "", fmt.Errorf("%s failed: %w", commandLine, err)
		}

		return "", fmt.Errorf("%s failed: %w: %s", commandLine, err, stderrText)
	}

	return stdout.String(), nil
}

func hasCluster(output string, clusterName string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true
		}
	}

	return false
}

func runningInLXCContainer() bool {
	if output, err := runCommand(context.Background(), "systemd-detect-virt", "--container"); err == nil {
		if strings.EqualFold(strings.TrimSpace(output), "lxc") {
			return true
		}
	}

	return runningInLXCContainerWithSignals(
		os.Getenv("container"),
		string(readFileIfPresent("/run/systemd/container")),
		readFileIfPresent("/proc/1/environ"),
		readFileIfPresent("/proc/1/cgroup"),
	)
}

func runningInLXCContainerWithSignals(containerEnvValue string, systemdContainerValue string, initEnviron []byte, initCgroup []byte) bool {
	if containsLXCSignal(containerEnvValue) {
		return true
	}
	if containsLXCSignal(systemdContainerValue) {
		return true
	}
	if bytes.Contains(bytes.ToLower(initEnviron), []byte("container=lxc")) {
		return true
	}
	if lxcCgroupPattern.Match(initCgroup) {
		return true
	}

	return false
}

var lxcCgroupPattern = regexp.MustCompile(`(^|/)(lxc|liblxc)(/|$)`)

func containsLXCSignal(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "lxc")
}

func readFileIfPresent(path string) []byte {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	return contents
}
