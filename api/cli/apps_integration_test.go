//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// TestApplicationCliIntegration runs real CLI processes, a SQLite-backed server,
// and Kubernetes deployments to verify saved plans, exit codes, and restart durability.
func TestApplicationCliIntegration(t *testing.T) {
	assert := is.New(t)
	binary, kubeconfigPath, image := os.Getenv("BORQ_APPLICATION_TEST_BINARY"), os.Getenv("BORQ_KUBECONFIG"), os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	if binary == "" {
		t.Skip("run make test-apps-e2e to build the CLI and exercise local deployments")
	}
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required")
	}
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(kubeconfig)
	assert.NoErr(err)
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	serverUrl, stop := startApplicationTestServer(t, ctx, binary, directory, port)
	client, err := appapi.NewClientWithResponses(serverUrl, appapi.WithHTTPClient(&http.Client{Timeout: 15 * time.Second}))
	assert.NoErr(err)
	organization := fmt.Sprintf("cli-%x", time.Now().UnixNano())
	namespace := ""
	_, err = runApplicationBinary(ctx, binary, "config", "set", "--server", serverUrl, "--organization", organization, "--space", "dev")
	assert.NoErr(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if namespace == "" {
			return
		}
		if err := kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up CLI deployment: %v", err)
		}
	})
	for _, arguments := range [][]string{
		{"org", "create", organization},
		{"org", "list"},
		{"space", "list"},
		{"cluster", "link", "local", "--kubeconfig", kubeconfigPath},
		{"cluster", "list"},
		{"space", "create", "dev", "--cluster", "local"},
	} {
		_, err := runApplicationBinary(ctx, binary, arguments...)
		assert.NoErr(err)
	}
	spaces, err := client.ListSpacesWithResponse(ctx, organization)
	assert.NoErr(err)
	assert.True(spaces.JSON200 != nil)
	assert.Equal(len(*spaces.JSON200), 2)
	emptyApps, err := runApplicationBinary(ctx, binary, "apps", "list", "--space", "local")
	assert.NoErr(err)
	assert.Equal(string(emptyApps), "apps: []\n")
	emptyManifests, err := runApplicationBinary(ctx, binary, "apps", "list", "--space", "local", "--manifest")
	assert.NoErr(err)
	assert.Equal(len(emptyManifests), 0)

	configFile := filepath.Join(directory, "env.yaml")
	assert.NoErr(os.WriteFile(configFile, []byte("values:\n- name: CLI_LABEL\n  value: from-yaml\n- name: EMPTY_TEXT\n  value: \"\"\n"), 0o600))
	output, err := runApplicationBinary(ctx, binary, "environment-group", "create", "web-config", "-f", configFile)
	assert.NoErr(err)
	var published appapi.EnvironmentGroup
	assert.NoErr(yaml.Unmarshal(output, &published))
	assert.Equal(published.Version, 1)
	assert.Equal(published.Values, []appapi.Variable{{Name: "CLI_LABEL", Value: "from-yaml"}, {Name: "EMPTY_TEXT", Value: ""}})
	source, err := os.ReadFile("testdata/apps.yaml")
	assert.NoErr(err)
	source = []byte(strings.ReplaceAll(string(source), "nginx:1.27.5", image))
	file := filepath.Join(directory, "apps.yaml")
	assert.NoErr(os.WriteFile(file, source, 0o600))
	flags := []string{"-f", file}

	// A first apply plans automatically. Its exit code reflects a real ready ReplicaSet.
	output, err = runApplicationBinary(ctx, binary, append(append([]string{"apply"}, flags...), "--wait", "--timeout", "70s")...)
	assert.NoErr(err)
	var operation appapi.Operation
	assert.NoErr(yaml.Unmarshal(output, &operation))
	assert.Equal(operation.Status, appapi.Succeeded)
	firstPlanId := operation.PlanID
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).LastSuccessfulRevision, 1)
	firstSpace := readApplicationTestSpace(t, ctx, client, organization)
	assert.Equal(firstSpace.Configuration.Apps[0].Env, appapi.EnvReference{Name: "web-config", Version: "1"})
	t.Log("apps list exports expanded app YAML and exact saved manifests with pinned env references")
	exported, err := runApplicationBinary(ctx, binary, "apps", "list")
	assert.NoErr(err)
	var exportedConfiguration appapi.Configuration
	assert.NoErr(yaml.Unmarshal(exported, &exportedConfiguration))
	assert.Equal(exportedConfiguration, firstSpace.Configuration)
	_, err = decodeApplicationPatch(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(exported), "from-yaml"))
	manifestOutput, err := runApplicationBinary(ctx, binary, "apps", "list", "--manifest")
	assert.NoErr(err)
	var manifests []appapi.Manifest
	decoder := yamlreader.NewDecoder(bytes.NewReader(manifestOutput))
	for {
		var manifest appapi.Manifest
		err := decoder.Decode(&manifest)
		if err == io.EOF {
			break
		}
		assert.NoErr(err)
		manifests = append(manifests, manifest)
	}
	firstPlan, err := client.GetPlanWithResponse(ctx, firstPlanId)
	assert.NoErr(err)
	assert.True(firstPlan.JSON200 != nil)
	namespace = firstPlan.JSON200.Values["namespace"].(string)
	actualManifests, err := json.Marshal(manifests)
	assert.NoErr(err)
	expectedManifests, err := json.Marshal(firstPlan.JSON200.Manifests)
	assert.NoErr(err)
	assert.Equal(string(actualManifests), string(expectedManifests))
	assert.True(!strings.Contains(string(manifestOutput), "from-yaml"))
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).Revision, 1)
	// The first plan remains idempotent even after it has completed.
	output, err = runApplicationBinary(ctx, binary, "apply", "--server", serverUrl, "--plan", firstPlanId, "--wait", "--timeout", "5s")
	assert.NoErr(err)
	assert.NoErr(yaml.Unmarshal(output, &operation))
	assert.Equal(operation.PlanID, firstPlanId)
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).Revision, 1)

	// An explicitly selected plan file rejects input changed after its review.
	badPatch := []byte("apps:\n- name: web\n  checks:\n    readiness:\n      path: /this-path-does-not-exist\n      periodSeconds: 1\n")
	assert.NoErr(os.WriteFile(file, badPatch, 0o600))
	output, err = runApplicationBinary(ctx, binary, append([]string{"plan"}, flags...)...)
	assert.NoErr(err)
	assert.True(strings.Contains(string(output), "web-config"))
	assert.True(!strings.Contains(string(output), "from-yaml"))
	exported, err = runApplicationBinary(ctx, binary, "apps", "list")
	assert.NoErr(err)
	assert.NoErr(yaml.Unmarshal(exported, &exportedConfiguration))
	assert.Equal(exportedConfiguration, firstSpace.Configuration)
	metadataBytes, err := os.ReadFile(file + ".borq-plan.json")
	assert.NoErr(err)
	var metadata savedApplicationPlan
	assert.NoErr(json.Unmarshal(metadataBytes, &metadata))
	assert.NoErr(os.WriteFile(file, source, 0o600))
	_, err = runApplicationBinary(ctx, binary, append(append([]string{"apply"}, flags...), "--plan-file", file+".borq-plan.json")...)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "changed since planning"))
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).Revision, 1)
	assert.NoErr(os.WriteFile(file, badPatch, 0o600))

	// The local wait can end while server work continues. Reusing the plan then
	// waits for the server's failure and returns a real nonzero process exit code.
	_, err = runApplicationBinary(ctx, binary, append(append([]string{"apply"}, flags...), "--wait", "--timeout", "1s")...)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "deadline exceeded"))
	accepted := readApplicationTestSpace(t, ctx, client, organization)
	assert.Equal(accepted.Revision, 2)
	assert.Equal(accepted.LastSuccessfulRevision, 1)
	_, err = runApplicationBinary(ctx, binary, "apply", "--server", serverUrl, "--plan", metadata.PlanID, "--wait", "--timeout", "70s")
	assert.True(err != nil)
	var processError *exec.ExitError // os/exec exposes process exit codes through a pointer error.
	assert.True(errors.As(err, &processError))
	assert.True(processError.ExitCode() != 0)
	assert.True(strings.Contains(err.Error(), "failed"))
	failed, err := client.GetOperationWithResponse(ctx, metadata.PlanID)
	assert.NoErr(err)
	if failed.JSON200 == nil {
		t.Fatalf("read failed operation: %s", failed.Body)
	}
	assert.Equal(failed.JSON200.Status, appapi.Failed)
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).LastSuccessfulRevision, 1)
	exported, err = runApplicationBinary(ctx, binary, "apps", "list")
	assert.NoErr(err)
	assert.NoErr(yaml.Unmarshal(exported, &exportedConfiguration))
	assert.Equal(exportedConfiguration, accepted.Configuration)

	// Restart with the same SQLite database and repair the accepted failed desired state.
	stop()
	_, _ = startApplicationTestServer(t, ctx, binary, directory, port)
	afterRestart := readApplicationTestSpace(t, ctx, client, organization)
	assert.Equal(afterRestart.Configuration, accepted.Configuration)
	assert.Equal(afterRestart.LastSuccessfulRevision, 1)
	// Repair explicitly: an omitted readiness field must retain the failed value.
	repair := []byte("apps:\n- name: web\n  checks:\n    readiness:\n      path: /healthz\n")
	assert.NoErr(os.WriteFile(file, repair, 0o600))
	_, err = runApplicationBinary(ctx, binary, append([]string{"plan"}, flags...)...)
	assert.NoErr(err)
	_, err = runApplicationBinary(ctx, binary, append(append([]string{"apply"}, flags...), "--wait", "--timeout", "70s")...)
	assert.NoErr(err)
	healthy := readApplicationTestSpace(t, ctx, client, organization)
	assert.Equal(healthy.Revision, 3)
	assert.Equal(healthy.LastSuccessfulRevision, 3)

	t.Log("successive applies plan automatically, print their diff, and deploy changed YAML without a plan command")
	for index, replicas := range []int{2, 1} {
		patch := fmt.Sprintf("apps:\n- name: web\n  replicas: %d\n", replicas)
		assert.NoErr(os.WriteFile(file, []byte(patch), 0600))
		command := exec.CommandContext(ctx, binary, "apply", "-f", file, "--wait", "--timeout", "70s")
		var output, diagnostics bytes.Buffer
		command.Stdout, command.Stderr = &output, &diagnostics
		if err := command.Run(); err != nil {
			t.Fatalf("automatic update failed: %v\n%s\n%s", err, output.String(), diagnostics.String())
		}
		assert.True(strings.Contains(diagnostics.String(), "Rollout"))
		assert.True(strings.Contains(diagnostics.String(), fmt.Sprintf("replicas: %d", replicas)))
		assert.True(strings.Contains(diagnostics.String(), "update Deployment "+namespace+"/web"))
		assert.NoErr(yaml.Unmarshal(output.Bytes(), &operation))
		assert.Equal(operation.Status, appapi.Succeeded)
		assert.Equal(readApplicationTestSpace(t, ctx, client, organization).LastSuccessfulRevision, 4+index)
		deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "web", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(int(*deployment.Spec.Replicas), replicas)
	}
	t.Log("a stale unapplied sidecar does not block a new ordinary apply")
	_, err = runApplicationBinary(ctx, binary, "plan", "-f", file)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(file, []byte("apps:\n- name: web\n  replicas: 0\n"), 0600))
	_, err = runApplicationBinary(ctx, binary, "apply", "-f", file, "--wait", "--timeout", "70s")
	assert.NoErr(err)
	assert.Equal(readApplicationTestSpace(t, ctx, client, organization).LastSuccessfulRevision, 6)

}

func runApplicationBinary(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		return output, fmt.Errorf("borq %s: %w: %s\n%s", strings.Join(arguments, " "), err, output, diagnostics.String())
	}
	return output, nil
}

func startApplicationTestServer(t *testing.T, ctx context.Context, binary, directory string, port int) (string, func()) {
	t.Helper()
	logfile, err := os.CreateTemp(directory, "server-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "server", "--port", strconv.Itoa(port), "--data-dir", directory, "--rollout-timeout", "40s")
	command.Stdout, command.Stderr = logfile, logfile
	if err := command.Start(); err != nil {
		_ = logfile.Close()
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = command.Process.Signal(os.Interrupt)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-finished
		}
		_ = logfile.Close()
	}
	t.Cleanup(stop)
	serverUrl := "http://127.0.0.1:" + strconv.Itoa(port)
	health := http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := health.Get(serverUrl + "/api/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return serverUrl, stop
			}
		}
		select {
		case err := <-finished:
			stopped = true
			_ = logfile.Close()
			logs, _ := os.ReadFile(logfile.Name())
			t.Fatalf("application test server exited: %v: %s", err, logs)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	logs, _ := os.ReadFile(logfile.Name())
	t.Fatalf("application test server did not become healthy: %s", logs)
	return "", stop
}

// The generated HTTP client requires pointer receivers for its configured transport.
func readApplicationTestSpace(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, organization string) appapi.Space {
	t.Helper()
	response, err := client.GetSpaceWithResponse(ctx, organization, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil {
		t.Fatalf("read application space: %s", response.Body)
	}
	return *response.JSON200
}
