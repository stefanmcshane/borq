//go:build integration

package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	"github.com/stefanmcshane/borq/business/server/platform"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

// TestBootstrapIntegration verifies local management provisioning, explicit target
// enrollment, durable component reconciliation, and application deployment.
func TestBootstrapIntegration(t *testing.T) {
	assert := is.New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is required by the test harness: %v", err)
	}
	binary := os.Getenv("BORQ_INTEGRATION_BOOTSTRAP_BINARY")
	image := os.Getenv("BORQ_INTEGRATION_BOOTSTRAP_IMAGE")
	keepClusters := os.Getenv("BORQ_INTEGRATION_KEEP_CLUSTER") == "1"
	originalKubeconfig, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	assert.NoErr(err)
	commandEnvironment := os.Environ()
	commandHome := t.TempDir()
	dockerConfig := os.Getenv("DOCKER_CONFIG")
	if dockerConfig == "" {
		directory, err := os.UserHomeDir()
		assert.NoErr(err)
		dockerConfig = filepath.Join(directory, ".docker")
	}
	dockerConfig, err = filepath.Abs(dockerConfig)
	assert.NoErr(err)
	var isolatedEnvironment []string
	for _, variable := range commandEnvironment {
		if strings.HasPrefix(variable, "HOME=") || strings.HasPrefix(variable, "PATH=") || strings.HasPrefix(variable, "DOCKER_CONFIG=") || strings.HasPrefix(variable, "XDG_CONFIG_HOME=") {
			continue
		}
		isolatedEnvironment = append(isolatedEnvironment, variable)
	}
	commandEnvironment = append(isolatedEnvironment, "HOME="+commandHome, "PATH="+t.TempDir(), "DOCKER_CONFIG="+dockerConfig, "XDG_CONFIG_HOME="+filepath.Join(commandHome, ".config"))
	var proxyEnvironment []corev1.EnvVar
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if value := os.Getenv(name); value != "" {
			proxyEnvironment = append(proxyEnvironment, corev1.EnvVar{Name: name, Value: bootstrapReachableProxy(t, value)})
		}
	}
	noProxy := os.Getenv("NO_PROXY") + "," + os.Getenv("no_proxy") + ",localhost,127.0.0.1,.svc,.cluster.local,.internal,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	proxyEnvironment = append(proxyEnvironment, corev1.EnvVar{Name: "NO_PROXY", Value: noProxy}, corev1.EnvVar{Name: "no_proxy", Value: noProxy})
	if binary == "" || image == "" {
		t.Fatal("run make test-integration-bootstrap to build the tested server image")
	}
	_, archiveError := os.Stat(filepath.Join(filepath.Dir(binary), "borq-server-linux-"+runtime.GOARCH+".tar"))
	assert.NoErr(archiveError)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	request := DefaultRequest()
	request.Mode = "dev"
	request.ClusterName = fmt.Sprintf("borq-it-%x", time.Now().UnixNano())
	request.KubeconfigPath = filepath.Join(commandHome, ".config", "borq", "kubeconfigs", request.ClusterName)
	request.HostPort = bootstrapFreePort(t)
	request.OpenbaoHostPort = bootstrapFreePort(t)
	request.TelemetryHostPort = bootstrapFreePort(t)
	request.ApiUrl = fmt.Sprintf("http://127.0.0.1:%d", request.HostPort)
	request.Timeout = 15 * time.Minute
	targetName := request.ClusterName + "-target"
	targetPath := filepath.Join(commandHome, ".config", "borq", "kubeconfigs", targetName)
	if !keepClusters {
		t.Cleanup(func() {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cleanupCancel()
			for _, name := range []string{targetName, request.ClusterName} {
				command := exec.CommandContext(cleanupContext, os.Getenv("BORQ_INTEGRATION_K3D"), "cluster", "delete", name)
				command.Env = commandEnvironment
				if _, err := command.CombinedOutput(); err != nil {
					t.Errorf("clean up bootstrap cluster %s: %v", name, err)
				}
			}
		})
	}
	t.Cleanup(func() {
		if t.Failed() {
			bootstrapDiagnostics(t, request.KubeconfigPath)
			bootstrapDiagnostics(t, targetPath)
		}
	})
	t.Log("creating bare Kubernetes without Borq, Helm releases, or API registration")
	bootstrapCreateBareCluster(t, ctx, request.ClusterName, "", []string{
		fmt.Sprintf("127.0.0.1:%d:%d@loadbalancer", request.HostPort, request.NodePort),
		fmt.Sprintf("0.0.0.0:%d:%d@server:0", request.TelemetryHostPort, request.TelemetryNodePort),
		fmt.Sprintf("0.0.0.0:%d:30200@server:0", request.OpenbaoHostPort),
	}, commandEnvironment)
	managementConfig, management := bootstrapKubeClient(t, request.KubeconfigPath)
	bootstrapAssertBareCluster(t, ctx, management)
	serverNodeId, err := runCommand(ctx, "docker", "inspect", "--format", "{{.Id}}", "k3d-"+request.ClusterName+"-server-0")
	assert.NoErr(err)
	bareLoadBalancer := bootstrapLoadBalancer(t, ctx, request.ClusterName)
	barePorts := bareLoadBalancer["ports"].(map[string]any)
	assert.Equal(barePorts[strconv.Itoa(int(request.NodePort))+"/tcp"], []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": strconv.Itoa(int(request.HostPort))}})
	managementAddress := bootstrapNodeAddress(t, ctx, management)
	advertisedApi := "http://" + net.JoinHostPort(managementAddress, strconv.Itoa(int(request.NodePort)))
	targetVaultURL := "http://" + net.JoinHostPort(managementAddress, "30200")
	targetTelemetryURL := "http://" + net.JoinHostPort(managementAddress, strconv.Itoa(int(request.TelemetryNodePort)))
	vaultEndpoint := fmt.Sprintf("http://127.0.0.1:%d", request.OpenbaoHostPort)
	endpoint := request.ApiUrl
	healthURL := endpoint + "/api/healthz"
	legacyTelemetryEndpoint := fmt.Sprintf("http://127.0.0.1:%d", request.TelemetryHostPort)

	t.Log("installing the minimal Borq pod; that server installs its management components")
	initialOutput := bootstrapInstallWithNetwork(t, ctx, binary, request, commandEnvironment, management, proxyEnvironment)
	bootstrapAssertProgress(t, initialOutput)
	bootstrapAssertManagement(t, ctx, management)
	serverDeployment, err := management.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(serverDeployment.Spec.Template.Spec.Containers[0].Image, image)
	unchangedServerNodeId, err := runCommand(ctx, "docker", "inspect", "--format", "{{.Id}}", "k3d-"+request.ClusterName+"-server-0")
	assert.NoErr(err)
	assert.Equal(unchangedServerNodeId, serverNodeId)
	bootstrapAssertSingleApiSetting(t, ctx, management, advertisedApi)
	bootstrapAssertNoTargetRole(t, ctx, endpoint, management)
	publishedLoadBalancer := bootstrapLoadBalancer(t, ctx, request.ClusterName)
	publishedPorts := publishedLoadBalancer["ports"].(map[string]any)
	for port, bindings := range barePorts {
		assert.Equal(publishedPorts[port], bindings)
	}
	assert.Equal(publishedPorts[strconv.Itoa(int(request.NodePort))+"/tcp"], []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": strconv.Itoa(int(request.HostPort))}})

	t.Log("repeating bootstrap preserves all managed Helm release revisions")
	managementRevisions := bootstrapReleaseRevisions(t, ctx, management, defaultNamespace, 5)
	sharedRevisions := bootstrapReleaseRevisions(t, ctx, management, platform.SystemNamespace, 2)
	assert.Equal(len(sharedRevisions), 2)
	bootstrapAssertComponentTimings(t, ctx, management, "management", "", managementRevisions)
	bootstrapInstallWithNetwork(t, ctx, binary, request, commandEnvironment, management, proxyEnvironment)
	assert.NoErr(probeBorq(ctx, healthURL, &http.Client{Timeout: 2 * time.Second}))
	assert.Equal(bootstrapReleaseRevisions(t, ctx, management, defaultNamespace, 5), managementRevisions)
	assert.Equal(bootstrapReleaseRevisions(t, ctx, management, platform.SystemNamespace, 2), sharedRevisions)
	assert.Equal(bootstrapLoadBalancer(t, ctx, request.ClusterName), publishedLoadBalancer)
	bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/management/components/upgrade", nil, http.StatusOK)
	assert.Equal(bootstrapReleaseRevisions(t, ctx, management, defaultNamespace, 5), managementRevisions)
	assert.Equal(bootstrapReleaseRevisions(t, ctx, management, platform.SystemNamespace, 2), sharedRevisions)
	bootstrapAssertPublicBoundary(t, ctx, legacyTelemetryEndpoint)
	bootstrapAssertNoTargetRole(t, ctx, endpoint, management)

	t.Log("management startup recovers a real interrupted shared Traefik upgrade")
	bootstrapSetNodeScheduling(t, ctx, management, false)
	sharedUpgrade := bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/management/configuration", map[string]any{
		"components": map[string]any{"traefik": map[string]any{"values": map[string]any{"deployment": map[string]any{"replicas": 2}}}},
	}, http.StatusAccepted)
	sharedPendingRevision := 0
	upgradeContext, upgradeCancel := context.WithTimeout(ctx, 2*time.Minute)
	for {
		records, err := management.CoreV1().Secrets(platform.SystemNamespace).List(upgradeContext, metav1.ListOptions{LabelSelector: "owner=helm,name=traefik,status=pending-upgrade"})
		assert.NoErr(err)
		if len(records.Items) == 1 {
			sharedPendingRevision, err = strconv.Atoi(records.Items[0].Labels["version"])
			assert.NoErr(err)
			break
		}
		bootstrapPause(t, upgradeContext)
	}
	upgradeCancel()
	bootstrapCrash(t, ctx, management, healthURL, true)
	for {
		operation := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/management/operations/"+bootstrapString(t, sharedUpgrade, "id"), nil, http.StatusOK)
		if operation["status"] == "succeeded" {
			break
		}
		if operation["status"] == "failed" {
			t.Fatalf("shared controller upgrade failed after restart: %v", operation["message"])
		}
		bootstrapPause(t, ctx)
	}
	traefik, err := management.AppsV1().Deployments(platform.SystemNamespace).Get(ctx, "traefik", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(traefik.Status.ReadyReplicas, int32(2))
	bootstrapAssertManagement(t, ctx, management)
	sharedRevisions = bootstrapReleaseRevisions(t, ctx, management, platform.SystemNamespace, 2)
	sharedRecoveredRevision, err := strconv.Atoi(sharedRevisions["traefik"])
	assert.NoErr(err)
	assert.True(sharedRecoveredRevision > sharedPendingRevision)

	bootstrapCreateBareCluster(t, ctx, targetName, "k3d-"+request.ClusterName, nil, commandEnvironment)
	t.Log("the shared cluster registry and kubeconfig folder retain both local clusters")
	registry, err := os.ReadFile(filepath.Join(commandHome, ".config", "borq", "clusters.yaml"))
	assert.NoErr(err)
	var localClusters []localcluster.Local
	assert.NoErr(yaml.UnmarshalStrict(registry, &localClusters))
	assert.Equal(len(localClusters), 2)
	for _, local := range localClusters {
		assert.Equal(local.Runtime, "docker")
		assert.Equal(local.KubeconfigPath, filepath.Join(commandHome, ".config", "borq", "kubeconfigs", local.Name))
	}
	// kubectl uses this same loading API for a KUBECONFIG list; SDK loading rules require a pointer.
	merged, err := (&clientcmd.ClientConfigLoadingRules{Precedence: []string{request.KubeconfigPath, targetPath}}).Load()
	assert.NoErr(err)
	assert.Equal(len(merged.Clusters), 2)
	assert.Equal(len(merged.Contexts), 2)
	targetKubeconfig, err := os.ReadFile(targetPath)
	assert.NoErr(err)
	_, target := bootstrapKubeClient(t, targetPath)
	bootstrapAssertBareCluster(t, ctx, target)
	internalConfig, err := clientcmd.Load(targetKubeconfig)
	assert.NoErr(err)
	targetAddress := bootstrapNodeAddress(t, ctx, target)
	for _, cluster := range internalConfig.Clusters {
		cluster.Server = "https://" + net.JoinHostPort(targetAddress, "6443")
	}
	internalBytes, err := clientcmd.Write(*internalConfig)
	assert.NoErr(err)
	bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap", nil, http.StatusOK)
	t.Log("registration-only and component diffs leave the target untouched")
	remote := bootstrapQuickRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/clusters", map[string]any{"name": "remote", "connection": map[string]any{"kubeconfig": string(internalBytes), "reconcile": false}})
	remoteId := bootstrapString(t, remote, "id")
	remoteName := bootstrapString(t, remote, "name")
	assert.True(remote["kubeconfig"] == nil || remote["kubeconfig"] == "")
	disabled := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components", nil, http.StatusOK)
	assert.Equal(disabled["status"], "disabled")
	bootstrapAssertBareCluster(t, ctx, target)
	initialDiff := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components/diff", nil, http.StatusOK)
	bootstrapAssertComponentDiff(t, initialDiff, "install")
	bootstrapAssertBareCluster(t, ctx, target)
	unchanged := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components", nil, http.StatusOK)
	assert.Equal(unchanged, disabled)
	previewContext, previewCancel := context.WithTimeout(ctx, 5*time.Second)
	previewOutput, previewDiff, previewError := bootstrapClusterCli(t, previewContext, binary, commandEnvironment,
		"cluster", "link", "remote", "--organization", "bootstrap", "--api-url", endpoint, "--reconcile=false",
		"--kubeconfig", targetPath, "--api-server", "https://"+net.JoinHostPort(targetAddress, "6443"))
	previewCancel()
	if previewError != nil {
		t.Fatalf("registration-only CLI link failed: %v\n%s", previewError, previewDiff)
	}
	var preview map[string]any
	assert.NoErr(yaml.Unmarshal(previewOutput, &preview))
	remoteId = bootstrapString(t, preview, "id")
	remoteName = bootstrapString(t, preview, "name")
	if !strings.Contains(previewDiff, "Missing target component: cert-manager") {
		t.Fatalf("registration-only preview omitted the missing component summary:\n%s", previewDiff)
	}
	bootstrapAssertBareCluster(t, ctx, target)

	t.Log("foreign controllers block both CLI previews and server-side installation")
	replicas := int32(0)
	_, err = target.AppsV1().Deployments("default").Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-traefik", Labels: map[string]string{"app.kubernetes.io/name": "traefik", "app.kubernetes.io/instance": "existing-traefik"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, // Kubernetes distinguishes an explicit zero from its default replica count.
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "existing-traefik"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "existing-traefik"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "foreign", Image: "foreign-controller:unused"}}}},
		},
	}, metav1.CreateOptions{})
	assert.NoErr(err)
	conflictDiff := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components/diff", nil, http.StatusOK)
	bootstrapAssertComponentConflict(t, conflictDiff)
	conflictOutput, conflictText, conflictError := bootstrapClusterCli(t, ctx, binary, commandEnvironment,
		"cluster", "link", "remote", "--organization", "bootstrap", "--api-url", endpoint,
		"--kubeconfig", targetPath, "--api-server", "https://"+net.JoinHostPort(targetAddress, "6443"))
	if conflictError == nil || !strings.Contains(conflictText, "conflict") {
		t.Fatalf("CLI link did not reject its conflicting preview: %v\n%s", conflictError, conflictText)
	}
	var conflictingCluster map[string]any
	assert.NoErr(yaml.Unmarshal(conflictOutput, &conflictingCluster))
	remoteId = bootstrapString(t, conflictingCluster, "id")
	remoteName = bootstrapString(t, conflictingCluster, "name")
	blocked := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components", nil, http.StatusOK)
	assert.Equal(blocked["status"], "disabled")
	bypassOutput, bypassText, bypassError := bootstrapClusterCli(t, ctx, binary, commandEnvironment,
		"cluster", "link", "remote", "--organization", "bootstrap", "--api-url", endpoint, "--diff=false",
		"--kubeconfig", targetPath, "--api-server", "https://"+net.JoinHostPort(targetAddress, "6443"))
	if bypassError != nil {
		t.Fatalf("link without a CLI preview failed to queue: %v\n%s", bypassError, bypassText)
	}
	var bypass map[string]any
	assert.NoErr(yaml.Unmarshal(bypassOutput, &bypass))
	remoteId = bootstrapString(t, bypass, "clusterId")
	remoteName = bootstrapLinkedClusterName(t, ctx, endpoint, remoteId)
	failed := bootstrapWaitComponentStatus(t, ctx, endpoint, remoteName, bootstrapString(t, bypass, "id"), "failed")
	bootstrapAssertComponentConflict(t, failed["diff"].(map[string]any))
	bootstrapAssertNoPlatformWrites(t, ctx, target)
	bootstrapAssertNoClusterVaultMount(t, ctx, management, vaultEndpoint, remoteId)
	assert.NoErr(target.AppsV1().Deployments("default").Delete(ctx, "existing-traefik", metav1.DeleteOptions{}))
	bootstrapAssertBareCluster(t, ctx, target)

	t.Log("a short CLI wait and a hard server crash preserve the queued installation")
	bootstrapSetNodeScheduling(t, ctx, target, false)
	waitContext, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	waiterResult, waiterOutput, waiterError := bootstrapClusterCli(t, waitContext, binary, commandEnvironment,
		"cluster", "reconcile", remoteName, "--organization", "bootstrap", "--api-url", endpoint, "--timeout", "1s")
	assert.True(waitContext.Err() == nil)
	waitCancel()
	if waiterError == nil || !strings.Contains(waiterOutput, "deadline") {
		t.Fatalf("expected the CLI waiter to time out while installation continues: %v\n%s", waiterError, waiterOutput)
	}
	var detached map[string]any
	assert.NoErr(yaml.Unmarshal(waiterResult, &detached))
	installationId := bootstrapString(t, detached, "id")
	assert.True(installationId != failed["id"])
	installing := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components", nil, http.StatusOK)
	assert.Equal(installing["id"], installationId)
	if status := installing["status"]; status != "queued" && status != "diffing" && status != "installing" {
		t.Fatalf("short waiter stopped before observing an active installation: %v", status)
	}
	pendingComponent := bootstrapWaitConcurrentTargetReleases(t, ctx, target)
	_, err = target.AppsV1().Deployments(platform.SystemNamespace).Get(ctx, "alloy", metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err)) // Alloy must wait for ESO and its generated telemetry Secret.
	pendingRevision := bootstrapAssertPendingRelease(t, ctx, target, platform.SystemNamespace, pendingComponent)
	bootstrapCrash(t, ctx, management, healthURL, false)
	bootstrapSetNodeScheduling(t, ctx, target, true)
	bootstrapWaitComponentStatus(t, ctx, endpoint, remoteName, installationId, "ready")
	bootstrapAssertTarget(t, ctx, target)
	bootstrapAssertTargetAddresses(t, ctx, target, targetVaultURL, targetTelemetryURL, remoteId)
	targetRevisions := bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5)
	bootstrapAssertComponentTimings(t, ctx, management, "target", remoteId, targetRevisions)
	recoveredRevision, err := strconv.Atoi(targetRevisions[pendingComponent])
	assert.NoErr(err)
	assert.True(recoveredRevision > pendingRevision)
	bootstrapQuickRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap/clusters/"+remoteName, map[string]any{"kubeconfig": string(internalBytes)})
	bootstrapWaitComponentStatus(t, ctx, endpoint, remoteName, "", "ready")
	assert.Equal(bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5), targetRevisions)
	upgrade := bootstrapQuickRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/clusters/"+remoteName+"/components/upgrade", nil)
	bootstrapWaitComponentStatus(t, ctx, endpoint, remoteName, bootstrapString(t, upgrade, "id"), "ready")
	assert.Equal(bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5), targetRevisions)
	diffOutput, _, diffError := bootstrapClusterCli(t, ctx, binary, commandEnvironment,
		"cluster", "diff", remoteName, "--organization", "bootstrap", "--api-url", endpoint)
	assert.NoErr(diffError)
	var noChanges map[string]any
	assert.NoErr(yaml.Unmarshal(diffOutput, &noChanges))
	bootstrapAssertComponentDiff(t, noChanges, "none")
	assert.Equal(bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5), targetRevisions)

	bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap/spaces/dev", map[string]any{"cluster": remoteName}, http.StatusOK)
	t.Log("publishing a named config and deploying through the management vault into the remote cluster")
	bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/dev/environment-groups/bootstrap-config/versions", map[string]any{"values": []any{map[string]any{"name": "BOOTSTRAP_VALUE", "value": "vault-backed"}}}, http.StatusCreated)
	plan := bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/dev/plans", map[string]any{
		"apps": []any{map[string]any{"name": "worker", "image": "nginx:1.27.5", "port": 80, "command": "/bin/sh -c \"printf ready > /usr/share/nginx/html/healthz; printf '%s' \\\"$BOOTSTRAP_VALUE\\\" > /usr/share/nginx/html/index.html; exec nginx -g 'daemon off;'\"", "env": map[string]any{"name": "bootstrap-config"}}},
	}, http.StatusCreated)
	appNamespace := plan["values"].(map[string]any)["namespace"].(string)
	plannedConfiguration := plan["configuration"].(map[string]any)
	plannedApp := plannedConfiguration["apps"].([]any)[0].(map[string]any)
	assert.Equal(plannedApp["env"], map[string]any{"name": "bootstrap-config", "version": "1"})
	bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/dev/environment-groups/bootstrap-config/versions", map[string]any{"values": []any{map[string]any{"name": "BOOTSTRAP_VALUE", "value": "newer-not-applied"}}}, http.StatusCreated)
	operation := bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/plans/"+bootstrapString(t, plan, "id")+"/apply", nil, http.StatusAccepted)
	bootstrapWaitOperation(t, ctx, endpoint, bootstrapString(t, operation, "id"))
	secrets, err := target.CoreV1().Secrets(appNamespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	pinnedSecretName := ""
	for _, secret := range secrets.Items {
		if string(secret.Data["BOOTSTRAP_VALUE"]) == "vault-backed" {
			pinnedSecretName = secret.Name
			assert.True(secret.Immutable != nil && *secret.Immutable)
		}
	}
	assert.True(pinnedSecretName != "")
	appResponse, err := target.CoreV1().RESTClient().Get().AbsPath("/api/v1/namespaces/" + appNamespace + "/services/http:worker:80/proxy/").Do(ctx).Raw()
	assert.NoErr(err)
	assert.Equal(string(appResponse), "vault-backed")

	t.Log("spaces on one target keep independent app and config state without reinstalling controllers")
	defaultSpace := bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap/spaces/remote", map[string]any{"cluster": remoteName}, http.StatusOK)
	assert.Equal(defaultSpace["cluster"], remoteName)
	previewSpace := bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap/spaces/remote", map[string]any{"cluster": remoteName}, http.StatusOK)
	assert.Equal(previewSpace["id"], defaultSpace["id"])
	bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/remote/environment-groups/bootstrap-config/versions", map[string]any{"values": []any{map[string]any{"name": "BOOTSTRAP_VALUE", "value": "preview"}}}, http.StatusCreated)
	previewPlan := bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/remote/plans", map[string]any{
		"apps": []any{map[string]any{"name": "worker", "image": "nginx:1.27.5", "replicas": 0, "env": map[string]any{"name": "bootstrap-config"}}},
	}, http.StatusCreated)
	previewNamespace := previewPlan["values"].(map[string]any)["namespace"].(string)
	previewOperation := bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/plans/"+bootstrapString(t, previewPlan, "id")+"/apply", nil, http.StatusAccepted)
	bootstrapWaitOperation(t, ctx, endpoint, bootstrapString(t, previewOperation, "id"))
	previewDeployment, err := target.AppsV1().Deployments(previewNamespace).Get(ctx, "worker", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(*previewDeployment.Spec.Replicas, int32(0))
	previewConfig := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/spaces/remote/environment-groups/bootstrap-config", nil, http.StatusOK)
	assert.Equal(previewConfig["values"], []any{map[string]any{"name": "BOOTSTRAP_VALUE", "value": "preview"}})
	devConfig := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/spaces/dev/environment-groups/bootstrap-config", nil, http.StatusOK)
	assert.Equal(devConfig["values"], []any{map[string]any{"name": "BOOTSTRAP_VALUE", "value": "newer-not-applied"}})
	appResponse, err = target.CoreV1().RESTClient().Get().AbsPath("/api/v1/namespaces/" + appNamespace + "/services/http:worker:80/proxy/").Do(ctx).Raw()
	assert.NoErr(err)
	assert.Equal(string(appResponse), "vault-backed")
	assert.Equal(bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5), targetRevisions)
	bootstrapAssertTelemetry(t, ctx, legacyTelemetryEndpoint, managementConfig, target, remoteId)
	bootstrapAssertObjectStorage(t, ctx, management, request.ClusterName)

	t.Log("normal CLI linking previews and queues without waiting for controllers")
	linkContext, linkCancel := context.WithTimeout(ctx, 5*time.Second)
	linkedOutput, linkedDiff, linkError := bootstrapClusterCli(t, linkContext, binary, commandEnvironment,
		"cluster", "link", "local", "--organization", "bootstrap", "--api-url", endpoint,
		"--local", request.ClusterName)
	linkCancel()
	if linkError != nil {
		t.Fatalf("background CLI link failed: %v\n%s", linkError, linkedDiff)
	}
	var linked map[string]any
	assert.NoErr(yaml.Unmarshal(linkedOutput, &linked))
	assert.Equal(linked["status"], "queued")
	if !strings.Contains(linkedDiff, "Missing target component: external-secrets") || strings.Contains(linkedDiff, "Missing target component: cert-manager") || strings.Contains(linkedDiff, "Missing target component: traefik") {
		t.Fatalf("queued link preview omitted the missing component summary:\n%s", linkedDiff)
	}
	localId := bootstrapString(t, linked, "clusterId")
	assert.True(localId != remoteId)
	bootstrapWaitComponentStatus(t, ctx, endpoint, bootstrapLinkedClusterName(t, ctx, endpoint, localId), bootstrapString(t, linked, "id"), "ready")
	convergedRevisions := bootstrapReleaseRevisions(t, ctx, management, platform.SystemNamespace, 5)
	for name, revision := range sharedRevisions {
		assert.Equal(convergedRevisions[name], revision)
	}
	bootstrapAssertManagement(t, ctx, management)
	bootstrapAssertTargetAddresses(t, ctx, management, targetVaultURL, targetTelemetryURL, localId)
	bootstrapAssertVaultIsolation(t, ctx, management, target, vaultEndpoint, localId, remoteId)
	remoteTelemetry, err := target.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-telemetry", metav1.GetOptions{})
	assert.NoErr(err)
	crossCluster, err := http.NewRequestWithContext(ctx, http.MethodPost, legacyTelemetryEndpoint+"/api/v1/ingest/"+localId+"/loki/api/v1/push", bytes.NewReader([]byte(`{"streams":[]}`)))
	assert.NoErr(err)
	crossCluster.Header.Set("Authorization", "Bearer "+string(remoteTelemetry.Data["TELEMETRY_TOKEN"]))
	crossCluster.Header.Set("Content-Type", "application/json")
	denied, err := http.DefaultClient.Do(crossCluster)
	assert.NoErr(err)
	denied.Body.Close()
	assert.Equal(denied.StatusCode, http.StatusUnauthorized)

	t.Log("a physical cluster cannot be reassigned to another organization through a second registration")
	targetCredential, err := target.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-openbao", metav1.GetOptions{})
	assert.NoErr(err)
	storeBefore := bootstrapSecretStoreSpec(t, ctx, target)
	bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap-shadow", nil, http.StatusOK)
	bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap-shadow/clusters/remote", map[string]any{"kubeconfig": string(internalBytes)}, http.StatusConflict)
	apiClient, err := appapi.NewClientWithResponses(endpoint)
	assert.NoErr(err)
	shadowClusters, err := apiClient.ListClustersWithResponse(ctx, "bootstrap-shadow")
	assert.NoErr(err)
	assert.Equal(shadowClusters.StatusCode(), http.StatusOK)
	if shadowClusters.JSON200 == nil {
		t.Fatal("cluster list must return an array after a rejected registration")
	}
	assert.Equal(len(*shadowClusters.JSON200), 0)
	unchangedCredential, err := target.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-openbao", metav1.GetOptions{})
	assert.NoErr(err)
	// Comparing through a boolean prevents failed assertions from printing vault tokens.
	assert.True(bytes.Equal(unchangedCredential.Data["token"], targetCredential.Data["token"]))
	assert.Equal(bootstrapSecretStoreSpec(t, ctx, target), storeBefore)
	assert.Equal(bootstrapReleaseRevisions(t, ctx, target, platform.SystemNamespace, 5), targetRevisions)

	t.Log("restarting Borq preserves organizations, spaces, target credentials, and the deployed configuration")
	bootstrapRestart(t, ctx, management, healthURL)
	bootstrapWaitForPublicListener(t, ctx, legacyTelemetryEndpoint)
	pinnedSecret, err := target.CoreV1().Secrets(appNamespace).Get(ctx, pinnedSecretName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(string(pinnedSecret.Data["BOOTSTRAP_VALUE"]), "vault-backed")
	assert.True(pinnedSecret.Immutable != nil && *pinnedSecret.Immutable)
	appResponse, err = target.CoreV1().RESTClient().Get().AbsPath("/api/v1/namespaces/" + appNamespace + "/services/http:worker:80/proxy/").Do(ctx).Raw()
	assert.NoErr(err)
	assert.Equal(string(appResponse), "vault-backed")
	bootstrapRequest(t, ctx, endpoint, http.MethodPut, "/api/v1/organizations/bootstrap/spaces/dev", map[string]any{"cluster": remoteName}, http.StatusOK)
	config := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/spaces/dev/environment-groups/bootstrap-config", nil, http.StatusOK)
	assert.Equal(config["version"], float64(2))
	next := bootstrapRequest(t, ctx, endpoint, http.MethodPost, "/api/v1/organizations/bootstrap/spaces/dev/plans", map[string]any{}, http.StatusCreated)
	configuration, ok := next["configuration"].(map[string]any)
	assert.True(ok)
	apps, ok := configuration["apps"].([]any)
	assert.True(ok)
	assert.Equal(len(apps), 1)
	assert.Equal(apps[0].(map[string]any)["env"], map[string]any{"name": "bootstrap-config", "version": "2"})
	bootstrapAssertTelemetry(t, ctx, legacyTelemetryEndpoint, managementConfig, target, remoteId)
	bootstrapAssertPublicBoundary(t, ctx, legacyTelemetryEndpoint)
	unchangedKubeconfig, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	assert.NoErr(err)
	assert.Equal(unchangedKubeconfig.CurrentContext, originalKubeconfig.CurrentContext)
}

func bootstrapAssertProgress(t *testing.T, output string) {
	t.Helper()
	assert := is.New(t)
	count := 0
	namespacedPod := false
	componentPod := false
	componentStatus := false
	namespacedVolume := false
	serverAttempt := false
	managementPhase := false
	for _, line := range strings.Split(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["event"] != "management_bootstrap_progress" {
			continue
		}
		count++
		for _, field := range []string{"elapsed", "pods", "volumes", "components", "management_phase", "server_attempt"} {
			assert.True(record[field] != nil)
		}
		pods := fmt.Sprint(record["pods"])
		namespacedPod = namespacedPod || strings.Contains(pods, "borq-mgmt/borq-")
		componentPod = componentPod || strings.Contains(pods, "borq-mgmt/openbao-") || strings.Contains(pods, "borq-mgmt/rustfs-") || strings.Contains(pods, "borq-mgmt/loki-")
		componentStatus = componentStatus || strings.Contains(fmt.Sprint(record["components"]), "openbao: ")
		namespacedVolume = namespacedVolume || strings.Contains(fmt.Sprint(record["volumes"]), "borq-mgmt/")
		if attempt, ok := record["server_attempt"].(float64); ok && attempt > 0 {
			serverAttempt = true
		}
		if phase, ok := record["management_phase"].(string); ok && phase != "" && phase != "api_unreachable" && !strings.HasPrefix(phase, "http_") {
			managementPhase = true
		}
	}
	assert.True(count >= 2)
	assert.True(namespacedPod)
	assert.True(componentPod)
	assert.True(componentStatus)
	assert.True(namespacedVolume)
	assert.True(serverAttempt)
	assert.True(managementPhase)
	t.Logf("verified %d bootstrap progress records with namespaced pod details", count)
}

func bootstrapLoadBalancer(t *testing.T, ctx context.Context, name string) map[string]any {
	t.Helper()
	assert := is.New(t)
	output, err := runCommand(ctx, "docker", "inspect", "--format", `{"id":{{json .Id}},"ports":{{json .HostConfig.PortBindings}}}`, "k3d-"+name+"-serverlb")
	assert.NoErr(err)
	var loadBalancer map[string]any
	assert.NoErr(json.Unmarshal([]byte(output), &loadBalancer))
	return loadBalancer
}

// bootstrapLinkedClusterName resolves the generated name from queued setup output.
func bootstrapLinkedClusterName(t *testing.T, ctx context.Context, endpoint, id string) string {
	t.Helper()
	assert := is.New(t)
	client, err := appapi.NewClientWithResponses(endpoint)
	assert.NoErr(err)
	response, err := client.ListClustersWithResponse(ctx, "bootstrap")
	assert.NoErr(err)
	assert.Equal(response.StatusCode(), http.StatusOK)
	assert.True(response.JSON200 != nil)
	for _, cluster := range *response.JSON200 {
		if cluster.ID != nil && *cluster.ID == id {
			return cluster.Name
		}
	}
	t.Fatalf("queued setup has no saved cluster: %s", id)
	return ""
}

func bootstrapClusterCli(t *testing.T, ctx context.Context, binary string, environment []string, arguments ...string) ([]byte, string, error) {
	t.Helper()
	if len(arguments) >= 2 && arguments[0] == "cluster" && arguments[1] == "link" {
		arguments = append(arguments, "--output", "json")
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = t.TempDir()
	command.Env = environment
	var stdout, stderr bytes.Buffer // exec.Command requires pointer-based io.Writer implementations.
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.String(), err
}

func bootstrapQuickRequest(t *testing.T, ctx context.Context, endpoint, method, path string, body map[string]any) map[string]any {
	t.Helper()
	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return bootstrapRequest(t, requestContext, endpoint, method, path, body, http.StatusOK)
}

func bootstrapWaitComponentStatus(t *testing.T, ctx context.Context, endpoint, cluster, installationId, expectedStatus string) map[string]any {
	t.Helper()
	for {
		current := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/organizations/bootstrap/clusters/"+cluster+"/components", nil, http.StatusOK)
		if installationId != "" && current["id"] != installationId {
			t.Fatal("component reconciliation changed its durable installation ID")
		}
		if current["status"] == expectedStatus {
			return current
		}
		switch current["status"] {
		case "queued", "diffing", "installing":
			bootstrapPause(t, ctx)
		default:
			t.Fatalf("component installation did not complete: status=%v message=%v", current["status"], current["message"])
		}
	}
}

func bootstrapAssertComponentConflict(t *testing.T, diff map[string]any) {
	t.Helper()
	assert := is.New(t)
	components, ok := diff["components"].([]any)
	assert.True(ok)
	for _, entry := range components {
		component := entry.(map[string]any)
		if component["name"] == "traefik" {
			assert.Equal(component["action"], "conflict")
			assert.True(strings.Contains(bootstrapString(t, component, "reason"), "default/existing-traefik"))
			return
		}
	}
	t.Fatal("component diff omitted the conflicting Traefik installation")
}

// Bootstrap has more independent charts than installation slots, so it must
// accept whichever three start first before releasing the scheduling gate.
func bootstrapWaitConcurrentManagementReleases(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		records, err := kube.CoreV1().Secrets("").List(waitContext, metav1.ListOptions{LabelSelector: "owner=helm"})
		if err != nil {
			t.Fatal(err)
		}
		started, pending := 0, 0
		for _, record := range records.Items {
			if record.Namespace != defaultNamespace && record.Namespace != platform.SystemNamespace {
				continue
			}
			started++
			if record.Labels["status"] == "pending-install" {
				pending++
			}
		}
		if started >= 3 && pending >= 2 {
			return
		}
		bootstrapPause(t, waitContext)
	}
}

// Independent controllers can acquire the bounded installation slots in any order.
func bootstrapWaitConcurrentTargetReleases(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) string {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		records, err := kube.CoreV1().Secrets(platform.SystemNamespace).List(waitContext, metav1.ListOptions{LabelSelector: "owner=helm,status=pending-install"})
		if err != nil {
			t.Fatal(err)
		}
		if len(records.Items) >= 2 {
			return records.Items[0].Labels["name"]
		}
		bootstrapPause(t, waitContext)
	}
}

func bootstrapAssertPendingRelease(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, namespace, name string) int {
	t.Helper()
	assert := is.New(t)
	records, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + name + ",status=pending-install"})
	assert.NoErr(err)
	if len(records.Items) != 1 {
		all, readErr := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + name})
		assert.NoErr(readErr)
		labels := []map[string]string{}
		for _, record := range all.Items {
			labels = append(labels, record.Labels)
		}
		t.Fatalf("expected one pending Helm release for %s; observed release labels: %v", name, labels)
	}
	revision, err := strconv.Atoi(records.Items[0].Labels["version"])
	assert.NoErr(err)
	return revision
}

func bootstrapAssertComponentDiff(t *testing.T, diff map[string]any, action string) {
	t.Helper()
	assert := is.New(t)
	components, ok := diff["components"].([]any)
	assert.True(ok)
	assert.Equal(len(components), 5)
	expected := map[string]bool{"cert-manager": true, "external-secrets": true, "traefik": true, "alloy": true, "snapshot-controller": true}
	for _, entry := range components {
		component, ok := entry.(map[string]any)
		assert.True(ok)
		name := bootstrapString(t, component, "name")
		assert.True(expected[name])
		delete(expected, name)
		assert.Equal(component["action"], action)
		desiredVersion := bootstrapString(t, component, "desiredVersion")
		if action == "install" {
			assert.Equal(component["installedVersion"], "")
			assert.Equal(component["revision"], float64(0))
			continue
		}
		assert.Equal(strings.TrimPrefix(bootstrapString(t, component, "installedVersion"), "v"), strings.TrimPrefix(desiredVersion, "v"))
		assert.Equal(component["upgradeAvailable"], false)
		assert.Equal(component["valuesDiff"], "")
	}
	assert.Equal(len(expected), 0)
}

// The command writes through bytes.Buffer's pointer-based io.Writer API until
// completed reports its exit. Callers read the buffer only after that signal.
func bootstrapStart(t *testing.T, ctx context.Context, binary string, request Request, environment []string) (<-chan error, *bytes.Buffer) {
	t.Helper()
	arguments := []string{"management", "bootstrap", request.ClusterName, "--api-url", request.ApiUrl,
		"--kubeconfig", request.KubeconfigPath, "--development",
		"--server-image-archive", filepath.Join(filepath.Dir(binary), "borq-server-linux-"+runtime.GOARCH+".tar"),
		"--node-port", strconv.Itoa(int(request.NodePort)),
		"--timeout", request.Timeout.String(),
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = t.TempDir()
	for _, variable := range environment {
		if !strings.HasPrefix(variable, "PATH=") {
			command.Env = append(command.Env, variable)
		}
	}
	// Bootstrap must find Borq's private k3d with no system Helm, k3d, or k3s binaries.
	command.Env = append(command.Env, "PATH="+t.TempDir())
	output := new(bytes.Buffer)
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() { completed <- command.Wait() }()
	return completed, output
}

func bootstrapAwait(t *testing.T, completed <-chan error, output *bytes.Buffer) {
	t.Helper()
	if err := <-completed; err != nil {
		t.Fatalf("management command failed: %v\n%s", err, output.String())
	}
}

// Kubernetes creation belongs to this isolated test fixture, outside Borq commands.
func bootstrapCreateBareCluster(t *testing.T, ctx context.Context, name, network string, ports []string, environment []string) {
	t.Helper()
	binary := os.Getenv("BORQ_INTEGRATION_K3D")
	if binary == "" {
		t.Fatal("run make test-integration-bootstrap")
	}
	arguments := []string{"cluster", "create", name, "--image", "rancher/k3s:v1.35.3-k3s1", "--api-port", fmt.Sprintf("127.0.0.1:%d", bootstrapFreePort(t)),
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--k3s-arg", "--disable=traefik@server:*",
		"--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*", "--k3s-arg", "--kube-proxy-arg=conntrack-max-per-core=0@server:*", "--wait", "--timeout", "3m"}
	if runtime.GOOS == "linux" {
		arguments = append(arguments, "--volume", "/dev/null:/dev/kmsg@server:*")
	}
	if network != "" {
		arguments = append(arguments, "--network", network)
	}
	for _, port := range ports {
		arguments = append(arguments, "--port", port)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare Kubernetes fixture: %v: %s", err, output)
	}
	command = exec.CommandContext(ctx, binary, "kubeconfig", "get", name)
	command.Env = environment
	credentials, err := command.Output()
	is.New(t).NoErr(err)
	configurationDirectory := ""
	for _, variable := range environment {
		if strings.HasPrefix(variable, "XDG_CONFIG_HOME=") {
			configurationDirectory = strings.TrimPrefix(variable, "XDG_CONFIG_HOME=")
		}
	}
	path := filepath.Join(configurationDirectory, "borq", "kubeconfigs", name)
	is.New(t).NoErr(os.MkdirAll(filepath.Dir(path), 0700))
	is.New(t).NoErr(os.WriteFile(path, credentials, 0600))
	// Retain legacy credential discovery coverage for externally prepared clusters.
	registry := filepath.Join(configurationDirectory, "borq", "clusters.yaml")
	entries := []localcluster.Local{}
	if source, err := os.ReadFile(registry); err == nil {
		is.New(t).NoErr(yaml.UnmarshalStrict(source, &entries))
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	entries = append(entries, localcluster.Local{Name: name, Runtime: "docker", KubeconfigPath: path})
	source, err := yaml.Marshal(entries)
	is.New(t).NoErr(err)
	is.New(t).NoErr(os.WriteFile(registry, source, 0600))
}

func bootstrapAssertBareCluster(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	bootstrapAssertNoPlatformWrites(t, ctx, kube)
	deployments, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	for _, deployment := range deployments.Items {
		for _, name := range []string{"borq", "openbao", "rustfs", "loki", "prometheus", "cert-manager", "external-secrets", "traefik", "alloy", "snapshot-controller"} {
			if strings.Contains(deployment.Name, name) {
				t.Fatalf("bare cluster unexpectedly installed %s/%s", deployment.Namespace, deployment.Name)
			}
		}
	}
}

func bootstrapAssertNoPlatformWrites(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	_, err := kube.CoreV1().Namespaces().Get(ctx, platform.SystemNamespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected bare cluster without namespace %s; namespace query returned %v", platform.SystemNamespace, err)
	}
	releases, err := kube.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
	assert.NoErr(err)
	assert.Equal(len(releases.Items), 0)
}

func bootstrapAssertNoTargetRole(t *testing.T, ctx context.Context, endpoint string, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	for _, name := range []string{"external-secrets", "alloy", "snapshot-controller"} {
		_, err := kube.AppsV1().Deployments(platform.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
		assert.True(apierrors.IsNotFound(err))
	}
	client, err := appapi.NewClientWithResponses(endpoint)
	assert.NoErr(err)
	organizations, err := client.ListOrganizationsWithResponse(ctx)
	assert.NoErr(err)
	assert.Equal(organizations.StatusCode(), http.StatusOK)
	if organizations.JSON200 == nil {
		t.Fatal("organization list must return an array after management bootstrap")
	}
	assert.Equal(len(*organizations.JSON200), 0)
}

func bootstrapSetNodeScheduling(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, enabled bool) {
	t.Helper()
	assert := is.New(t)
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.Equal(len(nodes.Items), 1)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := kube.CoreV1().Nodes().Get(ctx, nodes.Items[0].Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		node.Spec.Unschedulable = !enabled
		_, err = kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
	assert.NoErr(err)
}

// bootstrapInstallWithNetwork gates scheduling so each new server receives the
// executor's proxy and CA before it starts installing its management components.
func bootstrapInstallWithNetwork(t *testing.T, ctx context.Context, binary string, request Request, environment []string, kube *kubeclient.Clientset, proxyEnvironment []corev1.EnvVar) string {
	t.Helper()
	var previousGeneration int64
	previous, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	if err == nil {
		previousGeneration = previous.Generation
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	bootstrapSetNodeScheduling(t, ctx, kube, false)
	completed, output := bootstrapStart(t, ctx, binary, request, environment)
	bootstrapWaitForDeployment(t, ctx, kube, previousGeneration, completed, output)
	bootstrapConfigureTestNetwork(t, ctx, kube, proxyEnvironment)
	bootstrapWaitForConfiguredPod(t, ctx, kube)
	if previousGeneration == 0 {
		t.Log("independent management charts install while their pods are held unscheduled")
		bootstrapWaitConcurrentManagementReleases(t, ctx, kube)
		// The vault may still be waiting for an installation slot. If its chart
		// has started, unscheduled pods must still block dependent components.
		vault, err := kube.AppsV1().StatefulSets(defaultNamespace).Get(ctx, "openbao", metav1.GetOptions{})
		if err == nil {
			is.New(t).Equal(vault.Status.ReadyReplicas, int32(0))
		} else {
			is.New(t).True(apierrors.IsNotFound(err))
		}
		_, err = kube.AppsV1().Deployments(defaultNamespace).Get(ctx, "rustfs", metav1.GetOptions{})
		is.New(t).True(apierrors.IsNotFound(err)) // RustFS needs ready OpenBao and ESO.
		_, err = kube.AppsV1().StatefulSets(defaultNamespace).Get(ctx, "loki", metav1.GetOptions{})
		is.New(t).True(apierrors.IsNotFound(err)) // Loki needs RustFS and its bucket.
	}
	bootstrapSetNodeScheduling(t, ctx, kube, true)
	bootstrapAwait(t, completed, output)
	bootstrapWaitForAvailable(t, ctx, kube)
	return output.String()
}

func bootstrapWaitForDeployment(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, previousGeneration int64, completed <-chan error, output *bytes.Buffer) {
	t.Helper()
	for {
		deployment, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
		if err == nil && deployment.Generation > previousGeneration {
			return
		}
		// Adding the API mapping can briefly replace k3d's load balancer while
		// this poll still has a connection to its Kubernetes API endpoint.
		var networkError net.Error // errors.As requires an output pointer for wrapped transport errors.
		if err != nil && !apierrors.IsNotFound(err) && !errors.As(err, &networkError) &&
			!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) &&
			!apierrors.IsServiceUnavailable(err) && !apierrors.IsTimeout(err) {
			t.Fatal(err)
		}
		select {
		case err := <-completed:
			t.Fatalf("bootstrap exited before updating its Deployment: %v\n%s", err, output.String())
		default:
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapWaitForConfiguredPod(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	for {
		pods, err := kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=borq"})
		assert.NoErr(err)
		if len(pods.Items) == 1 && pods.Items[0].DeletionTimestamp == nil {
			pod := pods.Items[0]
			for _, variable := range pod.Spec.Containers[0].Env {
				if variable.Name == "SSL_CERT_FILE" && variable.Value == "/var/run/borq-test-trust/ca-certificates.crt" {
					return
				}
			}
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapAssertPublicBoundary(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	// This PUT succeeds on the administrative listener; public ingestion must not expose it.
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/api/v1/organizations/bootstrap", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("public telemetry listener exposed an administrative route: status %d", response.StatusCode)
	}
}

// The telemetry Service publishes only ready pods. Its endpoints can lag the
// administrative health response briefly after Kubernetes replaces the server.
func bootstrapWaitForPublicListener(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	client := http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(waitContext, http.MethodGet, endpoint+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusNotFound {
				return
			}
		}
		bootstrapPause(t, waitContext)
	}
}

// bootstrapConfigureTestNetwork supplies the test executor's HTTPS trust and
// proxy at runtime, keeping session-specific certificates out of image layers.
func bootstrapConfigureTestNetwork(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, environment []corev1.EnvVar) {
	t.Helper()
	assert := is.New(t)
	bundle, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	assert.NoErr(err)
	_, err = kube.CoreV1().ConfigMaps(defaultNamespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-test-trust"},
		Data:       map[string]string{"ca-certificates.crt": string(bundle)},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	proxyValues := map[string][]byte{}
	for _, variable := range environment {
		proxyValues[variable.Name] = []byte(variable.Value)
	}
	_, err = kube.CoreV1().Secrets(defaultNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-test-proxy"}, Data: proxyValues,
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var volumes []corev1.Volume
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.Name != "bootstrap-test-trust" {
				volumes = append(volumes, volume)
			}
		}
		deployment.Spec.Template.Spec.Volumes = append(volumes, corev1.Volume{
			Name: "bootstrap-test-trust", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "bootstrap-test-trust"}}},
		})
		// Only Borq may schedule on the cordoned node. Real component pods stay
		// pending until the test has observed overlapping Helm installations.
		toleratesCordon := false
		for _, toleration := range deployment.Spec.Template.Spec.Tolerations {
			if toleration.Key == "node.kubernetes.io/unschedulable" {
				toleratesCordon = true
			}
		}
		if !toleratesCordon {
			deployment.Spec.Template.Spec.Tolerations = append(deployment.Spec.Template.Spec.Tolerations,
				corev1.Toleration{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		}
		for index := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[index] // Mutate the owning Deployment's pod template.
			var variables []corev1.EnvVar
			for _, variable := range container.Env {
				if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil && variable.ValueFrom.SecretKeyRef.Name == "bootstrap-test-proxy" || variable.Name == "SSL_CERT_FILE" {
					continue
				}
				variables = append(variables, variable)
			}
			container.Env = variables
			for _, variable := range environment {
				container.Env = append(container.Env, corev1.EnvVar{Name: variable.Name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "bootstrap-test-proxy"}, Key: variable.Name}}})
			}
			container.Env = append(container.Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/var/run/borq-test-trust/ca-certificates.crt"})
			var mounts []corev1.VolumeMount
			for _, mount := range container.VolumeMounts {
				if mount.Name != "bootstrap-test-trust" {
					mounts = append(mounts, mount)
				}
			}
			container.VolumeMounts = append(mounts, corev1.VolumeMount{Name: "bootstrap-test-trust", MountPath: "/var/run/borq-test-trust", ReadOnly: true})
		}
		_, err = kube.AppsV1().Deployments(defaultNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	})
	assert.NoErr(err)
}

func bootstrapWaitForAvailable(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	for {
		current, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
		assert.NoErr(err)
		if current.Status.ObservedGeneration >= current.Generation && current.Status.UpdatedReplicas == 1 && current.Status.AvailableReplicas == 1 && current.Status.Replicas == 1 {
			return
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapReachableProxy(t *testing.T, address string) string {
	t.Helper()
	proxy, err := url.Parse(address)
	if err != nil || proxy.Hostname() == "" {
		t.Fatal("configured test proxy URL is invalid")
	}
	if net.ParseIP(proxy.Hostname()) != nil {
		return address
	}
	addresses, err := net.LookupHost(proxy.Hostname())
	if err != nil || len(addresses) == 0 {
		t.Fatal("cannot resolve configured test proxy hostname")
	}
	port := proxy.Port()
	if port == "" {
		port = "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
	}
	proxy.Host = net.JoinHostPort(addresses[0], port)
	return proxy.String()
}

func bootstrapKubeClient(t *testing.T, path string) (*rest.Config, *kubeclient.Clientset) {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubeclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return config, client
}

func bootstrapNodeAddress(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) string {
	t.Helper()
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 {
		t.Fatalf("expected a single disposable cluster node, got %d", len(nodes.Items))
	}
	for _, address := range nodes.Items[0].Status.Addresses {
		if address.Type == corev1.NodeInternalIP && net.ParseIP(address.Address) != nil {
			return address.Address
		}
	}
	t.Fatal("disposable cluster node has no internal IP")
	return ""
}

func bootstrapAssertManagement(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	for _, name := range []string{"cert-manager", "traefik"} {
		controllers, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + name})
		assert.NoErr(err)
		assert.Equal(len(controllers.Items), 1)
		assert.Equal(controllers.Items[0].Namespace, platform.SystemNamespace)
		assert.True(controllers.Items[0].Status.AvailableReplicas > 0)
	}
	for _, name := range []string{"borq", "prometheus-server", "external-secrets-mgmt", "rustfs"} {
		_, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
	}
	for _, name := range []string{"openbao", "loki"} {
		_, err := kube.AppsV1().StatefulSets(defaultNamespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
	}
	deployments, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	for _, deployment := range deployments.Items {
		if strings.Contains(deployment.Name, "flux") || strings.Contains(deployment.Name, "gitea") || strings.Contains(deployment.Name, "external-dns") {
			t.Fatalf("unexpected management deployment %s", deployment.Name)
		}
	}
	claims, err := kube.CoreV1().PersistentVolumeClaims(defaultNamespace).List(ctx, metav1.ListOptions{})
	assert.NoErr(err)
	assert.True(len(claims.Items) >= 4)
	for _, claim := range claims.Items {
		assert.Equal(claim.Status.Phase, corev1.ClaimBound)
		assert.True(claim.Spec.Resources.Requests.Storage().Value() <= 5*1024*1024*1024)
	}
}

// bootstrapAssertComponentTimings checks the actual server pod's successful setup
// logs against installed releases, rather than timing the bootstrap CLI process.
func bootstrapAssertComponentTimings(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, scope, clusterId string, releases map[string]string) {
	t.Helper()
	assert := is.New(t)
	pods, err := kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=borq"})
	assert.NoErr(err)
	completed := map[string]bool{}
	summary := false
	for _, pod := range pods.Items {
		// Kubernetes accepts a pointer to distinguish omitted log options.
		logs, err := kube.CoreV1().Pods(defaultNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: borqName}).DoRaw(ctx)
		assert.NoErr(err)
		for _, line := range strings.Split(string(logs), "\n") {
			fields := map[string]string{}
			for _, word := range strings.Fields(line) {
				if key, value, ok := strings.Cut(word, "="); ok {
					fields[key] = value
				}
			}
			if fields["scope"] != scope || fields["cluster_id"] != clusterId {
				continue
			}
			if strings.Contains(line, " components_setup_finished ") && fields["status"] == "ready" {
				summary = true
			}
			if !strings.Contains(line, " component_setup_finished ") || fields["status"] != "deployed" {
				continue
			}
			name := fields["component"]
			if _, installed := releases[name]; !installed {
				continue
			}
			duration, err := strconv.ParseInt(fields["duration_ms"], 10, 64)
			assert.NoErr(err)
			waiting, err := strconv.ParseInt(fields["wait_duration_ms"], 10, 64)
			assert.NoErr(err)
			assert.True(duration >= 0 && waiting >= 0)
			assert.True(fields["duration"] != "")
			completed[name] = true
		}
	}
	for name := range releases {
		if !completed[name] {
			t.Errorf("server did not log successful %s setup timing for %s", scope, name)
		}
	}
	assert.True(summary)
}

func bootstrapAssertSingleApiSetting(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, advertisedApi string) {
	t.Helper()
	assert := is.New(t)
	deployment, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	assert.NoErr(err)
	apiConfigured := false
	for _, argument := range deployment.Spec.Template.Spec.Containers[0].Args {
		if argument == "--api-url="+advertisedApi {
			apiConfigured = true
		}
		assert.True(!strings.HasPrefix(argument, "--management-openbao-url="))
		assert.True(!strings.HasPrefix(argument, "--management-telemetry-url="))
	}
	assert.True(apiConfigured)
}

func bootstrapAssertTarget(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) {
	t.Helper()
	assert := is.New(t)
	for _, name := range []string{"external-secrets", "external-secrets-webhook", "external-secrets-cert-controller", "cert-manager", "cert-manager-webhook", "traefik", "alloy", "snapshot-controller"} {
		_, err := kube.AppsV1().Deployments(platform.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
		assert.NoErr(err)
	}
	_, err := kube.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-openbao", metav1.GetOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-telemetry", metav1.GetOptions{})
	assert.NoErr(err)
	_, err = kube.CoreV1().Services(platform.SystemNamespace).Get(ctx, "openbao", metav1.GetOptions{})
	assert.True(err != nil) // Deployment targets use the public management vault.
}

func bootstrapAssertTargetAddresses(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, vaultURL, telemetryURL, targetId string) {
	t.Helper()
	assert := is.New(t)
	store := bootstrapSecretStoreSpec(t, ctx, kube)
	provider := store["provider"].(map[string]any)
	vault := provider["vault"].(map[string]any)
	assert.Equal(vault["server"], vaultURL)
	configuration, err := kube.CoreV1().ConfigMaps(platform.SystemNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=alloy"})
	assert.NoErr(err)
	for _, item := range configuration.Items {
		for _, value := range item.Data {
			if strings.Contains(value, telemetryURL+"/api/v1/ingest/"+targetId+"/loki/api/v1/push") && strings.Contains(value, telemetryURL+"/api/v1/ingest/"+targetId+"/api/v1/write") {
				return
			}
		}
	}
	t.Fatal("Alloy must use the automatically managed ingestion address")
}

// bootstrapSecretStoreSpec checks provider identity without reading or logging credentials.
func bootstrapSecretStoreSpec(t *testing.T, ctx context.Context, kube *kubeclient.Clientset) map[string]any {
	t.Helper()
	data, err := kube.CoreV1().RESTClient().Get().AbsPath("/apis/external-secrets.io/v1/clustersecretstores/borq-openbao").Do(ctx).Raw()
	if err != nil {
		t.Fatal(err)
	}
	var store struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	return store.Spec
}

func bootstrapReleaseRevisions(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, namespace string, expected int) map[string]string {
	t.Helper()
	secrets, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,status=deployed"})
	if err != nil {
		t.Fatal(err)
	}
	revisions := map[string]string{}
	for _, secret := range secrets.Items {
		revisions[secret.Labels["name"]] = secret.Labels["version"]
	}
	if len(revisions) != expected {
		t.Fatalf("expected %d persisted Helm releases in %s, got %v", expected, namespace, revisions)
	}
	return revisions
}

func bootstrapWaitOperation(t *testing.T, ctx context.Context, endpoint, operationId string) {
	t.Helper()
	for {
		current := bootstrapRequest(t, ctx, endpoint, http.MethodGet, "/api/v1/operations/"+operationId, nil, http.StatusOK)
		if current["status"] == "succeeded" {
			return
		}
		if current["status"] == "failed" {
			t.Fatalf("bootstrapped app rollout failed: %v", current["message"])
		}
		bootstrapPause(t, ctx)
	}
}

// bootstrapCrash sends SIGKILL only to this test's Borq container. Containerd
// confirms the old process exited before Kubernetes starts its replacement, so
// the shared SQLite volume never has two server processes writing concurrently.
func bootstrapCrash(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, probeUrl string, resumeScheduling bool) {
	t.Helper()
	assert := is.New(t)
	pods, err := kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=borq"})
	assert.NoErr(err)
	assert.Equal(len(pods.Items), 1)
	pod := pods.Items[0]
	assert.True(strings.HasPrefix(pod.Spec.NodeName, "k3d-borq-it-"))
	var previous corev1.ContainerStatus
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == borqName {
			previous = status
		}
	}
	assert.True(strings.HasPrefix(previous.ContainerID, "containerd://") && previous.Ready)
	_, err = runCommand(ctx, "docker", "exec", pod.Spec.NodeName, "ctr", "--address", "/run/k3s/containerd/containerd.sock", "--namespace", "k8s.io", "tasks", "kill", "--signal", "SIGKILL", strings.TrimPrefix(previous.ContainerID, "containerd://"))
	assert.NoErr(err)
	if resumeScheduling {
		bootstrapSetNodeScheduling(t, ctx, kube, true)
	}
	waitContext, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		current, err := kube.CoreV1().Pods(defaultNamespace).Get(waitContext, pod.Name, metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(current.UID, pod.UID)
		for _, status := range current.Status.ContainerStatuses {
			if status.Name != borqName || status.ContainerID == previous.ContainerID || status.RestartCount <= previous.RestartCount || !status.Ready {
				continue
			}
			for _, condition := range current.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					assert.True(status.LastTerminationState.Terminated != nil)
					assert.Equal(status.LastTerminationState.Terminated.ExitCode, int32(137))
					http.DefaultClient.CloseIdleConnections()
					assert.NoErr(probeBorq(waitContext, probeUrl, &http.Client{Timeout: 2 * time.Second}))
					return
				}
			}
		}
		bootstrapPause(t, waitContext)
	}
}

func bootstrapRestart(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, probeUrl string) {
	t.Helper()
	assert := is.New(t)
	pods, err := kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=borq"})
	assert.NoErr(err)
	assert.Equal(len(pods.Items), 1)
	previousUid := pods.Items[0].UID
	assert.NoErr(kube.CoreV1().Pods(defaultNamespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{}))
	for {
		pods, err = kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=borq"})
		assert.NoErr(err)
		if len(pods.Items) == 1 && pods.Items[0].UID != previousUid {
			for _, condition := range pods.Items[0].Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					http.DefaultClient.CloseIdleConnections()
					assert.NoErr(probeBorq(ctx, probeUrl, &http.Client{Timeout: 2 * time.Second}))
					return
				}
			}
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapAssertNoClusterVaultMount(t *testing.T, ctx context.Context, management *kubeclient.Clientset, vaultURL, clusterId string) {
	t.Helper()
	assert := is.New(t)
	admin, err := management.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, vaultURL+"/v1/sys/mounts", nil)
	assert.NoErr(err)
	request.Header.Set("X-Vault-Token", string(admin.Data["token"]))
	response, err := http.DefaultClient.Do(request)
	assert.NoErr(err)
	defer response.Body.Close()
	assert.Equal(response.StatusCode, http.StatusOK)
	var mounts struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	assert.NoErr(json.NewDecoder(response.Body).Decode(&mounts))
	assert.True(mounts.Data != nil)
	_, exists := mounts.Data["borq-"+clusterId+"/"]
	assert.True(!exists)
}

func bootstrapAssertVaultIsolation(t *testing.T, ctx context.Context, management, target *kubeclient.Clientset, vaultURL, localId, remoteId string) {
	t.Helper()
	assert := is.New(t)
	localSecret, err := management.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-openbao", metav1.GetOptions{})
	assert.NoErr(err)
	remoteSecret, err := target.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-openbao", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(!bytes.Equal(localSecret.Data["token"], remoteSecret.Data["token"]))
	admin, err := management.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	address := vaultURL
	for _, credential := range []struct {
		token   []byte
		ownId   string
		otherId string
	}{{localSecret.Data["token"], localId, remoteId}, {remoteSecret.Data["token"], remoteId, localId}} {
		for _, access := range []struct {
			path   string
			status int
		}{
			{"/v1/sys/mounts", http.StatusForbidden},
			{"/v1/borq-" + credential.ownId + "/data/borq/platform/telemetry", http.StatusOK},
			{"/v1/borq-" + credential.otherId + "/data/borq/platform/telemetry", http.StatusForbidden},
		} {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+access.path, nil)
			assert.NoErr(err)
			request.Header.Set("X-Vault-Token", string(credential.token))
			response, err := http.DefaultClient.Do(request)
			assert.NoErr(err)
			response.Body.Close()
			assert.Equal(response.StatusCode, access.status)
		}
	}
	// A target token must also be unable to impersonate the management operator.
	assert.True(!bytes.Equal(admin.Data["token"], remoteSecret.Data["token"]))
}

func bootstrapAssertTelemetry(t *testing.T, ctx context.Context, endpoint string, config *rest.Config, target *kubeclient.Clientset, targetId string) {
	t.Helper()
	assert := is.New(t)
	secret, err := target.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, "borq-telemetry", metav1.GetOptions{})
	assert.NoErr(err)
	token := string(secret.Data["TELEMETRY_TOKEN"])
	assert.True(token != "")
	line := "bootstrap-telemetry-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	payload, err := json.Marshal(map[string]any{"streams": []any{map[string]any{
		"stream": map[string]string{"job": "borq-bootstrap-integration", "borq_cluster": targetId},
		"values": [][]string{{strconv.FormatInt(time.Now().UnixNano(), 10), line}},
	}}})
	assert.NoErr(err)
	path := "/api/v1/ingest/" + targetId + "/loki/api/v1/push"
	for _, input := range []struct {
		token  string
		status int
	}{{"invalid", http.StatusUnauthorized}, {token, http.StatusNoContent}} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(payload))
		assert.NoErr(err)
		request.Header.Set("Authorization", "Bearer "+input.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		assert.NoErr(err)
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		assert.NoErr(readErr)
		if response.StatusCode != input.status {
			t.Fatalf("telemetry gateway returned %d, want %d: %s", response.StatusCode, input.status, body)
		}
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		query := url.Values{"query": {`{job="borq-bootstrap-integration",borq_cluster="` + targetId + `"}`}}
		logs := bootstrapServiceGet(t, ctx, config, "http:loki:3100", "/loki/api/v1/query_range?"+query.Encode())
		if bytes.Contains(logs, []byte(line)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authenticated log did not reach Loki: %s", logs)
		}
		bootstrapPause(t, ctx)
	}
	deadline = time.Now().Add(90 * time.Second)
	for {
		query := url.Values{"query": {`up{borq_cluster="` + targetId + `"}`}}
		encoded := bootstrapServiceGet(t, ctx, config, "http:prometheus-server:80", "/api/v1/query?"+query.Encode())
		var response struct {
			Data struct {
				Result []any `json:"result"`
			} `json:"data"`
		}
		assert.NoErr(json.Unmarshal(encoded, &response))
		if len(response.Data.Result) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Alloy metrics did not reach management Prometheus: %s", encoded)
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapAssertObjectStorage(t *testing.T, ctx context.Context, kube *kubeclient.Clientset, managementClusterName string) {
	t.Helper()
	assert := is.New(t)
	_, err := kube.CoreV1().RESTClient().Post().AbsPath("/api/v1/namespaces/" + defaultNamespace + "/services/http:loki:3100/proxy/flush").Do(ctx).Raw()
	assert.NoErr(err)
	backoffLimit := int32(0)
	activeDeadlineSeconds := int64(120)
	_, err = kube.BatchV1().Jobs(defaultNamespace).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-object-storage"},
		Spec: batchv1.JobSpec{BackoffLimit: &backoffLimit, ActiveDeadlineSeconds: &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name: "verify", Image: "public.ecr.aws/aws-cli/aws-cli:2.38.1",
					Command: []string{"/bin/sh", "-ec"},
					Args:    []string{`attempt=0; while [ "$attempt" -lt 45 ]; do count=$(aws --endpoint-url http://rustfs-svc:9000 s3api list-objects-v2 --bucket "$MANAGEMENT_BUCKET_NAME" --prefix fake/ --max-keys 1 --no-paginate --query KeyCount --output text); if [ "$count" -gt 0 ]; then exit 0; fi; attempt=$((attempt+1)); sleep 2; done; echo 'Loki did not persist a chunk in RustFS' >&2; exit 1`},
					Env: []corev1.EnvVar{
						{Name: "MANAGEMENT_BUCKET_NAME", Value: platform.ManagementBucketName(managementClusterName)},
						{Name: "AWS_DEFAULT_REGION", Value: "us-east-1"},
						{Name: "AWS_EC2_METADATA_DISABLED", Value: "true"},
						{Name: "AWS_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "borq-rustfs"}, Key: "RUSTFS_ACCESS_KEY"}}},
						{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "borq-rustfs"}, Key: "RUSTFS_SECRET_KEY"}}},
					},
				}},
			}},
		},
	}, metav1.CreateOptions{})
	assert.NoErr(err)
	for {
		job, err := kube.BatchV1().Jobs(defaultNamespace).Get(ctx, "bootstrap-object-storage", metav1.GetOptions{})
		assert.NoErr(err)
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				return
			}
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				pods, _ := kube.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=bootstrap-object-storage"})
				for _, pod := range pods.Items {
					logs, _ := kube.CoreV1().Pods(defaultNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
					t.Logf("object storage verification: %s", logs)
				}
				t.Fatal("Loki did not persist a log chunk in development RustFS")
			}
		}
		bootstrapPause(t, ctx)
	}
}

func bootstrapServiceGet(t *testing.T, ctx context.Context, config *rest.Config, service, path string) []byte {
	t.Helper()
	client, err := rest.HTTPClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, config.Host+"/api/v1/namespaces/"+defaultNamespace+"/services/"+service+"/proxy"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("management service %s returned %d: %s", service, response.StatusCode, body)
	}
	return body
}

func bootstrapFreePort(t *testing.T) int32 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", listener.Addr())
	}
	return int32(address.Port)
}

func bootstrapRequest(t *testing.T, ctx context.Context, endpoint, method, path string, body map[string]any, expectedStatus int) map[string]any {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("%s %s returned %d: %s", method, path, response.StatusCode, encoded)
	}
	var result map[string]any
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func bootstrapString(t *testing.T, value map[string]any, key string) string {
	t.Helper()
	text, ok := value[key].(string)
	if !ok || text == "" {
		t.Fatalf("response did not contain string %s", key)
	}
	return text
}

func bootstrapPause(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(time.Second):
	}
}

func bootstrapDiagnostics(t *testing.T, path string) {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return
	}
	kube, err := kubeclient.NewForConfig(config)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pod := range pods.Items {
			for _, container := range pod.Status.ContainerStatuses {
				state, _ := json.Marshal(container.State)
				t.Logf("bootstrap diagnostic pod=%s container=%s phase=%s ready=%t state=%s", pod.Name, container.Name, pod.Status.Phase, container.Ready, state)
				if container.Name == borqName && !container.Ready || container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
					lines := int64(40) // The Kubernetes SDK uses a pointer to distinguish an omitted log limit.
					logs, err := kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &lines}).DoRaw(ctx)
					if err == nil {
						t.Logf("bootstrap diagnostic pod=%s container=%s logs: %s", pod.Name, container.Name, logs)
					}
				}
			}
		}
	}
	events, err := kube.CoreV1().Events("").List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err == nil {
		for _, event := range events.Items {
			t.Logf("bootstrap diagnostic %s/%s %s: %s", event.InvolvedObject.Kind, event.InvolvedObject.Name, event.Reason, event.Message)
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
