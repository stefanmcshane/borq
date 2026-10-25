//go:build integration

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeclient "k8s.io/client-go/kubernetes"
)

// TestSharedS3ProjectionIntegration proves real ESO isolates credential generations
// and preparation cannot accept the old Secret while replacement credentials are absent.
func TestSharedS3ProjectionIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BORQ_MANAGEMENT_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("BORQ_MANAGEMENT_KUBECONFIG is required")
	}
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfig}, logger.Logger{})
	assert.NoErr(err)
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	assert.NoErr(err)
	vault, err := OpenManagementVault(ctx, connection, "borq")
	assert.NoErr(err)
	admin, err := connection.Secrets().Get(ctx, "borq", "borq-openbao-admin")
	assert.NoErr(err)
	settings := Settings{Namespace: "borq", Profile: "production", Timeout: 90 * time.Second, S3: S3Settings{Bucket: "projection-test"}}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	names := make([]string, 0, 2)
	for index, generation := range []string{"original", "replacement"} {
		ref := "test/s3-projection/" + suffix + "/" + generation
		settings.S3.CredentialsRef = ref
		values, err := renderComponentValues(component{Name: "loki", Values: "loki.yaml"}, valuesInput{Settings: settings})
		assert.NoErr(err)
		env := values["singleBinary"].(map[string]any)["extraEnvFrom"].([]any)[0].(map[string]any)
		name := env["secretRef"].(map[string]any)["name"].(string)
		if index > 0 {
			assert.True(name != names[0])
		}
		names = append(names, name)
		binding := externalSecret(name, settings.Namespace, "borq-management-openbao", ref)
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			assert.NoErr(connection.Objects().Delete(cleanup, binding))
			// Delete only this test's private KV document, including all its versions.
			err := client.CoreV1().RESTClient().Delete().Namespace(settings.Namespace).
				Resource("services").Name("http:openbao:8200").SubResource("proxy").
				Suffix("v1/secret/metadata", ref).SetHeader("X-Vault-Token", admin.Data["token"]).Do(cleanup).Error()
			assert.NoErr(err)
		})
		if index > 0 {
			missing, stop := context.WithTimeout(ctx, 2*time.Second)
			err := prepareSharedS3(missing, connection, settings)
			stop()
			assert.True(errors.Is(err, context.DeadlineExceeded))
			assert.True(strings.Contains(err.Error(), "wait for component Secret borq/"+name))
		}
		credentials := map[string]string{"accessKey": generation + "-access", "secretKey": generation + "-secret", "sessionToken": generation + "-session"}
		_, err = vault.WriteKv(ctx, "secret", ref, openbao.WriteKvInput{Values: credentials})
		assert.NoErr(err)
		assert.NoErr(prepareSharedS3(ctx, connection, settings))
		projected, err := connection.Secrets().Get(ctx, settings.Namespace, name)
		assert.NoErr(err)
		assert.Equal(projected.Data, credentials)
		live, err := connection.Objects().Get(ctx, binding)
		assert.NoErr(err)
		target, found, err := unstructured.NestedString(live.(*unstructured.Unstructured).Object, "spec", "target", "name")
		assert.NoErr(err)
		assert.True(found)
		assert.Equal(target, name)
	}
	original, err := connection.Secrets().Get(ctx, settings.Namespace, names[0])
	assert.NoErr(err)
	assert.Equal(original.Data["accessKey"], "original-access")
	assert.Equal(original.Data["secretKey"], "original-secret")
}
