//go:build integration
// +build integration

package managementbootstrap

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const integrationBorqImageEnvVar = "BORQ_INTEGRATION_IMAGE"

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

	clusterName := "borq-management-it"
	borqRemoteImage := integrationBorqImage()

	kindConfigPath, err := writeIntegrationKindConfig(hostPort)
	if err != nil {
		t.Fatalf("write integration kind config: %v", err)
	}

	t.Cleanup(func() {
		cleanupContext := context.Background()
		_, _ = runCommand(cleanupContext, "kind", "delete", "cluster", "--name", clusterName)
		_ = os.Remove(kindConfigPath)
	})

	testContext, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	if err := pullRemoteImage(testContext, borqRemoteImage); err != nil {
		t.Fatalf("pull borq image: %v", err)
	}
	if err := createIntegrationCluster(testContext, clusterName, kindConfigPath); err != nil {
		t.Fatalf("create integration cluster: %v", err)
	}

	request := DefaultRequest()
	request.ClusterName = clusterName
	request.HostPort = hostPort
	request.ProbeURL = ""
	request.Timeout = 5 * time.Minute
	request.BorqImage = borqRemoteImage

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

func integrationBorqImage() string {
	if image := strings.TrimSpace(os.Getenv(integrationBorqImageEnvVar)); image != "" {
		return image
	}

	return borqImage
}

func pullRemoteImage(ctx context.Context, image string) error {
	if _, err := runCommand(ctx, "docker", "pull", image); err != nil {
		return fmt.Errorf("docker pull %q: %w", image, err)
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

func writeIntegrationKindConfig(hostPort int32) (string, error) {
	temporaryFile, err := os.CreateTemp("", "borq-kind-integration-*.yaml")
	if err != nil {
		return "", err
	}
	defer temporaryFile.Close()

	config := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
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
