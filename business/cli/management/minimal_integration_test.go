//go:build integration

package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestManagementBootstrapMinimalIntegration proves API-only bootstrap, separate
// key export, autonomous service installation and persistence across a Borq restart.
func TestManagementBootstrapMinimalIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG")
	image := os.Getenv("BORQ_INTEGRATION_BOOTSTRAP_IMAGE")
	if kubeconfig == "" || image == "" {
		t.Skip("requires a disposable cluster via BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG and a loaded BORQ_INTEGRATION_BOOTSTRAP_IMAGE")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	t.Chdir(t.TempDir())
	configuration, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	assert.NoErr(err)
	kube, err := kubeclient.NewForConfig(configuration)
	assert.NoErr(err)
	request := DefaultRequest()
	request.ClusterName, request.KubeconfigPath, request.BorqImage = "", kubeconfig, image
	request.StorageClass = "standard"
	request.Timeout = 3 * time.Minute
	result, err := Execute(ctx, logger.Logger{}, request)
	assert.NoErr(err)
	assert.Equal(result.Namespace, defaultNamespace)
	assert.True(result.KubernetesProxy)
	assert.True(result.ManagementURL == "")
	deployment, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, "--management-bootstrap-only"))
	service, err := kube.CoreV1().Services(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(service.Spec.Type, corev1.ServiceTypeClusterIP)
	assert.Equal(service.Spec.Ports[0].NodePort, int32(0))
	claim, err := kube.CoreV1().PersistentVolumeClaims(defaultNamespace).Get(ctx, borqDataClaimName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(claim.Status.Phase, corev1.ClaimBound)
	secret, err := kube.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-admin-auth", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(result.AdminToken == string(secret.Data["token"]))
	recoveryFile, err := ExportKeys(ctx, ExportKeysInput{KubeconfigPath: kubeconfig, OutputFile: filepath.Join(t.TempDir(), "recovery.json"), Timeout: time.Minute})
	assert.NoErr(err)
	recoveryBytes, err := os.ReadFile(recoveryFile)
	assert.NoErr(err)
	var recovery bootstrapRecovery
	assert.NoErr(json.Unmarshal(recoveryBytes, &recovery))
	vaultSecret, err := kube.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(recovery.Borq.AdminToken == result.AdminToken)
	assert.True(recovery.OpenBao.OriginalToken == string(vaultSecret.Data["token"]))
	assert.True(recovery.OpenBao.SealKey == string(vaultSecret.Data["sealKey"]))
	assert.True(recovery.OpenBao.SealKey != "")
	info, err := os.Stat(recoveryFile)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	client, err := appapi.NewClientWithResponses(result.ApiUrl, appapi.WithHTTPClient(result.HttpClient))
	assert.NoErr(err)
	organizations, err := client.ListOrganizationsWithResponse(ctx)
	assert.NoErr(err)
	assert.True(organizations.JSON200 != nil)
	assert.Equal(len(*organizations.JSON200), 0)
	deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "BORQ_BOOTSTRAP_RETAIN_TEST", Value: "retained"})
	_, err = kube.AppsV1().Deployments(defaultNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
	assert.NoErr(err)
	bootstrapWaitForAvailable(t, ctx, kube)
	repeated, err := Execute(ctx, logger.Logger{}, request)
	assert.NoErr(err)
	assert.Equal(repeated.ClusterName, result.ClusterName)
	assert.True(repeated.AdminToken == result.AdminToken)
	_, err = ExportKeys(ctx, ExportKeysInput{KubeconfigPath: kubeconfig, OutputFile: recoveryFile, Timeout: time.Minute})
	assert.NoErr(err)
	retainedRecovery, err := os.ReadFile(recoveryFile)
	assert.NoErr(err)
	assert.True(string(retainedRecovery) == string(recoveryBytes))
	retainedClaim, err := kube.CoreV1().PersistentVolumeClaims(defaultNamespace).Get(ctx, borqDataClaimName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retainedClaim.UID, claim.UID)
	retainedSecret, err := kube.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-admin-auth", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retainedSecret.UID, secret.UID)
	retainedServer, err := kube.AppsV1().Deployments(defaultNamespace).Get(ctx, borqName, metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(slices.Contains(retainedServer.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "BORQ_BOOTSTRAP_RETAIN_TEST", Value: "retained"}))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	vault, err := platform.OpenManagementVault(ctx, connection, defaultNamespace)
	assert.NoErr(err)
	assert.NoErr(vault.WaitForActive(ctx, time.Second, 3*time.Minute))
	assert.NoErr(vault.EnsureKvV2(ctx, "bootstrap-test"))
	after, err := kube.CoreV1().Secrets(defaultNamespace).Get(ctx, "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	assert.True(string(after.Data["sealKey"]) == recovery.OpenBao.SealKey)
	assert.True(string(after.Data["token"]) == recovery.OpenBao.OriginalToken)
}
