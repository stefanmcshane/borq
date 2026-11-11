package kubeapps

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// TestManagedIngressWaitsForCertificate verifies HTTPS readiness requires the
// current Certificate revision and valid, matching TLS material for the route.
func TestManagedIngressWaitsForCertificate(t *testing.T) {
	now := time.Now()
	valid := tlsSecret(t, "example.test", now.Add(-time.Hour), now.Add(time.Hour))
	expired := tlsSecret(t, "example.test", now.Add(-2*time.Hour), now.Add(-time.Hour))
	future := tlsSecret(t, "example.test", now.Add(time.Hour), now.Add(2*time.Hour))
	otherHost := tlsSecret(t, "other.test", now.Add(-time.Hour), now.Add(time.Hour))
	for _, test := range []struct {
		name   string
		change func(map[string]any, map[string]any, map[string]any)
		omit   string
		state  string
	}{
		{name: "ready", state: "succeeded"},
		{name: "certificate absent", omit: "Certificate", state: "running"},
		{name: "secret absent", omit: "Secret", state: "running"},
		{name: "no ready condition", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) { delete(certificate, "status") }},
		{name: "not ready", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			readyCertificateCondition(certificate)["status"] = "False"
		}},
		{name: "stale ready condition", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			readyCertificateCondition(certificate)["observedGeneration"] = int64(1)
		}},
		{name: "different secret", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			certificate["spec"].(map[string]any)["secretName"] = "other"
		}},
		{name: "different issuer", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			valueAt(certificate, "spec", "issuerRef").(map[string]any)["name"] = "other"
		}},
		{name: "namespace issuer", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			valueAt(certificate, "spec", "issuerRef").(map[string]any)["kind"] = "Issuer"
		}},
		{name: "different issuer group", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			valueAt(certificate, "spec", "issuerRef").(map[string]any)["group"] = "other.test"
		}},
		{name: "different certificate host", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			certificate["spec"].(map[string]any)["dnsNames"] = []any{"other.test"}
		}},
		{name: "extra certificate host", state: "running", change: func(_ map[string]any, certificate, _ map[string]any) {
			certificate["spec"].(map[string]any)["dnsNames"] = []any{"example.test", "other.test"}
		}},
		{name: "ingress issuer drift", state: "running", change: func(ingress, _, _ map[string]any) {
			valueAt(ingress, "metadata", "annotations").(map[string]any)["cert-manager.io/cluster-issuer"] = "other"
		}},
		{name: "ingress host drift", state: "running", change: func(ingress, _, _ map[string]any) {
			valueAt(ingress, "spec", "rules").([]any)[0].(map[string]any)["host"] = "other.test"
		}},
		{name: "wrong secret type", state: "running", change: func(_, _, secret map[string]any) { secret["type"] = "Opaque" }},
		{name: "certificate bytes missing", state: "running", change: func(_, _, secret map[string]any) { delete(secret["data"].(map[string]any), "tls.crt") }},
		{name: "invalid certificate encoding", state: "running", change: func(_, _, secret map[string]any) {
			secret["data"].(map[string]any)["tls.crt"] = "!private-data-do-not-return!"
		}},
		{name: "invalid private key encoding", state: "running", change: func(_, _, secret map[string]any) {
			secret["data"].(map[string]any)["tls.key"] = "!private-data-do-not-return!"
		}},
		{name: "invalid certificate pem", state: "running", change: func(_, _, secret map[string]any) {
			secret["data"].(map[string]any)["tls.crt"] = base64.StdEncoding.EncodeToString([]byte("private-data-do-not-return"))
		}},
		{name: "wrong private key", state: "running", change: func(_, _, secret map[string]any) {
			secret["data"].(map[string]any)["tls.key"] = valueAt(otherHost, "data", "tls.key")
		}},
		{name: "expired certificate", state: "running", change: func(_, _, secret map[string]any) { secret["data"] = expired["data"] }},
		{name: "future certificate", state: "running", change: func(_, _, secret map[string]any) { secret["data"] = future["data"] }},
		{name: "wrong certificate host", state: "running", change: func(_, _, secret map[string]any) { secret["data"] = otherHost["data"] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			desired := managedIngress()
			live, certificate, secret := copyMap(desired), readyCertificate(), copyMap(valid)
			if test.change != nil {
				test.change(live, certificate, secret)
			}
			resources := []map[string]any{live}
			if test.omit != "Certificate" {
				resources = append(resources, certificate)
			}
			if test.omit != "Secret" {
				resources = append(resources, secret)
			}
			client, fakeClient := testClient(resources...)
			status, err := client.Status(context.Background(), []map[string]any{desired}, "target-one")
			assert.NoErr(err)
			assert.Equal(status.State, test.state)
			assert.True(status.Resources[0].Message != "")
			encoded, err := json.Marshal(status)
			assert.NoErr(err)
			assert.True(!strings.Contains(string(encoded), "private-data-do-not-return"))
			assert.True(!strings.Contains(string(encoded), valueAt(secret, "data", "tls.key").(string)))
			for _, action := range fakeClient.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
}

// TestManagedIngressReadErrors verifies authorization and transient API failures
// are retried as observation errors instead of being mistaken for missing TLS.
func TestManagedIngressReadErrors(t *testing.T) {
	for _, resource := range []string{"certificates", "secrets"} {
		for _, failure := range []string{"forbidden", "unavailable"} {
			t.Run(resource+"/"+failure, func(t *testing.T) {
				assert := is.New(t)
				client, fakeClient := testClient(managedIngress(), readyCertificate())
				fakeClient.PrependReactor("get", resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
					if failure == "forbidden" {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "frontend-public-tls", errors.New("access denied"))
					}
					return true, nil, apierrors.NewServiceUnavailable("temporarily unavailable")
				})
				_, err := client.Status(context.Background(), []map[string]any{managedIngress()}, "target-one")
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), "read ingress"))
			})
		}
	}
}

// TestIngressWithoutManagedIssuerKeepsAdmissionReadiness preserves readiness for
// older saved manifests that do not ask cert-manager to manage their certificate.
func TestIngressWithoutManagedIssuerKeepsAdmissionReadiness(t *testing.T) {
	assert := is.New(t)
	ingress := managedIngress()
	delete(ingress["metadata"].(map[string]any), "annotations")
	client, fakeClient := testClient(ingress)
	status, err := client.Status(context.Background(), []map[string]any{ingress}, "target-one")
	assert.NoErr(err)
	assert.Equal(status.State, "succeeded")
	assert.Equal(len(fakeClient.Actions()), 1)
}

func managedIngress() map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"name": "frontend-public", "namespace": "shop", "labels": map[string]any{targetLabel: "target-one"},
			"annotations": map[string]any{"cert-manager.io/cluster-issuer": "borq"},
		},
		"spec": map[string]any{
			"tls":   []any{map[string]any{"hosts": []any{"example.test"}, "secretName": "frontend-public-tls"}},
			"rules": []any{map[string]any{"host": "example.test"}},
		},
	}
}

func readyCertificate() map[string]any {
	return map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "frontend-public-tls", "namespace": "shop", "generation": int64(2)},
		"spec": map[string]any{
			"secretName": "frontend-public-tls", "dnsNames": []any{"example.test"},
			"issuerRef": map[string]any{"name": "borq", "kind": "ClusterIssuer", "group": "cert-manager.io"},
		},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}},
	}
}

func readyCertificateCondition(certificate map[string]any) map[string]any {
	return valueAt(certificate, "status", "conditions").([]any)[0].(map[string]any)
}

func tlsSecret(t *testing.T, host string, notBefore, notAfter time.Time) map[string]any {
	t.Helper()
	assert := is.New(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assert.NoErr(err)
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host}, NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, &key.PublicKey, key)
	assert.NoErr(err)
	keyDer, err := x509.MarshalECPrivateKey(key)
	assert.NoErr(err)
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata": map[string]any{"name": "frontend-public-tls", "namespace": "shop"},
		"data": map[string]any{
			"tls.crt": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			"tls.key": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})),
		},
	}
}
