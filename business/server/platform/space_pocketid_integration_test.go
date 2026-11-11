//go:build integration

package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestSpacePocketIdIntegration proves SQLite survives a real ordered Pod rollover
// and remove/reinstall, including its admin, OIDC client and signing keys. A Pod
// watch observes every add/delete event to reject overlapping database writers.
// It creates only an isolated namespace on the selected cluster.
func TestSpacePocketIdIntegration(t *testing.T) {
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("BORQ_KUBECONFIG is required")
	}
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	is.NoErr(err)
	configuration := connection.RestConfig()
	client, err := ctrlclient.NewWithWatch(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme()})
	is.NoErr(err)
	namespace := fmt.Sprintf("borq-space-pocketid-%d", time.Now().UnixNano())
	is.NoErr(client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Errorf("remove isolated Pocket ID namespace: %v", err)
		}
	})
	input := SpacePocketIdInput{Connection: connection, Namespace: namespace, ID: strings.ReplaceAll(uuid.NewString(), "-", ""), IssuerURL: "https://" + namespace + ".example.test", EncryptionKey: rand.Text() + rand.Text(), ApiKey: rand.Text() + rand.Text(), Timeout: 2 * time.Minute}
	archive, err := sqliteworkload.NewArchiveKey()
	is.NoErr(err)
	backupStore, backupCredentials := pocketIdBackupFixture(t, ctx, kubeconfigPath, connection, client, namespace, input.ID)
	input.ArchiveKey, input.BackupCredentials = archive, backupCredentials
	input.BackupDestination = sqliteworkload.Destination{Endpoint: "http://rustfs-svc." + namespace + ".svc:9000", Region: backupStore.Region, Bucket: backupStore.Bucket, Prefix: backupStore.Prefix, PathStyle: true, SyncInterval: "1s"}
	endpoint, err := InstallSpacePocketId(ctx, input)
	is.NoErr(err)
	key := ctrlclient.ObjectKey{Namespace: namespace, Name: endpoint.ServiceName}
	var secret corev1.Secret
	var claim corev1.PersistentVolumeClaim
	var workload appsv1.StatefulSet
	is.NoErr(client.Get(ctx, key, &secret))
	is.NoErr(client.Get(ctx, key, &claim))
	is.NoErr(client.Get(ctx, key, &workload))
	is.Equal(claim.Status.Phase, corev1.ClaimBound)
	is.Equal(secret.Annotations["borq.io/pocket-id-pvc-uid"], string(claim.UID))
	is.Equal(claim.Annotations["borq.io/pocket-id-secret-uid"], string(secret.UID))
	transport, err := rest.HTTPClientFor(&configuration)
	is.NoErr(err)
	baseURL := strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:" + endpoint.ServiceName + ":1411/proxy"
	provider, err := pocketid.New(pocketid.Config{BaseURL: baseURL, ApiKey: input.ApiKey, HttpClient: transport})
	is.NoErr(err)
	admin := pocketid.Admin{ID: uuid.NewString(), Username: "space-owner", Email: "owner@example.test"}
	is.NoErr(provider.EnsureAdmin(ctx, admin))
	oidcClient := pocketid.OidcClient{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), Name: "Retained client", CallbackURL: "https://client.example.test/callback", LogoutCallbackURL: "https://client.example.test/logout", LaunchURL: "https://client.example.test", Secret: rand.Text() + rand.Text()}
	is.NoErr(provider.EnsureClient(ctx, oidcClient))
	read := func(path string) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		is.NoErr(err)
		response, err := transport.Do(request)
		is.NoErr(err)
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		is.NoErr(err)
		is.Equal(response.StatusCode, http.StatusOK)
		return body
	}
	signingKeys := read("/.well-known/jwks.json")
	setupRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/signup/setup", nil)
	is.NoErr(err)
	setupResponse, err := transport.Do(setupRequest)
	is.NoErr(err)
	setupResponse.Body.Close()
	is.Equal(setupResponse.StatusCode, http.StatusNotFound)

	var pods corev1.PodList
	selector := ctrlclient.MatchingLabels{"borq.io/space-pocket-id": input.ID}
	is.NoErr(client.List(ctx, &pods, ctrlclient.InNamespace(namespace), selector))
	is.Equal(len(pods.Items), 1)
	originalPodUid := pods.Items[0].UID
	stream, err := client.Watch(ctx, &corev1.PodList{}, ctrlclient.InNamespace(namespace), selector, &ctrlclient.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: pods.ResourceVersion}})
	is.NoErr(err)
	defer stream.Stop()
	// A template revision exercises the real StatefulSet rolling-update mechanism
	// without fetching an unrelated binary version or mutating tenant identity.
	workload.Spec.Template.Annotations = map[string]string{"borq.io/integration-rollover": uuid.NewString()}
	is.NoErr(client.Update(ctx, &workload))
	livePods := map[string]bool{string(originalPodUid): true}
	replacementReady := false
	for !replacementReady {
		select {
		case <-ctx.Done():
			t.Fatal("Pocket ID rollover did not finish")
		case event, open := <-stream.ResultChan():
			if !open {
				t.Fatal("Pod watch closed before rollover completed")
			}
			if event.Type == watch.Error {
				t.Fatalf("Pod watch failed: %v", event.Object)
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if event.Type == watch.Deleted {
				delete(livePods, string(pod.UID))
				continue
			}
			livePods[string(pod.UID)] = true
			if len(livePods) > 1 {
				t.Fatal("StatefulSet rollover started a second database writer before deleting the first")
			}
			replacementReady = pod.UID != originalPodUid && slices.ContainsFunc(pod.Status.Conditions, func(condition corev1.PodCondition) bool {
				return condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
			})
		}
	}
	stream.Stop()
	input.RequireExistingStorage = true
	_, err = InstallSpacePocketId(ctx, input)
	is.NoErr(err)
	pocketIdVerifyRestoredIdentity(t, ctx, transport, baseURL, input.ApiKey, admin, oidcClient)
	is.Equal(string(read("/.well-known/jwks.json")), string(signingKeys))
	pocketIdWaitForArchive(t, ctx, backupStore)
	remote, err := objectstore.New(backupStore)
	is.NoErr(err)
	objects, err := remote.List(ctx, "")
	is.NoErr(err)
	for _, object := range objects {
		if !strings.Contains(object.Key, "/snapshots/") && !strings.Contains(object.Key, "/wal/") {
			continue
		}
		ciphertext, err := remote.Read(ctx, object.Key)
		is.NoErr(err)
		is.True(bytes.HasPrefix(ciphertext, []byte("age-encryption.org/v1\n")))
	}
	is.NoErr(RemoveSpacePocketId(ctx, input))
	var retainedSecret corev1.Secret
	var retainedClaim corev1.PersistentVolumeClaim
	is.NoErr(client.Get(ctx, key, &retainedSecret))
	is.NoErr(client.Get(ctx, key, &retainedClaim))
	is.Equal(retainedSecret.UID, secret.UID)
	is.Equal(retainedClaim.UID, claim.UID)
	is.True(apierrors.IsNotFound(client.Get(ctx, key, &appsv1.StatefulSet{})))
	_, err = InstallSpacePocketId(ctx, input)
	is.NoErr(err)
	pocketIdVerifyRestoredIdentity(t, ctx, transport, baseURL, input.ApiKey, admin, oidcClient)
	is.Equal(string(read("/.well-known/jwks.json")), string(signingKeys))
}
