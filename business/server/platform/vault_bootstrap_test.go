package platform

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestVaultBootstrapDoesNotReplaceMaterial rejects partial or missing credentials
// without changing the Secret or creating a key for an existing data volume.
func TestVaultBootstrapDoesNotReplaceMaterial(t *testing.T) {
	for _, test := range []struct {
		name   string
		data   map[string][]byte
		volume bool
	}{
		{name: "partial secret", data: map[string][]byte{"token": []byte(strings.Repeat("a", 64))}},
		{name: "missing secret with data", volume: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			client := fake.NewClientset()
			if test.volume {
				_, err := client.CoreV1().PersistentVolumeClaims("borq").Create(t.Context(), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-openbao-0"}}, metav1.CreateOptions{})
				assert.NoErr(err)
			} else {
				_, err := client.CoreV1().Secrets("borq").Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-openbao-admin", Labels: managedLabels()}, Data: test.data}, metav1.CreateOptions{})
				assert.NoErr(err)
			}
			err := ensureAdminSecret(t.Context(), kubernetes.NewConnectionForTest(client), "borq")
			assert.True(err != nil)
			if test.volume {
				assert.True(strings.Contains(err.Error(), "storage exists"))
			} else {
				assert.True(strings.Contains(err.Error(), "incomplete or invalid"))
			}
			for _, action := range client.Actions() {
				assert.True(action.GetVerb() != "update" && action.GetVerb() != "patch")
			}
			secrets, err := client.CoreV1().Secrets("borq").List(t.Context(), metav1.ListOptions{})
			assert.NoErr(err)
			if test.volume {
				assert.Equal(len(secrets.Items), 0)
			} else {
				assert.Equal(secrets.Items[0].Data, test.data)
			}
		})
	}
}

// TestVaultBootstrapConcurrentCreate proves competing retries retain one complete
// credential set; OpenBao lifecycle success is covered by real cluster tests.
func TestVaultBootstrapConcurrentCreate(t *testing.T) {
	assert := is.New(t)
	connection := kubernetes.NewConnectionForTest(fake.NewClientset())
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		workers.Go(func() { results <- ensureAdminSecret(context.Background(), connection, "borq") })
	}
	workers.Wait()
	close(results)
	for err := range results {
		assert.NoErr(err)
	}
	first, err := connection.Secrets().Get(t.Context(), "borq", "borq-openbao-admin")
	assert.NoErr(err)
	transport, err := connection.Secrets().Get(t.Context(), "borq", "borq-openbao-kms-tls")
	assert.NoErr(err)
	assert.NoErr(ensureAdminSecret(t.Context(), connection, "borq"))
	after, err := connection.Secrets().Get(t.Context(), "borq", "borq-openbao-admin")
	assert.NoErr(err)
	assert.Equal(after, first)
	retainedTransport, err := connection.Secrets().Get(t.Context(), "borq", "borq-openbao-kms-tls")
	assert.NoErr(err)
	assert.Equal(retainedTransport, transport)
}

// TestVaultBootstrapRejectsForeignSecret prevents adoption of another controller's recovery credentials.
func TestVaultBootstrapRejectsForeignSecret(t *testing.T) {
	assert := is.New(t)
	client := fake.NewClientset()
	connection := kubernetes.NewConnectionForTest(client)
	assert.NoErr(ensureAdminSecret(t.Context(), connection, "borq"))
	secret, err := client.CoreV1().Secrets("borq").Get(t.Context(), "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	original := secret.DeepCopy()
	secret.Labels["app.kubernetes.io/managed-by"] = "another-controller"
	_, err = client.CoreV1().Secrets("borq").Update(t.Context(), secret, metav1.UpdateOptions{})
	assert.NoErr(err)
	err = ensureAdminSecret(t.Context(), connection, "borq")
	assert.True(err != nil && strings.Contains(err.Error(), "outside Borq ownership"))
	retained, err := client.CoreV1().Secrets("borq").Get(t.Context(), "borq-openbao-admin", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retained.Data, original.Data)
	assert.Equal(retained.Labels["app.kubernetes.io/managed-by"], "another-controller")
}

// TestVaultKmsTlsOwnership rejects foreign or malformed transport material instead of silently replacing another owner's certificate.
func TestVaultKmsTlsOwnership(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			check := is.New(t)
			client := fake.NewClientset()
			labels := managedLabels()
			if foreign {
				labels = map[string]string{"app.kubernetes.io/managed-by": "someone-else"}
			}
			_, err := client.CoreV1().Secrets("borq").Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-openbao-kms-tls", Namespace: "borq", Labels: labels}, Data: map[string][]byte{"tls.crt": []byte("invalid-certificate")}}, metav1.CreateOptions{})
			check.NoErr(err)
			err = ensureVaultKmsTlsSecret(t.Context(), kubernetes.NewConnectionForTest(client), "borq")
			check.True(err != nil)
			if foreign {
				check.True(strings.Contains(err.Error(), "not owned"))
			} else {
				check.True(strings.Contains(err.Error(), "invalid certificate"))
			}
			for _, action := range client.Actions() {
				check.True(action.GetVerb() != "update" && action.GetVerb() != "patch")
			}
		})
	}
}
