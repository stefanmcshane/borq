package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CredentialSource identifies the OpenBao static role read by External Secrets.
type CredentialSource struct {
	// Namespace contains the existing ESO authentication Secret and credential producers.
	Namespace string
	// Host is the optional private management hostname added to the server certificate.
	Host string
	// Mount selects the registered target's database secrets engine.
	Mount string
	// Role identifies this database incarnation's application static role.
	Role string
	// Provider supplies the target's Vault-compatible authentication and TLS configuration.
	Provider map[string]any
}

// Environment retrieves the current ESO-projected application connection variables.
func (client Client) Environment(ctx context.Context, input Input) (map[string]string, error) {
	secret, err := client.kube.CoreV1().Secrets(credentialNamespace(input)).Get(ctx, input.Name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return nil, errors.New("PostgreSQL application credential is unavailable")
	}
	if secret.Labels["borq.run/postgres-id"] != input.Owner {
		return nil, errors.New("PostgreSQL application credential ownership conflict")
	}
	username, password := string(secret.Data["username"]), string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, errors.New("PostgreSQL application credential is incomplete")
	}
	return environmentValues(input, username, password), nil
}

func environmentValues(input Input, username, password string) map[string]string {
	host := input.Name + "-rw." + input.Namespace + ".svc"
	user := strings.ReplaceAll(url.QueryEscape(username), "+", "%20")
	credential := strings.ReplaceAll(url.QueryEscape(password), "+", "%20")
	address := "postgresql://" + user + ":" + credential + "@" + host + ":5432/" + url.PathEscape(input.Database)
	return map[string]string{"PGHOST": host, "PGPORT": "5432", "PGDATABASE": input.Database, "PGUSER": username, "PGPASSWORD": password, "DATABASE_URL": address}
}

func environmentManifest(input Input) map[string]any {
	return object("external-secrets.io/v1alpha1", "PushSecret", input.Name+"-environment", credentialNamespace(input), input.Owner, map[string]any{
		"refreshInterval": "15s", "updatePolicy": "Replace", "deletionPolicy": "None",
		"secretStoreRefs": []any{map[string]any{"name": "borq-openbao", "kind": "ClusterSecretStore"}},
		"selector":        map[string]any{"secret": map[string]any{"name": input.Name + "-credentials"}},
		"data":            []any{map[string]any{"conversionStrategy": "None", "match": map[string]any{"secretKey": "environment.json", "remoteRef": map[string]any{"remoteKey": input.EnvironmentExport.VaultPath, "property": "values"}}}},
	})
}

func credentialManifests(input Input) []map[string]any {
	name := input.Name + "-credentials"
	generator := object("generators.external-secrets.io/v1alpha1", "VaultDynamicSecret", name, credentialNamespace(input), input.Owner, map[string]any{
		"controller": "borq-system",
		"path":       input.Credentials.Mount + "/static-creds/" + input.Credentials.Role,
		"method":     "GET", "resultType": "Data", "provider": maps.Clone(input.Credentials.Provider),
	})
	host := input.Name + "-rw." + input.Namespace + ".svc"
	address := "postgresql://%s:%s@" + host + ":5432/" + url.PathEscape(input.Database)
	connection := fmt.Sprintf(`(printf %q (.username | urlquery | replace "+" "%%20") (.password | urlquery | replace "+" "%%20"))`, address)
	values := fmt.Sprintf(`{{ dict "PGHOST" %q "PGPORT" "5432" "PGDATABASE" %q "PGUSER" .username "PGPASSWORD" .password "DATABASE_URL" %s | toJson }}`, host, input.Database, connection)
	external := object("external-secrets.io/v1", "ExternalSecret", name, credentialNamespace(input), input.Owner, map[string]any{
		"refreshPolicy": "Periodic", "refreshInterval": "15s",
		"dataFrom": []any{map[string]any{"sourceRef": map[string]any{"generatorRef": map[string]any{"apiVersion": "generators.external-secrets.io/v1alpha1", "kind": "VaultDynamicSecret", "name": name}}}},
		"target": map[string]any{"name": name, "creationPolicy": "Owner", "deletionPolicy": "Retain", "immutable": false,
			"template": map[string]any{"engineVersion": "v2", "type": "kubernetes.io/basic-auth", "metadata": map[string]any{"labels": map[string]any{"borq.run/postgres-id": input.Owner}, "annotations": map[string]any{"borq.run/credential-rotation": "{{ .last_vault_rotation }}"}},
				"data": map[string]any{"username": "{{ .username }}", "password": "{{ .password }}", "environment.json": values}}},
	})
	return []map[string]any{generator, external, environmentManifest(input)}
}

// ReconcileEnvironment applies ESO-owned credential projections without writing their Secret data.
func (client Client) ReconcileEnvironment(ctx context.Context, input Input) error {
	if input.Isolated || len(input.Credentials.Provider) == 0 {
		return nil
	}
	if input.Credentials.Namespace == "" || input.Credentials.Mount == "" || input.Credentials.Role == "" || len(input.Credentials.Provider) == 0 || input.EnvironmentExport.Name == "" || input.EnvironmentExport.VaultPath == "" {
		return errors.New("PostgreSQL rotating credential source and environment export are required")
	}
	for _, part := range []string{input.Credentials.Mount, input.Credentials.Role} {
		if strings.ContainsAny(part, "/\\?#%") || part == "." || part == ".." {
			return errors.New("PostgreSQL credential engine and role must be path segments")
		}
	}
	for _, manifest := range credentialManifests(input) {
		if manifest["kind"] == "PushSecret" {
			secret, err := client.kube.CoreV1().Secrets(credentialNamespace(input)).Get(ctx, input.Name+"-credentials", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if secret.Labels["borq.run/postgres-id"] != input.Owner {
				return errors.New("PostgreSQL application credential ownership conflict")
			}
			if secret.DeletionTimestamp != nil || len(secret.Data["username"]) == 0 || len(secret.Data["password"]) == 0 || len(secret.Data["environment.json"]) == 0 {
				return nil
			}
		}
		if err := client.apply(ctx, manifest, input.Owner); err != nil {
			return err
		}
	}
	return nil
}

func credentialNamespace(input Input) string {
	if input.Credentials.Namespace != "" {
		return input.Credentials.Namespace
	}
	return input.Namespace
}

// EnvironmentExportReady observes the live owned producer and credential synchronizer.
func (client Client) EnvironmentExportReady(ctx context.Context, input Input) (bool, error) {
	if input.Isolated || input.Credentials.Mount == "" || input.EnvironmentExport.Name == "" {
		return false, errors.New("PostgreSQL environment export is not configured")
	}
	for _, desired := range credentialManifests(input) {
		kind := desired["kind"].(string)
		metadata := desired["metadata"].(map[string]any)
		live, err := client.dynamic.Resource(resource(kind)).Namespace(credentialNamespace(input)).Get(ctx, metadata["name"].(string), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if live.GetLabels()["borq.run/postgres-id"] != input.Owner {
			return false, errors.New("PostgreSQL environment producer ownership conflict")
		}
		if live.GetDeletionTimestamp() != nil || !projectionFieldsMatch(live.Object["spec"], desired["spec"]) {
			return false, nil
		}
		if kind == "VaultDynamicSecret" {
			continue
		}
		ready := false
		conditions, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			if condition["type"] == "Ready" && condition["status"] == "True" {
				ready = true
			}
		}
		if !ready {
			return false, nil
		}
	}
	_, err := client.Environment(ctx, input)
	return err == nil, err
}

func projectionFieldsMatch(actual, desired any) bool {
	switch expected := desired.(type) {
	case map[string]any:
		current, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range expected {
			if !projectionFieldsMatch(current[key], value) {
				return false
			}
		}
		return true
	case []any:
		current, ok := actual.([]any)
		if !ok || len(current) != len(expected) {
			return false
		}
		for index, value := range expected {
			if !projectionFieldsMatch(current[index], value) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, desired)
	}
}
