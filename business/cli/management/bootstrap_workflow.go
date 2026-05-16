package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"strings"
	"time"

	helmfoundation "github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/zarf/bootstrap"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	fluxNamespace               = "borq-system"
	fluxInstanceName            = "flux"
	fluxPullSecretName          = "flux-system"
	fluxOperatorReleaseName     = "flux-operator"
	fluxOperatorChartRef        = "oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator"
	fluxOperatorChartVersion    = "0.48.0"
	fluxDistributionVersion     = "2.8.x"
	externalSecretsNamespace    = "external-secrets"
	externalSecretsChartName    = "external-secrets"
	externalSecretsChartRepo    = "https://charts.external-secrets.io"
	externalSecretsChartVersion = "2.4.1"
	giteaReleaseName            = "gitea"
	giteaAdminSecretName        = "gitea-admin"
	giteaAdminUsername          = "gitea_admin"
	giteaChartRef               = "gitea"
	giteaChartRepoUrl           = "https://dl.gitea.com/charts/"
	giteaChartVersion           = "12.5.3"
	giteaRepoName               = "borq-fleet"
)

type managementBootstrapWorkflow struct {
	connection     kubernetes.Connection
	helmReleases   helmfoundation.ReleaseManager
	kubeconfigPath string
	log            logger.Logger
	request        Request
}

type giteaAdminCredentials struct {
	password string
	username string
}

func runManagementBootstrapWorkflow(
	ctx context.Context,
	log logger.Logger,
	request Request,
	connection kubernetes.Connection,
) error {
	helmReleases, err := helmfoundation.NewReleaseManager(helmfoundation.NewReleaseManagerInput{
		Connection:     connection,
		KubeconfigPath: strings.TrimSpace(request.KubeconfigPath),
	})
	if err != nil {
		return fmt.Errorf("create helm release manager: %w", err)
	}
	defer helmReleases.Close()

	workflow := managementBootstrapWorkflow{
		connection:     connection,
		helmReleases:   helmReleases,
		kubeconfigPath: strings.TrimSpace(request.KubeconfigPath),
		log:            log,
		request:        request,
	}

	return workflow.run(ctx)
}

func (workflow managementBootstrapWorkflow) run(ctx context.Context) error {
	if err := workflow.applyNamespaces(ctx); err != nil {
		return err
	}

	credentials, err := workflow.ensureGiteaAdminSecret(ctx)
	if err != nil {
		return err
	}

	if err := workflow.installGitea(ctx); err != nil {
		return err
	}

	if err := workflow.waitForGiteaApiAndSeedRepo(ctx, credentials); err != nil {
		return err
	}

	if err := workflow.installFluxOperator(ctx); err != nil {
		return err
	}

	connection, err := kubernetes.NewConnection(
		kubernetes.Config{KubeconfigPath: workflow.kubeconfigPath},
		workflow.log,
		kubernetes.WithFieldOwner(defaultFieldOwner),
	)
	if err != nil {
		return fmt.Errorf("refresh kubernetes connection after flux operator install: %w", err)
	}
	workflow.connection = connection

	if err := workflow.ensureFluxPullSecret(ctx, credentials); err != nil {
		return err
	}

	if err := workflow.applyFluxInstance(ctx); err != nil {
		return err
	}

	if err := workflow.waitForFluxControllers(ctx); err != nil {
		return err
	}

	if err := borqApp(workflow.request).Apply(ctx, workflow.connection); err != nil {
		return fmt.Errorf("apply borq app: %w", err)
	}
	if err := borqApp(workflow.request).WaitReady(ctx, workflow.connection); err != nil {
		return fmt.Errorf("wait for borq app: %w", err)
	}

	return nil
}

func (workflow managementBootstrapWorkflow) applyNamespaces(ctx context.Context) error {
	workflow.log.Info("applying_management_namespaces", logger.WithFields(
		logger.String("management_namespace", workflow.request.Namespace),
	))

	if err := workflow.connection.Namespaces().Apply(ctx, managementNamespace(workflow.request.Namespace)); err != nil {
		return fmt.Errorf("apply management namespace %q: %w", workflow.request.Namespace, err)
	}

	return nil
}

func (workflow managementBootstrapWorkflow) ensureGiteaAdminSecret(ctx context.Context) (giteaAdminCredentials, error) {
	existingSecret, err := workflow.connection.Secrets().Get(ctx, workflow.request.Namespace, giteaAdminSecretName)
	if err == nil {
		username := strings.TrimSpace(existingSecret.Data["username"])
		if username == "" {
			username = giteaAdminUsername
		}

		password := strings.TrimSpace(existingSecret.Data["password"])
		if password == "" {
			password, err = randomHex(24)
			if err != nil {
				return giteaAdminCredentials{}, fmt.Errorf("generate gitea admin password: %w", err)
			}
		}

		secret := kubernetes.Secret{
			Name:      giteaAdminSecretName,
			Namespace: workflow.request.Namespace,
			Labels:    appLabels(giteaReleaseName, "credentials"),
			Data: map[string]string{
				"username": username,
				"password": password,
			},
		}
		if err := workflow.connection.Secrets().Apply(ctx, secret); err != nil {
			return giteaAdminCredentials{}, fmt.Errorf("normalize gitea admin secret %q: %w", giteaAdminSecretName, err)
		}

		return giteaAdminCredentials{username: username, password: password}, nil
	}
	if !apierrors.IsNotFound(err) {
		return giteaAdminCredentials{}, fmt.Errorf("get gitea admin secret %q: %w", giteaAdminSecretName, err)
	}

	password, err := randomHex(24)
	if err != nil {
		return giteaAdminCredentials{}, fmt.Errorf("generate gitea admin password: %w", err)
	}

	secret := kubernetes.Secret{
		Name:      giteaAdminSecretName,
		Namespace: workflow.request.Namespace,
		Labels:    appLabels(giteaReleaseName, "credentials"),
		Data: map[string]string{
			"username": giteaAdminUsername,
			"password": password,
		},
	}
	if err := workflow.connection.Secrets().Apply(ctx, secret); err != nil {
		return giteaAdminCredentials{}, fmt.Errorf("apply gitea admin secret %q: %w", giteaAdminSecretName, err)
	}

	return giteaAdminCredentials{username: giteaAdminUsername, password: password}, nil
}

func (workflow managementBootstrapWorkflow) installGitea(ctx context.Context) error {
	workflow.log.Info("installing_gitea_with_helm_sdk", logger.WithFields(
		logger.String("namespace", workflow.request.Namespace),
		logger.String("release_name", giteaReleaseName),
		logger.String("chart", giteaChartRef),
		logger.String("version", giteaChartVersion),
	))

	return workflow.helmReleases.InstallOrUpgrade(ctx, helmfoundation.InstallOrUpgradeInput{
		Namespace:     workflow.request.Namespace,
		ReleaseName:   giteaReleaseName,
		ChartRef:      giteaChartRef,
		RepositoryUrl: giteaChartRepoUrl,
		Version:       giteaChartVersion,
		Values:        giteaHelmValues(giteaAdminSecretName),
		Timeout:       5 * time.Minute,
	})
}

func (workflow managementBootstrapWorkflow) waitForGiteaApiAndSeedRepo(ctx context.Context, credentials giteaAdminCredentials) error {
	localPort, err := freeLocalPort()
	if err != nil {
		return fmt.Errorf("allocate gitea port-forward port: %w", err)
	}

	session, err := startPortForward(
		ctx,
		workflow.kubeconfigPath,
		workflow.request.Namespace,
		"service/"+giteaHttpServiceName(),
		localPort,
		3000,
	)
	if err != nil {
		return fmt.Errorf("start gitea port-forward: %w", err)
	}
	defer session.Close()

	baseUrl := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	if err := waitForGiteaApi(ctx, baseUrl); err != nil {
		return err
	}
	if err := ensureGiteaRepo(ctx, baseUrl, credentials); err != nil {
		return err
	}
	if err := seedFleetRepo(ctx, baseUrl, credentials, workflow.seedRepoFiles()); err != nil {
		return err
	}

	return nil
}

func (workflow managementBootstrapWorkflow) installFluxOperator(ctx context.Context) error {
	workflow.log.Info("installing_flux_operator_with_helm_sdk", logger.WithFields(
		logger.String("namespace", fluxNamespace),
		logger.String("release_name", fluxOperatorReleaseName),
		logger.String("chart", fluxOperatorChartRef),
		logger.String("version", fluxOperatorChartVersion),
	))

	return workflow.helmReleases.InstallOrUpgrade(ctx, helmfoundation.InstallOrUpgradeInput{
		Namespace:   fluxNamespace,
		ReleaseName: fluxOperatorReleaseName,
		ChartRef:    fluxOperatorChartRef,
		Version:     fluxOperatorChartVersion,
		Values:      map[string]any{},
		Timeout:     5 * time.Minute,
	})
}

func (workflow managementBootstrapWorkflow) ensureFluxPullSecret(ctx context.Context, credentials giteaAdminCredentials) error {
	secret := kubernetes.Secret{
		Name:      fluxPullSecretName,
		Namespace: fluxNamespace,
		Labels:    appLabels(fluxInstanceName, "credentials"),
		Data: map[string]string{
			"username": credentials.username,
			"password": credentials.password,
		},
	}
	if err := workflow.connection.Secrets().Apply(ctx, secret); err != nil {
		return fmt.Errorf("apply flux pull secret %q: %w", fluxPullSecretName, err)
	}

	return nil
}

func (workflow managementBootstrapWorkflow) applyFluxInstance(ctx context.Context) error {
	fluxYAML, err := bootstrap.Render("flux.yaml", bootstrap.Data{
		Namespace:   workflow.request.Namespace,
		ClusterName: workflow.request.ClusterName,
		SyncUrl:     workflow.giteaSyncUrl(),
		ClusterPath: workflow.clusterSyncPath(),
	})
	if err != nil {
		return fmt.Errorf("render flux template: %w", err)
	}

	fluxInstance, err := parseResourceFromYAML(fluxYAML, "FluxInstance", fluxInstanceName)
	if err != nil {
		return fmt.Errorf("find FluxInstance in rendered YAML: %w", err)
	}

	if err := workflow.connection.Objects().Apply(ctx, fluxInstance); err != nil {
		return fmt.Errorf("apply FluxInstance %q: %w", fluxInstanceName, err)
	}

	return nil
}

func (workflow managementBootstrapWorkflow) waitForFluxControllers(ctx context.Context) error {
	controllerNames := []string{
		"source-controller",
		"kustomize-controller",
		"helm-controller",
		"notification-controller",
	}

	for _, controllerName := range controllerNames {
		if err := workflow.connection.Deployments().WaitForAvailable(ctx, fluxNamespace, controllerName); err != nil {
			return fmt.Errorf("wait for flux controller %q: %w", controllerName, err)
		}
	}

	return nil
}

func (workflow managementBootstrapWorkflow) seedRepoFiles() map[string]string {
	clusterPath := workflow.clusterSyncPath()
	data := bootstrap.Data{
		Namespace:   workflow.request.Namespace,
		ClusterName: workflow.request.ClusterName,
		SyncUrl:     workflow.giteaSyncUrl(),
		ClusterPath: clusterPath,
	}

	files := map[string]string{}
	for _, name := range []string{"kustomization.yaml", "crds/kustomization.yaml", "repos.yaml", "cert-manager.yaml", "gitea.yaml", "flux.yaml", "external-secrets.yaml", "prometheus.yaml", "loki.yaml", "alloy.yaml", "grafana.yaml"} {
		rendered, err := bootstrap.Render(name, data)
		if err != nil {
			panic(err)
		}
		files[filepath.ToSlash(filepath.Join(clusterPath, name))] = rendered
	}
	return files
}

func (workflow managementBootstrapWorkflow) clusterSyncPath() string {
	return filepath.ToSlash(filepath.Join("clusters", workflow.request.ClusterName))
}

func (workflow managementBootstrapWorkflow) giteaSyncUrl() string {
	return fmt.Sprintf(
		"http://%s.%s.svc.cluster.local:3000/%s/%s.git",
		giteaHttpServiceName(),
		workflow.request.Namespace,
		giteaAdminUsername,
		giteaRepoName,
	)
}

func giteaHelmValues(existingSecretName string) map[string]any {
	return map[string]any{
		"postgresql": map[string]any{
			"enabled": true,
		},
		"postgresql-ha": map[string]any{
			"enabled": false,
		},
		"valkey": map[string]any{
			"enabled": false,
		},
		"valkey-cluster": map[string]any{
			"enabled": false,
		},
		"persistence": map[string]any{
			"enabled": true,
			"size":    "10Gi",
		},
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    "100m",
				"memory": "200Mi",
			},
		},
		"service": map[string]any{
			"http": map[string]any{
				"clusterIP": "",
			},
			"ssh": map[string]any{
				"clusterIP": "",
			},
		},
		"gitea": map[string]any{
			"admin": map[string]any{
				"existingSecret": existingSecretName,
			},
			"metrics": map[string]any{
				"enabled": true,
				"serviceMonitor": map[string]any{
					"enabled": false,
				},
			},
			"config": map[string]any{
				"database": map[string]any{
					"DB_TYPE": "postgres",
				},
				"session": map[string]any{
					"PROVIDER": "memory",
				},
				"cache": map[string]any{
					"ADAPTER": "memory",
				},
				"queue": map[string]any{
					"TYPE": "level",
				},
			},
		},
	}
}

func giteaHttpServiceName() string {
	return giteaReleaseName + "-http"
}

func parseResourceFromYAML(yamlContent, kind, name string) (*unstructured.Unstructured, error) {
	decoder := apimachineryyaml.NewYAMLOrJSONDecoder(strings.NewReader(yamlContent), 4096)
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("resource %s/%s not found in YAML", kind, name)
		}
		if err != nil {
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		if obj.GetKind() == kind && obj.GetName() == name {
			return &obj, nil
		}
	}
}

func waitForGiteaApi(ctx context.Context, baseUrl string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	client := &http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseUrl+"/api/healthz", nil)
		if err != nil {
			return fmt.Errorf("build gitea health request: %w", err)
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
			return fmt.Errorf("gitea api at %q did not become ready before timeout: %w", baseUrl, ctx.Err())
		case <-ticker.C:
		}
	}
}

func ensureGiteaRepo(ctx context.Context, baseUrl string, credentials giteaAdminCredentials) error {
	getRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/api/v1/repos/%s/%s", baseUrl, credentials.username, giteaRepoName),
		nil,
	)
	if err != nil {
		return fmt.Errorf("build gitea repo lookup request: %w", err)
	}
	getRequest.SetBasicAuth(credentials.username, credentials.password)

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(getRequest)
	if err != nil {
		return fmt.Errorf("lookup gitea repo %q: %w", giteaRepoName, err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("lookup gitea repo %q: unexpected status %d: %s", giteaRepoName, response.StatusCode, strings.TrimSpace(string(body)))
	}

	createBody, err := json.Marshal(map[string]any{
		"name":           giteaRepoName,
		"default_branch": "main",
		"private":        true,
	})
	if err != nil {
		return fmt.Errorf("marshal gitea repo create request: %w", err)
	}

	createRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseUrl+"/api/v1/user/repos", bytes.NewReader(createBody))
	if err != nil {
		return fmt.Errorf("build gitea repo create request: %w", err)
	}
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.SetBasicAuth(credentials.username, credentials.password)

	createResponse, err := client.Do(createRequest)
	if err != nil {
		return fmt.Errorf("create gitea repo %q: %w", giteaRepoName, err)
	}
	defer createResponse.Body.Close()

	if createResponse.StatusCode != http.StatusCreated && createResponse.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(createResponse.Body)
		return fmt.Errorf("create gitea repo %q: unexpected status %d: %s", giteaRepoName, createResponse.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

func seedFleetRepo(
	ctx context.Context,
	baseUrl string,
	credentials giteaAdminCredentials,
	files map[string]string,
) error {
	tempDir, err := os.MkdirTemp("", "borq-fleet-repo-*")
	if err != nil {
		return fmt.Errorf("create temporary fleet repo directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("create temporary fleet repo worktree: %w", err)
	}

	repoUrl, err := giteaRepoGitUrl(baseUrl, credentials)
	if err != nil {
		return err
	}

	if _, err := runGitCommandInDir(ctx, repoDir, "git", "init", "-b", "main"); err != nil {
		return fmt.Errorf("initialize temporary fleet repo: %w", err)
	}
	if _, err := runGitCommandInDir(ctx, repoDir, "git", "config", "user.name", "borq bootstrap"); err != nil {
		return fmt.Errorf("configure temporary fleet repo author name: %w", err)
	}
	if _, err := runGitCommandInDir(ctx, repoDir, "git", "config", "user.email", "borq-bootstrap@local"); err != nil {
		return fmt.Errorf("configure temporary fleet repo author email: %w", err)
	}
	if _, err := runGitCommandInDir(ctx, repoDir, "git", "remote", "add", "origin", repoUrl); err != nil {
		return fmt.Errorf("configure temporary fleet repo remote: %w", err)
	}

	if _, err := runGitCommandInDir(ctx, repoDir, "git", "fetch", "origin", "main"); err == nil {
		if _, err := runGitCommandInDir(ctx, repoDir, "git", "reset", "--hard", "FETCH_HEAD"); err != nil {
			return fmt.Errorf("reset temporary fleet repo to origin/main: %w", err)
		}
	}

	for path, contents := range files {
		fullPath := filepath.Join(repoDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("create fleet repo directory for %q: %w", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
			return fmt.Errorf("write fleet repo file %q: %w", path, err)
		}
	}

	if _, err := runGitCommandInDir(ctx, repoDir, "git", "add", "."); err != nil {
		return fmt.Errorf("stage fleet repo files: %w", err)
	}

	statusOutput, err := runGitCommandInDir(ctx, repoDir, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("inspect fleet repo status: %w", err)
	}
	if strings.TrimSpace(statusOutput) == "" {
		return nil
	}

	if _, err := runGitCommandInDir(ctx, repoDir, "git", "commit", "-m", "Bootstrap borq fleet manifests"); err != nil {
		return fmt.Errorf("commit fleet repo bootstrap manifests: %w", err)
	}
	if _, err := runGitCommandInDir(ctx, repoDir, "git", "push", "origin", "HEAD:main"); err != nil {
		return fmt.Errorf("push fleet repo bootstrap manifests: %w", err)
	}

	return nil
}

func giteaRepoGitUrl(baseUrl string, credentials giteaAdminCredentials) (string, error) {
	parsedUrl, err := url.Parse(baseUrl)
	if err != nil {
		return "", fmt.Errorf("parse gitea base url %q: %w", baseUrl, err)
	}
	parsedUrl.User = url.UserPassword(credentials.username, credentials.password)
	parsedUrl.Path = fmt.Sprintf("/%s/%s.git", credentials.username, giteaRepoName)
	return parsedUrl.String(), nil
}

func randomHex(byteCount int) (string, error) {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("listener address was %T, want *net.TCPAddr", listener.Addr())
	}

	return address.Port, nil
}

type portForwardSession struct {
	cancel func()
	done   chan error
}

func (session portForwardSession) Close() error {
	session.cancel()
	select {
	case err := <-session.done:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		return nil
	}
}

func startPortForward(
	ctx context.Context,
	kubeconfigPath string,
	namespace string,
	resource string,
	localPort int,
	remotePort int,
) (portForwardSession, error) {
	portForwardContext, cancel := context.WithCancel(ctx)
	args := []string{}
	if strings.TrimSpace(kubeconfigPath) != "" {
		args = append(args, "--kubeconfig", kubeconfigPath)
	}
	args = append(args, "-n", namespace, "port-forward", resource, fmt.Sprintf("%d:%d", localPort, remotePort))

	command := exec.CommandContext(portForwardContext, "kubectl", args...)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr

	if err := command.Start(); err != nil {
		cancel()
		return portForwardSession{}, fmt.Errorf("start kubectl port-forward: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()

	dialAddress := fmt.Sprintf("127.0.0.1:%d", localPort)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		connection, err := net.DialTimeout("tcp", dialAddress, 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return portForwardSession{cancel: cancel, done: done}, nil
		}

		select {
		case err := <-done:
			cancel()
			if err == nil {
				return portForwardSession{}, fmt.Errorf("kubectl port-forward exited before becoming ready")
			}
			return portForwardSession{}, fmt.Errorf("kubectl port-forward failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			cancel()
			return portForwardSession{}, fmt.Errorf("wait for kubectl port-forward readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func runGitCommandInDir(ctx context.Context, dir string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		commandLine := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText == "" {
			return "", fmt.Errorf("%s failed: %w", commandLine, err)
		}
		return "", fmt.Errorf("%s failed: %w: %s", commandLine, err, stderrText)
	}

	return stdout.String(), nil
}
