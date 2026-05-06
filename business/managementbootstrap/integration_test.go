//go:build integration
// +build integration

package managementbootstrap

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	integrationBootstrapImageEnvVar   = "BORQ_INTEGRATION_BOOTSTRAP_IMAGE"
	integrationKeepClusterEnvVar      = "BORQ_INTEGRATION_KEEP_CLUSTER"
	integrationBootstrapClusterPrefix = "borq-bootstrap-it"
	integrationNamespace              = "borq-system"
)

func TestBootstrapIntegration(t *testing.T) {
	requireIntegrationBinary(t, "kind")
	requireIntegrationBinary(t, "docker")
	requireIntegrationBinary(t, "git")
	requireIntegrationBinary(t, "kubectl")
	requireDockerDaemon(t)

	repoRoot, err := repositoryRoot(context.Background())
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}

	hostPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("find free tcp port: %v", err)
	}

	clusterName := fmt.Sprintf("%s-%d", integrationBootstrapClusterPrefix, time.Now().Unix())
	borqImage := integrationBootstrapImage()

	testContext, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	if !keepClusterForDebug() {
		t.Cleanup(func() {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cleanupCancel()
			_, _ = runCommand(cleanupContext, "kind", "delete", "cluster", "--name", clusterName)
		})
	}

	if _, err := runBootstrapCLI(testContext, repoRoot, clusterName, hostPort, borqImage); err != nil {
		t.Fatalf("bootstrap command failed: %v", err)
	}

	connection, err := kindConnection(clusterName)
	if err != nil {
		t.Fatalf("build kubernetes connection: %v", err)
	}

	if err := assertBootstrapResources(testContext, connection); err != nil {
		t.Fatalf("assert bootstrap resources: %v", err)
	}
	if err := connection.Deployments().WaitForAvailable(testContext, integrationNamespace, borqName); err != nil {
		t.Fatalf("wait for borq deployment readiness: %v", err)
	}
	if err := probeBorq(testContext, probeURLForHostPort(hostPort)); err != nil {
		t.Fatalf("probe borq health endpoint: %v", err)
	}
	if err := assertPersistenceAcrossRestart(testContext, clusterName, integrationNamespace); err != nil {
		t.Fatalf("assert persistence across restart: %v", err)
	}
}

func assertBootstrapResources(ctx context.Context, connection kubernetes.Connection) error {
	if _, err := connection.Namespaces().Get(ctx, integrationNamespace); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	if _, err := connection.PersistentVolumeClaims().Get(ctx, integrationNamespace, borqDataClaimName); err != nil {
		return fmt.Errorf("persistent volume claim: %w", err)
	}
	if _, err := connection.ConfigMaps().Get(ctx, integrationNamespace, borqConfigName); err != nil {
		return fmt.Errorf("config map: %w", err)
	}
	if _, err := connection.ServiceAccounts().Get(ctx, integrationNamespace, borqServiceAccountName); err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	if _, err := connection.Roles().Get(ctx, integrationNamespace, borqRoleName); err != nil {
		return fmt.Errorf("role: %w", err)
	}
	if _, err := connection.RoleBindings().Get(ctx, integrationNamespace, borqRoleBindingName); err != nil {
		return fmt.Errorf("role binding: %w", err)
	}
	if _, err := connection.Deployments().Get(ctx, integrationNamespace, borqName); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if _, err := connection.Services().Get(ctx, integrationNamespace, borqName); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

func assertPersistenceAcrossRestart(ctx context.Context, clusterName string, namespace string) error {
	podName, err := borqPodName(ctx, clusterName, namespace)
	if err != nil {
		return err
	}

	marker := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	if _, err := runCommand(
		ctx,
		"kubectl",
		"--context",
		kindContextName(clusterName),
		"-n",
		namespace,
		"exec",
		podName,
		"--",
		"sh",
		"-c",
		fmt.Sprintf("echo %s > %s/persistence-check", marker, borqDataMountPath),
	); err != nil {
		return fmt.Errorf("write persistence marker: %w", err)
	}

	if _, err := runCommand(
		ctx,
		"kubectl",
		"--context",
		kindContextName(clusterName),
		"-n",
		namespace,
		"delete",
		"pod",
		podName,
	); err != nil {
		return fmt.Errorf("delete pod %q: %w", podName, err)
	}

	connection, err := kindConnection(clusterName)
	if err != nil {
		return err
	}
	if err := connection.Deployments().WaitForAvailable(ctx, namespace, borqName); err != nil {
		return fmt.Errorf("wait for deployment after pod restart: %w", err)
	}

	newPodName, err := borqPodName(ctx, clusterName, namespace)
	if err != nil {
		return err
	}

	output, err := runCommand(
		ctx,
		"kubectl",
		"--context",
		kindContextName(clusterName),
		"-n",
		namespace,
		"exec",
		newPodName,
		"--",
		"cat",
		borqDataMountPath+"/persistence-check",
	)
	if err != nil {
		return fmt.Errorf("read persistence marker: %w", err)
	}
	if strings.TrimSpace(output) != marker {
		return fmt.Errorf("persistence marker mismatch: got %q want %q", strings.TrimSpace(output), marker)
	}

	return nil
}

func borqPodName(ctx context.Context, clusterName string, namespace string) (string, error) {
	output, err := runCommand(
		ctx,
		"kubectl",
		"--context",
		kindContextName(clusterName),
		"-n",
		namespace,
		"get",
		"pods",
		"-l",
		"app.kubernetes.io/name="+borqName,
		"-o",
		"jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", fmt.Errorf("get borq pod name: %w", err)
	}

	podName := strings.TrimSpace(output)
	if podName == "" {
		return "", fmt.Errorf("borq pod name not found")
	}

	return podName, nil
}

func kindConnection(clusterName string) (kubernetes.Connection, error) {
	return kubernetes.NewConnection(
		kubernetes.Config{ContextName: kindContextName(clusterName)},
		logger.Logger{},
	)
}

func repositoryRoot(ctx context.Context) (string, error) {
	output, err := runCommand(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find repository root: %w", err)
	}

	return strings.TrimSpace(output), nil
}

func runCommandInDir(ctx context.Context, dir string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir

	output, err := command.CombinedOutput()
	if err != nil {
		commandLine := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
		return "", fmt.Errorf("%s failed: %w: %s", commandLine, err, strings.TrimSpace(string(output)))
	}

	return string(output), nil
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

func runBootstrapCLI(ctx context.Context, repoRoot string, clusterName string, hostPort int32, image string) (string, error) {
	return runCommandInDir(
		ctx,
		repoRoot,
		"go",
		"run",
		"./cmd/borq",
		"management",
		"bootstrap",
		"--cluster-name",
		clusterName,
		"--host-port",
		strconv.Itoa(int(hostPort)),
		"--timeout",
		"8m",
		"--server-image",
		image,
	)
}

func requireIntegrationBinary(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Fatalf("%s is required for the integration test: %v", binary, err)
	}
}

func requireDockerDaemon(t *testing.T) {
	t.Helper()

	output, err := exec.Command("docker", "info").CombinedOutput()
	if err != nil {
		t.Fatalf("docker daemon is required for the integration test: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

func integrationBootstrapImage() string {
	if image := strings.TrimSpace(os.Getenv(integrationBootstrapImageEnvVar)); image != "" {
		return image
	}
	return borqImage
}

func keepClusterForDebug() bool {
	return parseBoolEnv(integrationKeepClusterEnvVar)
}

func parseBoolEnv(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}

	parsed, err := strconv.ParseBool(value)
	if err == nil {
		return parsed
	}

	switch strings.ToLower(value) {
	case "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}
