//go:build integration

package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestPocketIdIntegration proves private startup, real cert-manager issuance,
// encrypted archives, group environment overrides,
// idempotent installation, pod-restart persistence and independent Litestream
// recovery from management RustFS. It uses isolated resources on an existing
// cluster with cert-manager and Traefik.
func TestPocketIdIntegration(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	is.NoErr(err)
	configuration := connection.RestConfig()
	client, err := ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	is.NoErr(err)
	namespace := fmt.Sprintf("borq-pocketid-%d", time.Now().UnixNano())
	identity := strings.ReplaceAll(uuid.NewString(), "-", "")
	hostname := namespace + ".example.test"
	issuer := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: namespace}, Spec: certmanagerv1.IssuerSpec{
		IssuerConfig: certmanagerv1.IssuerConfig{SelfSigned: &certmanagerv1.SelfSignedIssuer{}},
	}}
	is.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Errorf("delete test namespace: %v", err)
		}
		_ = client.Delete(cleanup, &issuer)
	})
	is.NoErr(client.Create(ctx, &issuer))
	backupStore, backupCredentials := pocketIdBackupFixture(t, ctx, kubeconfigPath, connection, client, namespace, identity)
	archive, err := sqliteworkload.NewArchiveKey()
	is.NoErr(err)
	input := PocketIdInput{ArchiveKey: archive, Connection: connection, Namespace: namespace, InstanceID: identity,
		IssuerURL: "https://" + hostname, StorageClass: "local-path", ClusterIssuer: namespace,
		EncryptionKey: rand.Text() + rand.Text(), ApiKey: rand.Text() + rand.Text(), Timeout: 2 * time.Minute, BackupCredentials: backupCredentials,
		Values: map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "75m"}},
			"podAnnotations": map[string]any{"example.test/chart-override": "retained"}, "config": map[string]any{"appName": "Managed Identity"}}}
	// Start with encrypted archives from the first write; reconciliation below
	// must preserve this real database and its original volume.
	resources, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	workload := resources[2].(*appsv1.Deployment)
	for _, index := range []int{0, 1, 4, 5, 6, 7, 3, 2} {
		is.NoErr(client.Create(ctx, resources[index]))
	}
	is.NoErr(connection.Deployments().WaitForRollout(ctx, namespace, workload.Name))
	endpoint := PocketIdEndpoint{Namespace: namespace, ServiceName: workload.Name, Port: 1411}
	key := ctrlclient.ObjectKey{Namespace: namespace, Name: endpoint.ServiceName}
	var ingresses gatewayv1.HTTPRouteList
	is.NoErr(client.List(ctx, &ingresses, ctrlclient.InNamespace(namespace)))
	is.Equal(len(ingresses.Items), 0)
	var service corev1.Service
	is.NoErr(client.Get(ctx, key, &service))
	is.Equal(service.Spec.Type, corev1.ServiceTypeClusterIP)
	is.Equal(len(service.Spec.ExternalIPs), 0)
	var claim corev1.PersistentVolumeClaim
	is.NoErr(client.Get(ctx, key, &claim))
	is.Equal(claim.Status.Phase, corev1.ClaimBound)

	// Probe from another pod, where tenant workloads can reach Pod IPs directly.
	// A positive health request distinguishes listener isolation from no routing.
	var pods corev1.PodList
	is.NoErr(client.List(ctx, &pods, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{"borq.io/pocket-id": identity}))
	is.Equal(len(pods.Items), 1)
	is.Equal(pods.Items[0].Spec.Containers[0].Resources.Requests.Cpu().String(), "75m")
	is.Equal(pods.Items[0].Annotations["example.test/chart-override"], "retained")
	probe := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tenant-setup-probe", Namespace: namespace}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, SecurityContext: pods.Items[0].Spec.SecurityContext,
		AutomountServiceAccountToken: pods.Items[0].Spec.AutomountServiceAccountToken,
		Containers: []corev1.Container{{Name: "probe", Image: pods.Items[0].Spec.Containers[1].Image,
			Command:                  []string{"sh", "-c", pocketIdBoundaryProbe()},
			Env:                      []corev1.EnvVar{{Name: "POCKET_HOST", Value: pods.Items[0].Status.PodIP}},
			SecurityContext:          pods.Items[0].Spec.Containers[1].SecurityContext,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
	}}
	is.NoErr(client.Create(ctx, &probe))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(&probe), &probe))
		if probe.Status.Phase == corev1.PodSucceeded {
			break
		}
		if probe.Status.Phase == corev1.PodFailed {
			for _, status := range probe.Status.ContainerStatuses {
				if status.State.Terminated != nil {
					t.Fatalf("private setup boundary probe failed: %s", status.State.Terminated.Message)
				}
			}
			t.Fatal("private setup boundary probe failed")
		}
		select {
		case <-ctx.Done():
			t.Fatal("private setup boundary probe did not finish")
		case <-ticker.C:
		}
	}

	transport, err := rest.HTTPClientFor(&configuration)
	is.NoErr(err)
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	baseURL := strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:" + endpoint.ServiceName + ":1411/proxy"
	provider, err := pocketid.New(pocketid.Config{BaseURL: baseURL, ApiKey: input.ApiKey, HttpClient: transport})
	is.NoErr(err)
	admin := pocketid.Admin{ID: uuid.NewString(), Username: "cluster-admin", Email: "admin@example.test"}
	is.NoErr(provider.EnsureAdmin(ctx, admin))
	oidcClient := pocketid.OidcClient{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), Name: "Backup recovery test",
		CallbackURL: "https://borq.example.test/callback", LogoutCallbackURL: "https://borq.example.test/login", LaunchURL: "https://borq.example.test/", Secret: rand.Text() + rand.Text()}
	is.NoErr(provider.EnsureClient(ctx, oidcClient))
	enrollmentToken, err := provider.NewOneTimeAccess(ctx, admin.ID)
	is.NoErr(err)
	for _, visit := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/lc/" + enrollmentToken, http.StatusOK},
		{http.MethodGet, "/login/alternative/code?code=" + enrollmentToken, http.StatusOK},
		{http.MethodPost, "/api/one-time-access-token/" + enrollmentToken, http.StatusOK},
		{http.MethodPost, "/api/one-time-access-token/" + enrollmentToken, http.StatusUnauthorized},
	} {
		request, err := http.NewRequestWithContext(ctx, visit.method, baseURL+visit.path, nil)
		is.NoErr(err)
		request.Header.Set("Referer", input.IssuerURL+"/lc/"+enrollmentToken)
		response, err := transport.Do(request)
		is.NoErr(err)
		response.Body.Close()
		is.Equal(response.StatusCode, visit.status)
	}
	for _, container := range []string{"pocket-id", "setup-guard"} {
		logURL := strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/pods/" + pods.Items[0].Name + "/log?container=" + container
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, logURL, nil)
		is.NoErr(err)
		response, err := transport.Do(request)
		is.NoErr(err)
		logs, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		is.NoErr(err)
		is.Equal(response.StatusCode, http.StatusOK)
		if bytes.Contains(logs, []byte(enrollmentToken)) {
			t.Fatalf("%s container logs exposed the enrollment credential", container)
		}
	}
	missingIssuerInput := input
	missingIssuerInput.ClusterIssuer += "-missing"
	is.NoErr(PublishPocketId(ctx, missingIssuerInput))
	certificateKey := ctrlclient.ObjectKey{Namespace: namespace, Name: endpoint.ServiceName + "-tls"}
	for {
		var certificate certmanagerv1.Certificate
		err := client.Get(ctx, certificateKey, &certificate)
		unready := false
		if err == nil && certificate.Spec.IssuerRef.Name == missingIssuerInput.ClusterIssuer {
			for _, condition := range certificate.Status.Conditions {
				if condition.Type == certmanagerv1.CertificateConditionReady && condition.Status == "False" && condition.ObservedGeneration == certificate.Generation {
					unready = true
				}
			}
		}
		if unready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("cert-manager did not observe the missing issuer")
		case <-ticker.C:
		}
	}
	is.NoErr(PublishPocketId(ctx, input))
	for {
		var certificate certmanagerv1.Certificate
		err := client.Get(ctx, certificateKey, &certificate)
		ready := false
		if err == nil {
			for _, condition := range certificate.Status.Conditions {
				if condition.Type == certmanagerv1.CertificateConditionReady && condition.Status == "True" && condition.ObservedGeneration == certificate.Generation {
					ready = true
				}
			}
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("cert-manager did not issue the Pocket ID HTTPRoute certificate")
		case <-ticker.C:
		}
	}
	var certificateSecret corev1.Secret
	is.NoErr(client.Get(ctx, certificateKey, &certificateSecret))
	block, _ := pem.Decode(certificateSecret.Data["tls.crt"])
	is.True(block != nil)
	certificate, err := x509.ParseCertificate(block.Bytes)
	is.NoErr(err)
	is.NoErr(certificate.VerifyHostname(hostname))
	is.NoErr(PublishPocketId(ctx, input)) // Real controller ownership must permit an unchanged retry.
	var before appsv1.Deployment
	is.NoErr(client.Get(ctx, key, &before))
	input.Values = map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}}, "config": map[string]any{"appName": "Updated Identity"}}
	input.Environment = map[string]string{"APP_NAME": "Group Identity", "SMTP_PASSWORD": rand.Text()}
	_, err = InstallPocketId(ctx, input)
	is.NoErr(err)
	var updated appsv1.Deployment
	is.NoErr(client.Get(ctx, key, &updated))
	is.True(updated.Generation > before.Generation)
	is.Equal(updated.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(), "100m")
	is.Equal(updated.Spec.Template.Annotations["example.test/chart-override"], "")
	var config corev1.Secret
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: key.Name + "-config"}, &config))
	is.Equal(string(config.Data["APP_NAME"]), "Group Identity")
	is.Equal(string(config.Data["SMTP_PASSWORD"]), input.Environment["SMTP_PASSWORD"])
	// Reading the process catches a Secret-after-rollout race that resource
	// readback alone misses: envFrom is captured only when the container starts.
	configRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/application-configuration", nil)
	is.NoErr(err)
	configResponse, err := transport.Do(configRequest)
	is.NoErr(err)
	is.Equal(configResponse.StatusCode, http.StatusOK)
	var runningConfig []struct{ Key, Value string }
	is.NoErr(json.NewDecoder(configResponse.Body).Decode(&runningConfig))
	configResponse.Body.Close()
	appName := ""
	for _, variable := range runningConfig {
		if variable.Key == "appName" {
			appName = variable.Value
		}
	}
	is.Equal(appName, "Group Identity")
	before = updated
	pocketIdWaitForArchive(t, ctx, backupStore)
	_, err = InstallPocketId(ctx, input)
	is.NoErr(err)
	var repeated appsv1.Deployment
	is.NoErr(client.Get(ctx, key, &repeated))
	is.Equal(repeated.Generation, before.Generation)
	is.Equal(repeated.ResourceVersion, before.ResourceVersion)

	zero := int32(0)
	repeated.Spec.Replicas = &zero
	is.NoErr(client.Update(ctx, &repeated))
	is.NoErr(connection.Deployments().WaitForRollout(ctx, namespace, endpoint.ServiceName))
	for {
		var pods corev1.PodList
		is.NoErr(client.List(ctx, &pods, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{"borq.io/pocket-id": identity}))
		if len(pods.Items) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Pocket ID's old pod did not stop before the persistence check")
		case <-ticker.C:
		}
	}
	restoredURL := pocketIdRestoreArchive(t, ctx, client, configuration.Host, repeated)
	pocketIdVerifyRestoredIdentity(t, ctx, transport, restoredURL, input.ApiKey, admin, oidcClient)
	_, err = InstallPocketId(ctx, input)
	is.NoErr(err)
	var retained corev1.PersistentVolumeClaim
	is.NoErr(client.Get(ctx, key, &retained))
	is.Equal(retained.UID, claim.UID)

	// A plain GET cannot recreate the administrator; it proves the real database
	// reopened after its only pod stopped and still contains the original account.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/users/"+admin.ID, nil)
	is.NoErr(err)
	request.Header.Set("X-API-KEY", input.ApiKey)
	response, err := transport.Do(request)
	is.NoErr(err)
	defer response.Body.Close()
	is.Equal(response.StatusCode, http.StatusOK)
	var account struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		IsAdmin  bool   `json:"isAdmin"`
	}
	is.NoErr(json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&account))
	is.Equal(account.ID, admin.ID)
	is.Equal(account.Username, admin.Username)
	is.True(account.IsAdmin)
}

func pocketIdBoundaryProbe() string {
	return `set -eu
request() {
    # BusyBox nc exits on stdin EOF; keep the writer alive for the response.
    { printf '%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nContent-Length: 0\r\n\r\n' "$1" "$2"; sleep 1; } |
        nc -w 3 "$POCKET_HOST" 1411 | head -n 1 | tr -d '\r'
}
health="$(request GET /healthz)"
if [ "$health" != 'HTTP/1.1 204 No Content' ]; then
    printf 'Expected the proxy health endpoint to be reachable, got: %s\n' "$health" >&2
    exit 1
fi
for method in GET POST; do
    for path in /api/signup/setup /api/signup/setup/ /api/signup%2fsetup /api/signup/%2e/setup /api//signup/setup /api/signup/./setup /api/signup/other/../setup /api/%73ignup/%73etup /api/signup%252fsetup; do
        result="$(request "$method" "$path")"
        if [ "$result" != 'HTTP/1.1 404 Not Found' ]; then
            printf 'Setup route was not blocked: %s %s returned %s\n' "$method" "$path" "$result" >&2
            exit 1
        fi
    done
done
if nc -z -w 2 "$POCKET_HOST" 1412; then
    printf 'Pocket ID direct listener is reachable from another pod\n' >&2
    exit 1
fi
`
}
