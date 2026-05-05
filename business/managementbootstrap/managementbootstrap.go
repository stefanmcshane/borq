package managementbootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/app"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"

	kindconfig "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/yaml"
)

const (
	defaultClusterName = "borq-management"
	defaultNamespace   = "borq-system"
	defaultHostPort    = 7833
	defaultNodePort    = 31833
	defaultTimeout     = 10 * time.Minute
	defaultFieldOwner  = "borq-management-bootstrap"
	whoamiImage        = "traefik/whoami:latest"
	vaultImage         = "hashicorp/vault:1.21.2"
	whoamiName         = "whoami"
	vaultName          = "vault"
	vaultRootToken     = "borq-root"
	vaultInternalName  = "vault-internal"
)

// Request describes a management bootstrap run.
type Request struct {
	ClusterName string
	HostPort    int32
	Namespace   string
	NodePort    int32
	ProbeURL    string
	Timeout     time.Duration
	WhoamiImage string
	VaultImage  string
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
		WhoamiImage: whoamiImage,
		VaultImage:  vaultImage,
	}
}

// Execute runs the local management bootstrap workflow.
func Execute(ctx context.Context, log logger.Logger, request Request) (Result, error) {
	request = request.withDefaults()
	if err := request.validate(); err != nil {
		return Result{}, err
	}

	log = log.Named("management-bootstrap")
	log.Info(
		"starting management bootstrap",
		logger.String("cluster_name", request.ClusterName),
		logger.String("namespace", request.Namespace),
		logger.String("probe_url", request.ProbeURL),
		logger.Duration("timeout", request.Timeout),
	)

	timeoutContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()

	clusterExists, err := kindClusterExists(timeoutContext, request.ClusterName)
	if err != nil {
		return Result{}, err
	}

	if clusterExists {
		log.Info("reusing existing kind management cluster", logger.String("cluster_name", request.ClusterName))
	} else {
		log.Info("creating kind management cluster", logger.String("cluster_name", request.ClusterName))
		if err := createKindCluster(timeoutContext, request); err != nil {
			return Result{}, err
		}
	}

	connection, err := kubernetes.NewConnection(
		kubernetes.Config{ContextName: kindContextName(request.ClusterName)},
		log.Named("kubernetes"),
		kubernetes.WithFieldOwner(defaultFieldOwner),
	)
	if err != nil {
		return Result{}, err
	}

	if err := connection.Namespaces().Apply(timeoutContext, managementNamespace(request.Namespace)); err != nil {
		return Result{}, err
	}

	bootstrapApps := bootstrapApps(request)
	for _, managedApp := range bootstrapApps {
		if err := managedApp.Apply(timeoutContext, connection); err != nil {
			return Result{}, err
		}
	}

	for _, managedApp := range bootstrapApps {
		if err := managedApp.WaitReady(timeoutContext, connection); err != nil {
			return Result{}, err
		}
	}

	log.Info("management apps are ready")
	log.Info("probing management liveness endpoint", logger.String("probe_url", request.ProbeURL))
	if err := probeWhoami(timeoutContext, request.ProbeURL); err != nil {
		return Result{}, err
	}

	log.Info(
		"management bootstrap completed",
		logger.String("cluster_name", request.ClusterName),
		logger.String("probe_url", request.ProbeURL),
		logger.Bool("reused", clusterExists),
	)

	return Result{ClusterName: request.ClusterName, ProbeURL: request.ProbeURL, Reused: clusterExists}, nil
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
	if request.WhoamiImage == "" {
		request.WhoamiImage = whoamiImage
	}
	if request.VaultImage == "" {
		request.VaultImage = vaultImage
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
	if strings.TrimSpace(request.WhoamiImage) == "" {
		return errors.New("whoami image must not be empty")
	}
	if strings.TrimSpace(request.VaultImage) == "" {
		return errors.New("vault image must not be empty")
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
	renderedConfig, err := RenderDefaultKindConfigYAML(request.HostPort, request.NodePort)
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

func probeURLForHostPort(hostPort int32) string {
	return fmt.Sprintf("http://localhost:%d", hostPort)
}

func kindContextName(clusterName string) string {
	return fmt.Sprintf("kind-%s", clusterName)
}

// DefaultKindClusterConfig returns the borq default management cluster config.
func DefaultKindClusterConfig(hostPortValue int32, nodePortValue int32) kindconfig.Cluster {
	return kindconfig.Cluster{
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
}

// RenderDefaultKindConfigYAML renders the default management cluster config as YAML.
func RenderDefaultKindConfigYAML(hostPortValue int32, nodePortValue int32) ([]byte, error) {
	cluster := DefaultKindClusterConfig(hostPortValue, nodePortValue)
	renderedConfig, err := yaml.Marshal(cluster)
	if err != nil {
		return nil, fmt.Errorf("render default kind config: %w", err)
	}

	return renderedConfig, nil
}

func bootstrapApps(request Request) []app.App {
	return []app.App{
		whoamiApp(request),
		vaultApp(request),
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

func whoamiApp(request Request) app.App {
	labels := appLabels(whoamiName, "liveness")
	selector := selectorLabels(whoamiName)

	return app.App{
		Name:      whoamiName,
		Namespace: request.Namespace,
		Labels:    labels,
		Services: []kubernetes.Service{
			{
				Name:      whoamiName,
				Namespace: request.Namespace,
				Labels:    labels,
				Selector:  selector,
				Type:      kubernetes.NodePortService,
				Ports: []kubernetes.ServicePort{
					{Name: "http", Port: request.HostPort, TargetPort: 80, NodePort: request.NodePort},
				},
			},
		},
		Workload: app.Workload{
			Kind: app.DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name:      whoamiName,
				Namespace: request.Namespace,
				Labels:    labels,
				Selector:  selector,
				Replicas:  1,
				Pods: kubernetes.PodTemplate{
					Labels: podLabels(labels, whoamiName),
					Containers: []kubernetes.Container{
						{
							Name:  whoamiName,
							Image: request.WhoamiImage,
							Ports: []kubernetes.ContainerPort{{Name: "http", Port: 80}},
						},
					},
				},
			},
		},
	}
}

func vaultApp(request Request) app.App {
	labels := appLabels(vaultName, "secrets")
	selector := selectorLabels(vaultName)

	return app.App{
		Name:      vaultName,
		Namespace: request.Namespace,
		Labels:    labels,
		Services: []kubernetes.Service{
			{
				Name:      vaultName,
				Namespace: request.Namespace,
				Labels:    labels,
				Selector:  selector,
				Type:      kubernetes.ClusterIPService,
				Ports:     []kubernetes.ServicePort{{Name: "http", Port: 8200, TargetPort: 8200}},
			},
			{
				Name:                     vaultInternalName,
				Namespace:                request.Namespace,
				Labels:                   labels,
				Selector:                 selector,
				Type:                     kubernetes.ClusterIPService,
				Headless:                 true,
				PublishNotReadyAddresses: true,
				Ports:                    []kubernetes.ServicePort{{Name: "http", Port: 8200, TargetPort: 8200}},
			},
		},
		Workload: app.Workload{
			Kind: app.StatefulSetWorkload,
			StatefulSet: kubernetes.StatefulSet{
				Name:        vaultName,
				Namespace:   request.Namespace,
				Labels:      labels,
				Selector:    selector,
				Replicas:    1,
				ServiceName: vaultInternalName,
				Pods: kubernetes.PodTemplate{
					Labels: podLabels(labels, vaultName),
					Containers: []kubernetes.Container{
						{
							Name:    vaultName,
							Image:   request.VaultImage,
							Command: []string{"vault"},
							Args: []string{
								"server",
								"-dev",
								"-dev-root-token-id=" + vaultRootToken,
								"-dev-listen-address=0.0.0.0:8200",
							},
							Env:   []kubernetes.EnvVar{{Name: "VAULT_ADDR", Value: "http://127.0.0.1:8200"}},
							Ports: []kubernetes.ContainerPort{{Name: "http", Port: 8200}},
							ReadinessProbe: kubernetes.HttpProbe{
								Path:                "/v1/sys/health?standbyok=true&sealedcode=204&uninitcode=204",
								Port:                8200,
								InitialDelaySeconds: 2,
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

func probeWhoami(ctx context.Context, probeURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
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
