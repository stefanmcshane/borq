//go:build integration
// +build integration

package managementbootstrap

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	testRegistryContainer = "borq-kind-registry-it"
	testRegistryImage     = "registry:2"
)

func TestExecuteIntegration(t *testing.T) {
	requireIntegrationBinary(t, "kind")
	requireIntegrationBinary(t, "docker")
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon is required for the integration test")
	}

	hostPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("find free tcp port: %v", err)
	}
	registryPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("find free registry port: %v", err)
	}

	clusterName := "borq-management-it"
	registryHost := fmt.Sprintf("localhost:%d", registryPort)
	borqRegistryImage := fmt.Sprintf("%s/borq-it:latest", registryHost)

	kindConfigPath, err := writeIntegrationKindConfig(hostPort, registryPort)
	if err != nil {
		t.Fatalf("write integration kind config: %v", err)
	}

	t.Cleanup(func() {
		cleanupContext := context.Background()
		_, _ = runCommand(cleanupContext, "kind", "delete", "cluster", "--name", clusterName)
		_, _ = runCommand(cleanupContext, "docker", "rm", "-f", testRegistryContainer)
		_ = os.Remove(kindConfigPath)
	})

	testContext, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	if err := ensureLocalRegistry(testContext, registryPort); err != nil {
		t.Fatalf("ensure local registry: %v", err)
	}
	if err := createIntegrationCluster(testContext, clusterName, kindConfigPath); err != nil {
		t.Fatalf("create integration cluster: %v", err)
	}
	if err := connectRegistryToKind(testContext); err != nil {
		t.Fatalf("connect registry to kind network: %v", err)
	}
	if err := configureRegistryForKindNodes(testContext, clusterName, registryPort); err != nil {
		t.Fatalf("configure registry for kind nodes: %v", err)
	}
	if err := buildAndPushBorqImage(testContext, borqRegistryImage); err != nil {
		t.Fatalf("build borq image: %v", err)
	}

	request := DefaultRequest()
	request.ClusterName = clusterName
	request.HostPort = hostPort
	request.ProbeURL = ""
	request.Timeout = 5 * time.Minute
	request.BorqImage = borqRegistryImage

	is := is.New(t)

	result, err := Execute(testContext, logger.Logger{}, request)
	is.NoErr(err)
	is.Equal(result.ClusterName, clusterName)
	is.Equal(result.ProbeURL, probeURLForHostPort(hostPort))

	result, err = Execute(testContext, logger.Logger{}, request)
	is.NoErr(err)
	is.True(result.Reused)
}

func requireIntegrationBinary(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is required for the integration test", binary)
	}
}

func ensureLocalRegistry(ctx context.Context, registryPort int32) error {
	runningState, err := runCommand(ctx, "docker", "inspect", "-f", "{{.State.Running}}", testRegistryContainer)
	if err == nil && strings.TrimSpace(runningState) == "true" {
		return nil
	}

	_, err = runCommand(
		ctx,
		"docker",
		"run",
		"-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:5000", registryPort),
		"--network", "bridge",
		"--name", testRegistryContainer,
		testRegistryImage,
	)
	if err != nil {
		return fmt.Errorf("start local registry container: %w", err)
	}

	return nil
}

func createIntegrationCluster(ctx context.Context, clusterName string, configPath string) error {
	_, err := runCommand(ctx, "kind", "create", "cluster", "--name", clusterName, "--config", configPath)
	if err != nil {
		return fmt.Errorf("kind create cluster: %w", err)
	}

	return nil
}

func connectRegistryToKind(ctx context.Context) error {
	networkState, err := runCommand(ctx, "docker", "inspect", "-f", "{{json .NetworkSettings.Networks.kind}}", testRegistryContainer)
	if err == nil && strings.TrimSpace(networkState) != "null" {
		return nil
	}

	_, err = runCommand(ctx, "docker", "network", "connect", "kind", testRegistryContainer)
	if err != nil {
		return fmt.Errorf("docker network connect kind registry: %w", err)
	}

	return nil
}

func configureRegistryForKindNodes(ctx context.Context, clusterName string, registryPort int32) error {
	nodesOutput, err := runCommand(ctx, "kind", "get", "nodes", "--name", clusterName)
	if err != nil {
		return fmt.Errorf("get kind nodes: %w", err)
	}

	registryDirectory := fmt.Sprintf("/etc/containerd/certs.d/localhost:%d", registryPort)
	hostsToml := fmt.Sprintf("[host.\"http://%s:5000\"]\n", testRegistryContainer)

	for _, node := range strings.Split(strings.TrimSpace(nodesOutput), "\n") {
		if strings.TrimSpace(node) == "" {
			continue
		}

		if _, err := runCommand(ctx, "docker", "exec", node, "mkdir", "-p", registryDirectory); err != nil {
			return fmt.Errorf("create registry dir on node %q: %w", node, err)
		}
		if err := copyTextToContainer(ctx, node, filepath.Join(registryDirectory, "hosts.toml"), hostsToml); err != nil {
			return fmt.Errorf("write hosts.toml on node %q: %w", node, err)
		}
	}

	return nil
}

func mirrorImageToLocalRegistry(ctx context.Context, sourceImage string, targetImage string) error {
	if _, err := runCommand(ctx, "docker", "image", "inspect", sourceImage); err != nil {
		if _, pullErr := runCommand(ctx, "docker", "pull", sourceImage); pullErr != nil {
			return fmt.Errorf("ensure source image %q: %w", sourceImage, pullErr)
		}
	}

	if _, err := runCommand(ctx, "docker", "tag", sourceImage, targetImage); err != nil {
		return fmt.Errorf("tag image %q as %q: %w", sourceImage, targetImage, err)
	}
	if _, err := runCommand(ctx, "docker", "push", targetImage); err != nil {
		return fmt.Errorf("push image %q: %w", targetImage, err)
	}

	return nil
}

func buildAndPushBorqImage(ctx context.Context, image string) error {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}

	targetOS := "linux"
	targetArch := runtime.GOARCH
	targetPlatform := fmt.Sprintf("%s/%s", targetOS, targetArch)

	buildContextPath, err := prepareBorqDockerContext(ctx, repositoryRoot, targetOS, targetArch)
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildContextPath)

	_, err = runCommand(
		ctx,
		"docker",
		"build",
		"--build-arg", "TARGETOS="+targetOS,
		"--build-arg", "TARGETARCH="+targetArch,
		"--build-arg", "TARGETPLATFORM="+targetPlatform,
		"-t", image,
		buildContextPath,
	)
	if err != nil {
		return fmt.Errorf("docker build borq image: %w", err)
	}
	if _, err := runCommand(ctx, "docker", "push", image); err != nil {
		return fmt.Errorf("docker push borq image: %w", err)
	}

	return nil
}

func prepareBorqDockerContext(ctx context.Context, repositoryRoot string, targetOS string, targetArch string) (string, error) {
	buildContextPath, err := os.MkdirTemp("", "borq-image-build-*")
	if err != nil {
		return "", fmt.Errorf("create borq image build context: %w", err)
	}

	dockerfileContents := fmt.Sprintf(`FROM scratch

COPY %s/%s/borq /usr/local/bin/borq

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/borq"]
CMD ["management", "server"]
`, targetOS, targetArch)
	if err := os.WriteFile(filepath.Join(buildContextPath, "Dockerfile"), []byte(dockerfileContents), 0o644); err != nil {
		_ = os.RemoveAll(buildContextPath)
		return "", fmt.Errorf("write dockerfile: %w", err)
	}

	platformPath := filepath.Join(buildContextPath, targetOS, targetArch)
	if err := os.MkdirAll(platformPath, 0o755); err != nil {
		_ = os.RemoveAll(buildContextPath)
		return "", fmt.Errorf("create platform directory: %w", err)
	}

	outputPath := filepath.Join(platformPath, "borq")
	command := exec.CommandContext(ctx, "go", "build", "-o", outputPath, "./cmd/borq")
	command.Dir = repositoryRoot
	command.Env = append(
		os.Environ(),
		"CGO_ENABLED=0",
		fmt.Sprintf("GOOS=%s", targetOS),
		fmt.Sprintf("GOARCH=%s", targetArch),
	)

	var stderr strings.Builder
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		_ = os.RemoveAll(buildContextPath)
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText == "" {
			return "", fmt.Errorf("build borq binary: %w", err)
		}

		return "", fmt.Errorf("build borq binary: %w: %s", err, stderrText)
	}

	return buildContextPath, nil
}

func writeIntegrationKindConfig(hostPort int32, registryPort int32) (string, error) {
	temporaryFile, err := os.CreateTemp("", "borq-kind-integration-*.yaml")
	if err != nil {
		return "", err
	}
	defer temporaryFile.Close()

	config := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: %d
    hostPort: %d
    listenAddress: "0.0.0.0"
    protocol: TCP
`, defaultNodePort, hostPort)

	if _, err := temporaryFile.WriteString(config); err != nil {
		return "", err
	}

	return temporaryFile.Name(), nil
}

func copyTextToContainer(ctx context.Context, containerName string, targetPath string, content string) error {
	command := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "sh", "-c", fmt.Sprintf("cat > %q", targetPath))
	command.Stdin = strings.NewReader(content)

	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker exec copy to %q failed: %w: %s", targetPath, err, strings.TrimSpace(string(output)))
	}

	return nil
}

func freeTCPPort() (int32, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("listener address was %T, want *net.TCPAddr", listener.Addr())
	}

	return int32(address.Port), nil
}

func int32String(value int32) string {
	return fmt.Sprintf("%d", value)
}
