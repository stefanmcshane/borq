//go:build integration

package apps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/google/uuid"
	"github.com/kballard/go-shellquote"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestApplicationTunnelIntegration verifies real issuance, controller acceptance,
// HTTP forwarding and safe TLS retirement on a dedicated controller fixture.
func TestApplicationTunnelIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_TUNNEL_KUBECONFIG")
	originAddress := os.Getenv("BORQ_TUNNEL_ORIGIN_URL")
	if kubeconfig == "" || originAddress == "" {
		t.Skip("set BORQ_TUNNEL_KUBECONFIG and BORQ_TUNNEL_ORIGIN_URL to an isolated Traefik and cert-manager fixture")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	connection := Cluster{KubeconfigPath: kubeconfig}
	client, err := networkDnsClient(connection)
	assert.NoErr(err)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	namespace, target, issuerName := "tunnel-"+suffix, "target-"+suffix, "issuer-"+suffix
	assert.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	issuer := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: issuerName}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{SelfSigned: &certmanagerv1.SelfSignedIssuer{}}}}
	assert.NoErr(client.Create(ctx, &issuer))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		_ = client.Delete(cleanup, &issuer)
	})
	configuration, err := Merge(Configuration{}, patchFromJson(t, `{"apps":[{"name":"web","image":"nginx:1.27-alpine","port":80,"ingresses":[{"name":"public","host":"web.tunnel-test.invalid"}]}]}`))
	assert.NoErr(err)
	configuration.Apps[0].Command = shellquote.Join("/bin/sh", "-c", `printf '%s\n' 'server { listen 80; location / { return 200 "$http_x_forwarded_proto"; } }' > /etc/nginx/conf.d/default.conf; exec nginx -g 'daemon off;'`)
	secure, err := RenderForPlan(configuration, namespace, target, "tls", issuerName)
	assert.NoErr(err)
	applications, err := newClusterClient(connection)
	assert.NoErr(err)
	assert.NoErr(applications.Apply(ctx, secure.Manifests, target))
	waitApplicationTunnelResources(t, ctx, applications, secure.Manifests, target)
	certificateKey := ctrlclient.ObjectKey{Namespace: namespace, Name: "web-public-tls"}
	var certificate certmanagerv1.Certificate
	assert.NoErr(client.Get(ctx, certificateKey, &certificate))
	var secret corev1.Secret
	assert.NoErr(client.Get(ctx, certificateKey, &secret))
	assert.True(len(secret.Data[corev1.TLSCertKey]) > 0)
	// Exercise the real garbage collector as if cert-manager's optional
	// certificate owner references were enabled for the issued Secret.
	secret.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&certificate, certmanagerv1.SchemeGroupVersion.WithKind("Certificate"))}
	assert.NoErr(client.Update(ctx, &secret))
	secretUID := secret.UID
	databasePath := filepath.Join(t.TempDir(), "tunnel.sqlite")
	manager, err := NewManager(ctx, NewInput{DatabasePath: databasePath})
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	origin, err := renderWithEnv(configuration, namespace, target, "origin", issuerName, nil, ResourceScope{Organization: "acme", Space: "production", Cluster: "local", tunnelDomain: "tunnel-test.invalid"}, nil, ManagementS3{})
	assert.NoErr(err)
	retained, candidates := applicationRetiredCertificates(secure.Manifests, origin.Manifests)
	assert.NoErr(rejectResourceDeletions(retained, origin.Manifests))
	stored := spaceRecord{Space: Space{ID: target}, Manifests: secure.Manifests}
	retired, err := manager.captureApplicationCertificateRetirements(ctx, stored, connection, candidates, origin.Manifests)
	assert.NoErr(err)
	assert.Equal(len(retired), 1)
	assert.Equal(fmt.Sprint(retired[0]["metadata"].(map[string]any)["uid"]), string(certificate.UID))
	saved := planRecord{Plan: Plan{Space: stored.Space, Manifests: origin.Manifests}, Connection: connection, RetiredCertificates: retired}
	planWrite, err := documentWrite("plan", "pending", saved, 0)
	assert.NoErr(err)
	operationWrite, err := documentWrite("operation", "pending", Operation{ID: "pending", PlanID: "pending", SpaceID: target, Status: "failed"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{planWrite, operationWrite}))
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, NewInput{DatabasePath: databasePath})
	assert.NoErr(err)
	stored.LatestOperation = "pending"
	stored.Manifests = origin.Manifests
	resumed, err := manager.captureApplicationCertificateRetirements(ctx, stored, connection, nil, origin.Manifests)
	assert.NoErr(err)
	assert.Equal(resumed, retired)
	assert.True(manager.retireApplicationCertificates(ctx, saved) != nil)
	assert.NoErr(client.Get(ctx, certificateKey, &certificate))
	assert.NoErr(applications.Apply(ctx, origin.Manifests, target))
	waitApplicationTunnelResources(t, ctx, applications, origin.Manifests, target)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, originAddress, nil)
	assert.NoErr(err)
	request.Host = configuration.Apps[0].Ingresses[0].Host
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	assert.NoErr(err)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	assert.NoErr(err)
	assert.Equal(response.StatusCode, http.StatusOK)
	assert.Equal(string(body), "https")
	assert.NoErr(manager.retireApplicationCertificates(ctx, saved))
	for {
		err := client.Get(ctx, certificateKey, &certmanagerv1.Certificate{})
		if apierrors.IsNotFound(err) {
			break
		}
		assert.NoErr(err)
		select {
		case <-ctx.Done():
			t.Fatal("waiting for orphan certificate retirement:", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	assert.NoErr(client.Get(ctx, certificateKey, &secret))
	assert.Equal(secret.UID, secretUID)
	assert.Equal(len(secret.OwnerReferences), 0)
	assert.NoErr(manager.retireApplicationCertificates(ctx, saved))
	replacement := certificate.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "", ""
	replacement.Status = certmanagerv1.CertificateStatus{}
	assert.NoErr(client.Create(ctx, replacement))
	assert.True(replacement.UID != certificate.UID)
	assert.True(manager.retireApplicationCertificates(ctx, saved) != nil)
	assert.NoErr(client.Get(ctx, certificateKey, replacement))
	assert.NoErr(applications.Apply(ctx, secure.Manifests, target))
	waitApplicationTunnelResources(t, ctx, applications, secure.Manifests, target)
	assert.NoErr(client.Get(ctx, certificateKey, replacement))
	assert.NoErr(client.Get(ctx, certificateKey, &secret))
}

func waitApplicationTunnelResources(t *testing.T, ctx context.Context, client kubeapps.Client, manifests []map[string]any, target string) {
	t.Helper()
	for {
		status, err := client.Status(ctx, manifests, target)
		if err == nil && status.State == "succeeded" {
			return
		}
		if ctx.Err() != nil || status.State == "failed" {
			t.Fatalf("real application rollout: state=%s message=%s error=%v", status.State, status.Message, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("real application rollout: %s: %v", status.Message, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
