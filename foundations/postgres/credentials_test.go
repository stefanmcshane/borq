package postgres

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestRetireBootstrapCredentials owns API ownership and deletion ordering, without simulating CNPG reconciliation.
func TestRetireBootstrapCredentials(t *testing.T) {
	check := is.New(t)
	input := Input{Namespace: "space", Name: "pg-database", Owner: "database-id", Username: "app"}
	cluster := unstructured.Unstructured{Object: object("postgresql.cnpg.io/v1", "Cluster", input.Name, input.Namespace, input.Owner, map[string]any{
		"bootstrap": map[string]any{"initdb": map[string]any{"owner": "app", "database": "app", "secret": map[string]any{"name": "pg-database-app"}}},
		"managed":   map[string]any{"roles": []any{map[string]any{"name": "app", "passwordSecret": map[string]any{"name": "pg-database-app"}}, map[string]any{"name": "retained-role"}}},
	})}
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pg-database-app", Namespace: "space", UID: "bootstrap-uid", Labels: map[string]string{"borq.run/postgres-id": input.Owner}}}
	deleted, patched := false, false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/clusters/pg-database") {
			if request.Method == http.MethodPut {
				check.NoErr(json.NewDecoder(request.Body).Decode(&cluster))
				roles, _, err := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
				check.NoErr(err)
				check.Equal(roles, []any{map[string]any{"name": "retained-role"}})
				patched = true
			}
			check.NoErr(json.NewEncoder(writer).Encode(cluster.Object))
			return
		}
		check.True(strings.HasSuffix(request.URL.Path, "/secrets/pg-database-app"))
		if deleted {
			http.Error(writer, `{"apiVersion":"v1","kind":"Status","reason":"NotFound","code":404}`, http.StatusNotFound)
			return
		}
		if request.Method == http.MethodDelete {
			check.True(patched)
			var options metav1.DeleteOptions
			check.NoErr(json.NewDecoder(request.Body).Decode(&options))
			check.Equal(string(*options.Preconditions.UID), "bootstrap-uid")
			deleted = true
			check.NoErr(json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"}))
			return
		}
		check.NoErr(json.NewEncoder(writer).Encode(secret))
	}))
	defer server.Close()
	config := rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	client := Client{kube: kube.NewForConfigOrDie(&config), dynamic: dynamic.NewForConfigOrDie(&config)}
	check.NoErr(client.RetireBootstrapCredentials(t.Context(), input))
	check.True(deleted)
	check.NoErr(client.RetireBootstrapCredentials(t.Context(), input))
	cluster.SetLabels(map[string]string{"borq.run/postgres-id": "foreign-owner"})
	check.True(client.RetireBootstrapCredentials(t.Context(), input) != nil)
}

// TestReadServerCa returns only the public certificate and requires a CNPG owner reference matching the live Cluster UID.
func TestReadServerCa(t *testing.T) {
	check := is.New(t)
	input := Input{Namespace: "space", Name: "pg-database", Owner: "database-id"}
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	check.NoErr(err)
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CNPG test CA"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, &key.PublicKey, key)
	check.NoErr(err)
	public := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cluster := unstructured.Unstructured{Object: object("postgresql.cnpg.io/v1", "Cluster", input.Name, input.Namespace, input.Owner, nil)}
	cluster.SetUID("cluster-uid")
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pg-database-ca", Namespace: "space", OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: input.Name, UID: "cluster-uid"}}}, Data: map[string][]byte{"ca.crt": public, "ca.key": []byte("private-material")}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/clusters/pg-database") {
			check.NoErr(json.NewEncoder(writer).Encode(cluster.Object))
			return
		}
		check.True(strings.HasSuffix(request.URL.Path, "/secrets/pg-database-ca"))
		check.NoErr(json.NewEncoder(writer).Encode(secret))
	}))
	defer server.Close()
	config := rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	client := Client{kube: kube.NewForConfigOrDie(&config), dynamic: dynamic.NewForConfigOrDie(&config)}
	actual, err := client.ReadServerCa(t.Context(), input)
	check.NoErr(err)
	check.Equal(actual, public)
	check.True(!strings.Contains(string(actual), "private-material"))
	secret.OwnerReferences[0].UID = "another-cluster"
	_, err = client.ReadServerCa(t.Context(), input)
	check.True(err != nil && strings.Contains(err.Error(), "ownership conflict"))
}
