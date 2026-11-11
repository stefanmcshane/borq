//go:build integration

package appserver_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func postgresLiveSecret(t *testing.T, plan appapi.Plan, app string) string {
	t.Helper()
	for _, manifest := range plan.Manifests {
		resource := unstructured.Unstructured{Object: manifest}
		if resource.GetKind() != "ExternalSecret" || resource.GetLabels()["app.kubernetes.io/name"] != app {
			continue
		}
		name, _, err := unstructured.NestedString(resource.Object, "spec", "target", "name")
		if err != nil || name == "" {
			t.Fatal("managed database consumer Secret name is missing")
		}
		return name
	}
	t.Fatal("managed database application has no live ExternalSecret")
	return ""
}

// Kubernetes clients own their configured transport through pointer receivers.
func (run postgresAcceptance) consumer(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, name, secretName string) postgresConsumer {
	t.Helper()
	secret, err := kube.CoreV1().Secrets(run.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := kube.CoreV1().Pods(run.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + name})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := kube.BatchV1().Jobs(run.namespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/phase=predeploy"})
	if err != nil {
		t.Fatal(err)
	}
	result := postgresConsumer{name: name, secret: secretName, password: string(secret.Data["PGPASSWORD"])}
	for _, job := range jobs.Items {
		result.jobs = append(result.jobs, string(job.UID))
	}
	slices.Sort(result.jobs)
	for _, pod := range pods.Items {
		if postgresConsumerReady(pod) {
			result.pod = pod
			return result
		}
	}
	t.Fatal("managed database application has no ready consumer")
	return result
}

func postgresConsumerReady(pod corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// Rotation crosses OpenBao's real scheduler, ESO delivery, the Borq consumer
// reconciliation loop, and a new PostgreSQL connection from the replacement app.
func (run postgresAcceptance) rotation(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, database appapi.Postgres, before postgresConsumer, manual bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	vault, err := platform.ManagementVault(ctx, run.config.kubeconfig, run.config.managementNamespace)
	if err != nil {
		t.Fatal(err)
	}
	mount, role := "database-"+run.clusterID, "pg-"+database.ID
	if manual {
		t.Logf("manually rotate %s and wait for the running application to reconnect", database.Name)
		if err := vault.RotateStaticRole(ctx, mount, role); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("wait for automatic OpenBao rotation of %s and application credential refresh", database.Name)
		role, err := vault.ReadStaticRole(ctx, mount, role)
		if err != nil || role.RotationPeriod != time.Minute {
			t.Fatal("automatic acceptance fixture must use a one minute rotation period")
		}
	}
	for {
		credentials, err := vault.ReadStaticCredentials(ctx, mount, role)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := kube.CoreV1().Secrets(run.namespace).Get(ctx, before.secret, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if credentials.Password != before.password && string(secret.Data["PGPASSWORD"]) == credentials.Password {
			pods, err := kube.CoreV1().Pods(run.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + before.name})
			if err != nil {
				t.Fatal(err)
			}
			for _, pod := range pods.Items {
				if pod.UID == before.pod.UID || !postgresConsumerReady(pod) {
					continue
				}
				output, _, connectionErr := run.sqlAttempt(ctx, pod.Name, "SELECT count(*) FROM recovery.accounts;", "")
				if connectionErr != nil || output != "257\n" {
					continue
				}
				_, diagnostic, obsoleteErr := run.sqlAttempt(ctx, pod.Name, "SELECT 1;", before.password)
				if obsoleteErr == nil || !strings.Contains(diagnostic, "password authentication failed") {
					t.Fatal("obsolete application password was not rejected at PostgreSQL authentication")
				}
				current := run.consumer(t, ctx, kube, before.name, before.secret)
				if !slices.Equal(current.jobs, before.jobs) {
					t.Fatal("credential rotation created or replaced a predeploy Job")
				}
				_, err := kube.CoreV1().Secrets(run.namespace).Get(ctx, database.ClusterName+"-app", metav1.GetOptions{})
				if !apierrors.IsNotFound(err) {
					t.Fatal("database bootstrap credential Secret survived the OpenBao handoff")
				}
				t.Log("replacement application reconnects with rotated credentials; obsolete password is rejected; predeploy Jobs remain unchanged")
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("rotated credentials did not reach a replacement application with a working database connection")
		case <-time.After(2 * time.Second):
		}
	}
}

func (run postgresAcceptance) restoreCredentialIsolation(t *testing.T, ctx context.Context, source, restored appapi.Postgres) {
	t.Helper()
	vault, err := platform.ManagementVault(ctx, run.config.kubeconfig, run.config.managementNamespace)
	if err != nil {
		t.Fatal(err)
	}
	mount := "database-" + run.clusterID
	sourceConnection, err := vault.ReadDatabaseConnection(ctx, mount, "pg-"+source.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetConnection, err := vault.ReadDatabaseConnection(ctx, mount, "pg-"+restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceConnection.ConnectionUrl == targetConnection.ConnectionUrl || sourceConnection.AllowedRole == targetConnection.AllowedRole {
		t.Fatal("restore reused the source OpenBao connection or static role")
	}
	sourceCredential, err := vault.ReadStaticCredentials(ctx, mount, "pg-"+source.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetCredential, err := vault.ReadStaticCredentials(ctx, mount, "pg-"+restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCredential.Password == targetCredential.Password {
		t.Fatal("restore reused the source application password")
	}
	_, diagnostic, err := run.sqlAttempt(ctx, "recovery-client-restored", "SELECT 1;", sourceCredential.Password)
	if err == nil || !strings.Contains(diagnostic, "password authentication failed") {
		t.Fatal("source credential was not rejected by the restored database authentication guard")
	}
	t.Log("restored database has an independent OpenBao connection, static role, and application password")
}

// SDK clients retain connection state through pointer receivers.
func (run postgresAcceptance) credentialsRemoved(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, database appapi.Postgres) {
	t.Helper()
	vault, err := platform.ManagementVault(ctx, run.config.kubeconfig, run.config.managementNamespace)
	if err != nil {
		t.Fatal(err)
	}
	mount, role := "database-"+run.clusterID, "pg-"+database.ID
	if _, err := vault.ReadStaticRole(ctx, mount, role); !errors.Is(err, openbao.ErrNotFound) {
		t.Fatal("removed database retains an OpenBao application credential role")
	}
	if _, err := vault.ReadDatabaseConnection(ctx, mount, role); !errors.Is(err, openbao.ErrNotFound) {
		t.Fatal("removed database retains an OpenBao management connection")
	}
	configuration, err := clientcmd.BuildConfigFromFlags("", run.config.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := dynamic.NewForConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []schema.GroupVersionResource{
		{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"},
		{Group: "generators.external-secrets.io", Version: "v1alpha1", Resource: "vaultdynamicsecrets"},
	} {
		_, err := resources.Resource(resource).Namespace(platform.SystemNamespace).Get(ctx, database.ClusterName+"-credentials", metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatal("removed database retains a credential projection controller")
		}
	}
	_, err = kube.CoreV1().Secrets(platform.SystemNamespace).Get(ctx, database.ClusterName+"-credentials", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("removed database retains its projected application password")
	}
}
