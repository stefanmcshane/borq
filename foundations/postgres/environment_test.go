package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/matryer/is"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestEnvironmentProjection verifies the ESO wire contract, URL escaping, and exclusive controller ownership of credential data.
func TestEnvironmentProjection(t *testing.T) {
	check := is.New(t)
	input := Input{Namespace: "space", Name: "pg-destination", Owner: "database-id", Database: "orders", Credentials: CredentialSource{Namespace: "borq", Mount: "database-target", Role: "pg-database-id", Provider: map[string]any{"server": "https://bao.example", "auth": map[string]any{"tokenSecretRef": map[string]any{"name": "borq-openbao", "key": "token"}}}}, EnvironmentExport: EnvironmentExport{Name: "postgres-orders-database-id", VaultPath: "borq/environment-groups/space-id/postgres-orders-database-id"}}
	password := "p@ss:/?#% $&+,;=ü"
	sourceAvailable := false
	source := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: input.Name + "-credentials", Namespace: "borq", ResourceVersion: "7", Labels: map[string]string{"borq.run/postgres-id": input.Owner}}, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{"username": []byte("orders"), "password": []byte(password)}}
	written := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/secrets/pg-destination-credentials") {
			check.Equal(request.Method, http.MethodGet)
			check.True(strings.Contains(request.URL.Path, "/namespaces/borq/"))
			if !sourceAvailable {
				http.Error(writer, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`, http.StatusNotFound)
				return
			}
			check.NoErr(json.NewEncoder(writer).Encode(source))
			return
		}
		check.True(strings.Contains(request.URL.Path, "/namespaces/borq/"))
		if request.Method == http.MethodPatch {
			var manifest map[string]any
			check.NoErr(json.NewDecoder(request.Body).Decode(&manifest))
			written[manifest["kind"].(string)] = manifest
			check.NoErr(json.NewEncoder(writer).Encode(manifest))
			return
		}
		http.Error(writer, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`, http.StatusNotFound)
	}))
	defer server.Close()
	config := rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	client := Client{kube: kube.NewForConfigOrDie(&config), dynamic: dynamic.NewForConfigOrDie(&config)}
	check.NoErr(client.ReconcileEnvironment(t.Context(), input))
	if len(written) != 2 || written["PushSecret"] != nil {
		t.Fatal("environment exporter started before its source credential Secret existed")
	}
	sourceAvailable = true
	source.Data["environment.json"] = []byte(`{}`)
	check.NoErr(client.ReconcileEnvironment(t.Context(), input))
	check.Equal(len(written), 3)
	generator := written["VaultDynamicSecret"]
	check.Equal(generator["spec"].(map[string]any)["path"], "database-target/static-creds/pg-database-id")
	external := written["ExternalSecret"]
	target := external["spec"].(map[string]any)["target"].(map[string]any)
	check.Equal(target["immutable"], false)
	check.Equal(external["spec"].(map[string]any)["refreshInterval"], "15s")
	secretTemplate := target["template"].(map[string]any)
	data := secretTemplate["data"].(map[string]any)
	engineValues := map[string]string{"username": "orders", "password": password, "last_vault_rotation": "2026-10-24T12:00:00Z", "rotation_period": "86400", "ttl": "10"}
	rendered := &strings.Builder{}
	environmentTemplate, err := template.New("environment").Option("missingkey=error").Funcs(sprig.TxtFuncMap()).Parse(data["environment.json"].(string))
	check.NoErr(err)
	check.NoErr(environmentTemplate.Execute(rendered, engineValues))
	var values map[string]string
	check.NoErr(json.Unmarshal([]byte(rendered.String()), &values))
	check.Equal(values, environmentValues(input, "orders", password))
	address, err := url.Parse(values["DATABASE_URL"])
	check.NoErr(err)
	decodedPassword, present := address.User.Password()
	check.True(present)
	check.Equal(decodedPassword, password)
	check.Equal(address.User.Username(), "orders")
	annotations := secretTemplate["metadata"].(map[string]any)["annotations"].(map[string]any)
	rotationTemplate, err := template.New("rotation").Option("missingkey=error").Parse(annotations["borq.run/credential-rotation"].(string))
	check.NoErr(err)
	rendered.Reset()
	check.NoErr(rotationTemplate.Execute(rendered, engineValues))
	check.Equal(rendered.String(), "2026-10-24T12:00:00Z")
	check.Equal(written["PushSecret"]["spec"].(map[string]any)["selector"], map[string]any{"secret": map[string]any{"name": "pg-destination-credentials"}})
	actual, err := client.Environment(t.Context(), input)
	check.NoErr(err)
	check.Equal(actual, values)
	source.Labels["borq.run/postgres-id"] = "foreign-owner"
	_, err = client.Environment(t.Context(), input)
	check.True(err != nil && strings.Contains(err.Error(), "ownership conflict"))
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
