package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matryer/is"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestEnvironmentProjection proves the credential-to-provider wire shape without simulating a controller.
func TestEnvironmentProjection(t *testing.T) {
	check := is.New(t)
	input := Input{Namespace: "space", Name: "pg-destination", Owner: "database-id", Database: "orders", EnvironmentExport: EnvironmentExport{Name: "postgres-orders-database-id", VaultPath: "borq/environment-groups/space-id/postgres-orders-database-id"}}
	password := "p@ss:/?#% ü"
	source := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: input.Name + "-app", Namespace: input.Namespace, ResourceVersion: "7", Labels: map[string]string{"borq.run/postgres-id": input.Owner, "cnpg.io/reload": "true"}}, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{"username": []byte("orders"), "password": []byte(password), "preserved": []byte("extra")}}
	var producer map[string]any
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/secrets/pg-destination-app") {
			if request.Method == http.MethodPut {
				check.NoErr(json.NewDecoder(request.Body).Decode(&source))
				writes++
			}
			check.NoErr(json.NewEncoder(writer).Encode(source))
			return
		}
		if strings.HasSuffix(request.URL.Path, "/pushsecrets/pg-destination-environment") {
			if request.Method == http.MethodPatch {
				check.NoErr(json.NewDecoder(request.Body).Decode(&producer))
				check.NoErr(json.NewEncoder(writer).Encode(producer))
				return
			}
			http.Error(writer, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`, http.StatusNotFound)
			return
		}
		t.Errorf("unexpected Kubernetes request %s %s", request.Method, request.URL.Path)
		writer.WriteHeader(500)
	}))
	defer server.Close()
	config := rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	client := Client{kube: kube.NewForConfigOrDie(&config), dynamic: dynamic.NewForConfigOrDie(&config)}
	check.NoErr(client.ReconcileEnvironment(t.Context(), input))
	check.Equal(writes, 1)
	check.Equal(source.Type, corev1.SecretTypeBasicAuth)
	check.Equal(string(source.Data["preserved"]), "extra")
	check.Equal(source.ResourceVersion, "7")
	var values map[string]string
	check.NoErr(json.Unmarshal(source.Data["environment.json"], &values))
	check.Equal(len(values), 6)
	check.Equal(values["PGHOST"], "pg-destination-rw.space.svc")
	check.Equal(values["PGPORT"], "5432")
	check.Equal(values["PGDATABASE"], "orders")
	check.Equal(values["PGUSER"], "orders")
	check.Equal(values["PGPASSWORD"], password)
	address, err := url.Parse(values["DATABASE_URL"])
	check.NoErr(err)
	decoded, exists := address.User.Password()
	check.True(exists)
	check.Equal(decoded, password)
	check.Equal(address.Host, "pg-destination-rw.space.svc:5432")
	check.Equal(address.Path, "/orders")
	check.Equal(producer["apiVersion"], "external-secrets.io/v1alpha1")
	check.Equal(producer["metadata"].(map[string]any)["labels"].(map[string]any)["borq.run/postgres-id"], input.Owner)
	check.Equal(producer["spec"], map[string]any{
		"refreshInterval": "1m", "updatePolicy": "Replace", "deletionPolicy": "None",
		"secretStoreRefs": []any{map[string]any{"name": "borq-openbao", "kind": "ClusterSecretStore"}},
		"selector":        map[string]any{"secret": map[string]any{"name": "pg-destination-app"}},
		"data":            []any{map[string]any{"conversionStrategy": "None", "match": map[string]any{"secretKey": "environment.json", "remoteRef": map[string]any{"remoteKey": input.EnvironmentExport.VaultPath, "property": "values"}}}},
	})
	encoded, err := json.Marshal(producer)
	check.NoErr(err)
	check.True(!strings.Contains(string(encoded), password))
	check.NoErr(client.ReconcileEnvironment(t.Context(), input))
	check.Equal(writes, 1)
	source.Labels["borq.run/postgres-id"] = "foreign-owner"
	err = client.ReconcileEnvironment(t.Context(), input)
	check.True(err != nil && strings.Contains(err.Error(), "ownership conflict"))
	check.Equal(writes, 1)
	input.Isolated = true
	check.NoErr((Client{}).ReconcileEnvironment(context.Background(), input))
}

// TestEnvironmentProducerStopsBeforeRuntimeDeletion protects retained history from a producer racing teardown.
func TestEnvironmentProducerStopsBeforeRuntimeDeletion(t *testing.T) {
	check := is.New(t)
	input := Input{Namespace: "space", Name: "pg-destination", Owner: "database-id"}
	producer := unstructured.Unstructured{Object: environmentManifest(input)}
	producer.SetUID("producer-uid")
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch request.Method {
		case http.MethodGet:
			if strings.HasSuffix(request.URL.Path, "/pushsecrets") {
				check.NoErr(json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "external-secrets.io/v1alpha1", "kind": "PushSecretList", "items": []any{producer.Object}}))
				return
			}
			check.NoErr(json.NewEncoder(writer).Encode(producer.Object))
		case http.MethodDelete:
			var deletion metav1.DeleteOptions
			check.NoErr(json.NewDecoder(request.Body).Decode(&deletion))
			check.Equal(string(*deletion.Preconditions.UID), "producer-uid")
			check.NoErr(json.NewEncoder(writer).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Success"}))
		default:
			t.Errorf("unexpected request %s", request.Method)
			writer.WriteHeader(500)
		}
	}))
	defer server.Close()
	client := Client{dynamic: dynamic.NewForConfigOrDie(&rest.Config{Host: server.URL})}
	complete, err := client.DeleteOwned(t.Context(), input)
	check.NoErr(err)
	check.True(!complete)
	check.Equal(requests, []string{
		"GET /apis/external-secrets.io/v1alpha1/namespaces/space/pushsecrets",
		"GET /apis/external-secrets.io/v1alpha1/namespaces/space/pushsecrets/pg-destination-environment",
		"DELETE /apis/external-secrets.io/v1alpha1/namespaces/space/pushsecrets/pg-destination-environment",
	})

	requests = nil
	input.EnvironmentExport.Name = "postgres-orders-database-id"
	producer.SetLabels(map[string]string{"borq.run/postgres-id": "foreign-owner"})
	complete, err = client.DeleteOwned(t.Context(), input)
	check.True(err != nil && strings.Contains(err.Error(), "conflicting environment producer"))
	check.True(!complete)
	check.Equal(requests, []string{"GET /apis/external-secrets.io/v1alpha1/namespaces/space/pushsecrets/pg-destination-environment"})
}
