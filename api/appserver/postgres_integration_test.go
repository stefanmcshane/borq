//go:build integration

package appserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type postgresAcceptanceConfig struct {
	binary              string `env:"BORQ_MANAGED_ADDON_TEST_BINARY"`
	server              string `env:"BORQ_MANAGED_ADDON_TEST_SERVER_URL"`
	kubeconfig          string `env:"BORQ_MANAGEMENT_KUBECONFIG"`
	organization        string `env:"BORQ_MANAGED_ADDON_TEST_ORGANIZATION"`
	cluster             string `env:"BORQ_MANAGED_ADDON_TEST_CLUSTER"`
	managementNamespace string `env:"BORQ_MANAGED_ADDON_TEST_MANAGEMENT_NAMESPACE"`
}

type postgresAcceptance struct {
	config                         postgresAcceptanceConfig
	organization, space, namespace string
	clusterID                      string
}

type postgresConsumer struct {
	name, secret string
	pod          corev1.Pod
	password     string
	jobs         []string
}

type postgresTableFingerprint struct {
	Count int
	Rows  []string
	Hash  string
}

type postgresFingerprint struct {
	Tables    map[string]postgresTableFingerprint
	Schema    string
	Sequences string
}

// TestManagedPostgresIntegrationRecovery owns live credential rotation and
// physical backup recovery acceptance across the public YAML, CLI, API, OpenBao,
// ESO, CNPG, S3 and SQL boundaries. Logical row bytes survive physical recovery.
func TestManagedPostgresIntegrationRecovery(t *testing.T) {
	assert := is.New(t)
	config := postgresAcceptanceConfig{
		binary:              os.Getenv("BORQ_MANAGED_ADDON_TEST_BINARY"),
		server:              os.Getenv("BORQ_MANAGED_ADDON_TEST_SERVER_URL"),
		kubeconfig:          os.Getenv("BORQ_MANAGEMENT_KUBECONFIG"),
		organization:        os.Getenv("BORQ_MANAGED_ADDON_TEST_ORGANIZATION"),
		cluster:             os.Getenv("BORQ_MANAGED_ADDON_TEST_CLUSTER"),
		managementNamespace: os.Getenv("BORQ_MANAGED_ADDON_TEST_MANAGEMENT_NAMESPACE"),
	}
	if config.binary == "" || config.server == "" || config.kubeconfig == "" {
		t.Fatal("BORQ_MANAGED_ADDON_TEST_BINARY, BORQ_MANAGED_ADDON_TEST_SERVER_URL and BORQ_MANAGEMENT_KUBECONFIG are required")
	}
	if config.organization == "" {
		config.organization = "dev"
	}
	if config.cluster == "" {
		config.cluster = "management"
	}
	if config.managementNamespace == "" {
		config.managementNamespace = "borq"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	client, err := appapi.NewClientWithResponses(config.server, appapi.WithHTTPClient(&http.Client{Timeout: 2 * time.Minute}))
	assert.NoErr(err)
	restConfig, err := clientcmd.BuildConfigFromFlags("", config.kubeconfig)
	assert.NoErr(err)
	kube, err := kubernetes.NewForConfig(restConfig)
	assert.NoErr(err)
	run := postgresAcceptance{config: config, organization: config.organization, space: fmt.Sprintf("recovery-%x", time.Now().UnixNano())}
	// The isolated namespace is also removed if provisioning fails before an addon
	// becomes deletable. No pre-existing namespace or archive is touched.
	t.Cleanup(func() { run.cleanup(t, client, kube) })
	clusters, err := client.ListClustersWithResponse(ctx, run.organization)
	assert.NoErr(err)
	assertApplicationResponse(t, clusters.StatusCode(), clusters.Body, http.StatusOK)
	if clusters.JSON200 == nil || !slices.ContainsFunc(*clusters.JSON200, func(cluster appapi.Cluster) bool { return cluster.Name == config.cluster }) {
		t.Fatalf("organization %s has no registered cluster %s", run.organization, config.cluster)
	}
	for _, cluster := range *clusters.JSON200 {
		if cluster.Name == config.cluster && cluster.ID != nil {
			run.clusterID = *cluster.ID
		}
	}
	if run.clusterID == "" {
		t.Fatal("registered cluster has no credential engine identity")
	}
	grant, err := client.SetOrganizationManagedAddonsWithResponse(ctx, run.organization, appapi.OrganizationManagedAddonsInput{Enabled: true})
	assert.NoErr(err)
	assertApplicationResponse(t, grant.StatusCode(), grant.Body, http.StatusOK)
	space, err := client.PutSpaceWithResponse(ctx, run.organization, run.space, appapi.SpaceInput{Cluster: config.cluster})
	assert.NoErr(err)
	assertApplicationResponse(t, space.StatusCode(), space.Body, http.StatusOK)
	if space.JSON200 == nil {
		t.Fatal("space response missing namespace")
	}
	run.namespace = space.JSON200.Namespace

	t.Log("apply public database.yaml and retrieve source credentials through CLI")
	run.cli(t, ctx, "apply", "--file", filepath.Join("testdata", "database.yaml"), "--timeout", "12m")
	source := run.database(t, ctx, "source")
	run.namespace = source.Namespace
	assert.True(source.ClusterName != "")
	if source.Specification.Instances == nil || source.Specification.Storage == nil {
		t.Fatal("normalized source defaults missing")
	}
	assert.Equal(*source.Specification.Instances, 1)
	assert.Equal(*source.Specification.Storage, "1Gi")
	sourcePod := run.clientPod(t, ctx, kube, "source", source.Specification)
	seed, err := os.ReadFile(filepath.Join("testdata", "postgres_seed.sql"))
	assert.NoErr(err)
	run.sql(t, ctx, sourcePod, string(seed))
	expected := run.fingerprint(t, ctx, sourcePod)
	assert.Equal(expected.Tables["accounts"].Count, 257)
	assert.Equal(expected.Tables["entries"].Count, 771)
	assert.Equal(expected.Tables["duplicates"].Count, 6)
	assert.Equal(expected.Tables["empty_table"].Count, 0)
	assert.Equal(len(slices.Compact(slices.Clone(expected.Tables["duplicates"].Rows))), 3)
	assert.Equal(run.sql(t, ctx, sourcePod, "SELECT encode(payload, 'hex') FROM recovery.accounts WHERE id = 256;"), "010000ff80\n")
	sourceConsumer := run.environmentConsumer(t, ctx, client, kube, source)
	run.rotation(t, ctx, kube, source, sourceConsumer, false)
	run.rotation(t, ctx, kube, source, run.consumer(t, ctx, kube, sourceConsumer.name, sourceConsumer.secret), true)

	t.Log("quiesced SQL fixture captured; request full backup through production API")
	previous := run.catalogue(t, ctx)
	backup, err := client.BackupPostgresWithResponse(ctx, run.organization, run.space, "source")
	assert.NoErr(err)
	assertApplicationResponse(t, backup.StatusCode(), backup.Body, http.StatusAccepted)
	if backup.JSON202 == nil {
		t.Fatal("backup response missing operation")
	}
	run.waitOperation(t, ctx, client, backup.JSON202.ID)
	selected := run.newBackup(t, ctx, previous, source.ID)
	t.Logf("restore catalogue backup %s into a new cluster", selected.ID)
	run.cli(t, ctx, "backup", "restore", "restored", "--backup", selected.ID, "--timeout", "12m")
	restored := run.database(t, ctx, "restored")
	assert.True(restored.ID != source.ID)
	assert.True(restored.ClusterName != source.ClusterName)
	restoredPod := run.clientPod(t, ctx, kube, "restored", restored.Specification)
	actual := run.fingerprint(t, ctx, restoredPod)
	assert.Equal(actual, expected)
	assert.Equal(run.fingerprint(t, ctx, sourcePod), expected)
	restoredConsumer := run.environmentConsumer(t, ctx, client, kube, restored)
	run.restoreCredentialIsolation(t, ctx, source, restored)
	run.rotation(t, ctx, kube, restored, restoredConsumer, true)
	t.Log("all table row bytes, duplicate multiplicity, hashes, schema and sequence states match")

	result := run.sql(t, ctx, restoredPod, `
SELECT NOT pg_is_in_recovery();
BEGIN;
INSERT INTO recovery.accounts(name, enabled, balance, payload, created_at, birthday, token, document, tags, precise)
VALUES ('post-restore', true, 1.25, '\x00ff', '2024-01-01 00:00:00+00', '2000-01-01', 'ffffffff-ffff-ffff-ffff-ffffffffffff', '{}', '{}', 0) RETURNING id;
INSERT INTO recovery.entries(account_id, ordinal, note, occurred_at)
SELECT id, 1, 'write-read', '2024-01-01' FROM recovery.accounts WHERE name='post-restore';
SELECT count(*) FROM recovery.entries e JOIN recovery.accounts a ON e.account_id=a.id WHERE a.name='post-restore' AND e.note='write-read';
SELECT nextval('recovery.ticket_number');
ROLLBACK;
SELECT count(*) FROM recovery.accounts WHERE name='post-restore';`)
	assert.Equal(result, "t\n258\n1\n74\n0\n")
	// nextval intentionally survives rollback. Compare data and schema separately.
	after := run.fingerprint(t, ctx, restoredPod)
	assert.Equal(after.Tables, expected.Tables)
	assert.Equal(after.Schema, expected.Schema)
	t.Log("exercise the default weekly restore-check Job with real projected-token authentication")
	run.restoreCheck(t, ctx, kube, source)
	assert.Equal(run.fingerprint(t, ctx, sourcePod), expected)
	groups, err := client.ListEnvironmentGroupsWithResponse(ctx, run.organization, run.space)
	assert.NoErr(err)
	if groups.JSON200 == nil || len(*groups.JSON200) != 2 {
		t.Fatal("restore verification must not publish additional environment groups")
	}
	hosts, passwords := map[string]bool{}, map[string]bool{}
	for _, group := range *groups.JSON200 {
		for _, variable := range group.Values {
			if variable.Name == "PGHOST" {
				hosts[variable.Value] = true
			}
			if variable.Name == "PGPASSWORD" {
				passwords[variable.Value] = true
			}
		}
	}
	if len(hosts) != 2 || len(passwords) != 2 {
		t.Fatal("restored database must publish its own host and password")
	}
	run.monitoring(t, ctx, client, "restored")
	for _, database := range []appapi.Postgres{restored, source} {
		deleted, err := client.DeletePostgresWithResponse(ctx, run.organization, run.space, database.Name)
		assert.NoErr(err)
		if deleted.JSON202 == nil {
			t.Fatalf("database deletion returned status %d", deleted.StatusCode())
		}
		run.waitOperation(t, ctx, client, deleted.JSON202.Operation)
		run.credentialsRemoved(t, ctx, kube, database)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	assert.NoErr(err)
	producers, err := dynamicClient.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1alpha1", Resource: "pushsecrets"}).Namespace(run.config.managementNamespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/postgres-id in (" + source.ID + "," + restored.ID + ")"})
	assert.NoErr(err)
	assert.Equal(len(producers.Items), 0)
}

// SDK clients retain their configured transports through pointer receivers.
func (run postgresAcceptance) environmentConsumer(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, kube *kubernetes.Clientset, database appapi.Postgres) postgresConsumer {
	t.Helper()
	t.Logf("connect an application through the %s database environment group", database.Name)
	assert := is.New(t)
	if database.EnvironmentGroup == "" {
		t.Fatal("database does not report its managed environment group")
	}
	var expected map[string]string
	assert.NoErr(json.Unmarshal(run.cli(t, ctx, "env", database.Name, "--output", "json"), &expected))
	group, err := client.GetEnvironmentGroupWithResponse(ctx, run.organization, run.space, database.EnvironmentGroup)
	assert.NoErr(err)
	if group.JSON200 == nil {
		t.Fatalf("database environment group returned status %d", group.StatusCode())
	}
	assert.Equal(group.JSON200.ManagedBy, "postgres/"+database.Name)
	actual := map[string]string{}
	for _, variable := range group.JSON200.Values {
		actual[variable.Name] = variable.Value
	}
	if len(actual) != 6 || !maps.Equal(actual, expected) {
		t.Fatal("managed group does not contain the six CLI connection values")
	}
	rejected, err := client.PublishEnvironmentGroupWithResponse(ctx, run.organization, run.space, database.EnvironmentGroup, appapi.PublishEnvironmentGroupInput{Values: []appapi.Variable{{Name: "PGHOST", Value: "invalid.example"}}})
	assert.NoErr(err)
	if rejected.StatusCode() != http.StatusConflict || !bytes.Contains(bytes.ToLower(rejected.Body), []byte("managed")) {
		t.Fatal("manual publication did not explain that the database group is managed")
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", run.config.kubeconfig)
	assert.NoErr(err)
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	assert.NoErr(err)
	producers := dynamicClient.Resource(schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1alpha1", Resource: "pushsecrets"}).Namespace(run.config.managementNamespace)
	owned, err := producers.List(ctx, metav1.ListOptions{LabelSelector: "borq.run/postgres-id=" + database.ID})
	assert.NoErr(err)
	if len(owned.Items) != 1 {
		t.Fatal("database must have one owned environment producer")
	}
	producer := owned.Items[0]
	previousRefresh, _, err := unstructured.NestedString(producer.Object, "status", "refreshTime")
	assert.NoErr(err)
	baseline := *group.JSON200
	refreshContext, refreshCancel := context.WithTimeout(ctx, time.Minute)
	defer refreshCancel()
	for {
		current, err := producers.Get(refreshContext, producer.GetName(), metav1.GetOptions{})
		assert.NoErr(err)
		refreshed, _, err := unstructured.NestedString(current.Object, "status", "refreshTime")
		assert.NoErr(err)
		if refreshed != "" && refreshed != previousRefresh {
			observed, err := client.GetEnvironmentGroupWithResponse(refreshContext, run.organization, run.space, database.EnvironmentGroup)
			assert.NoErr(err)
			if observed.JSON200 == nil {
				t.Fatal("managed database environment disappeared during controller refresh")
			}
			if slices.Equal(observed.JSON200.Values, baseline.Values) {
				if observed.JSON200.Version != baseline.Version {
					t.Fatal("unchanged controller refresh allocated a new environment version")
				}
				break
			}
			baseline = *observed.JSON200
			previousRefresh = refreshed
		}
		select {
		case <-refreshContext.Done():
			t.Fatal("External Secrets did not complete an unchanged refresh")
		case <-time.After(time.Second):
		}
	}
	command, port := "sleep 2400", 0
	predeploy := "psql -X -qAt --set ON_ERROR_STOP=1 -c 'SELECT 1;'"
	name := "env-consumer-" + database.Name
	patch := []appapi.AppPatch{{Name: name, Image: database.Specification.Image, Command: &command, Port: &port, Env: applicationEnvPatch(database.EnvironmentGroup, "latest"), Predeploy: &appapi.PredeployPatch{Command: &predeploy}}}
	plan := createApplicationPlan(t, ctx, client, run.organization, run.space, appapi.Patch{Apps: &patch})
	t.Logf("accepted consumer %s plan %s", name, plan.ID)
	secretName := postgresLiveSecret(t, plan, name)
	encodedPlan, err := json.Marshal(plan)
	assert.NoErr(err)
	if bytes.Contains(encodedPlan, []byte(expected["PGPASSWORD"])) {
		t.Fatal("public application plan contains a rotating database password")
	}
	if applicationByName(t, plan.Configuration, name).Env.Version != "latest" {
		t.Fatal("managed database consumer was pinned to an expiring credential version")
	}
	applyApplicationPlan(t, ctx, client, plan.ID)
	consumerContext, consumerCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer consumerCancel()
	for {
		secret, err := kube.CoreV1().Secrets(run.namespace).Get(consumerContext, secretName, metav1.GetOptions{})
		assert.NoErr(err)
		if secret.Immutable != nil && *secret.Immutable {
			t.Fatal("managed database consumer Secret must permit rotation")
		}
		assert.NoErr(json.Unmarshal(run.cli(t, consumerContext, "env", database.Name, "--output", "json"), &expected))
		consumed := map[string]string{}
		for key, value := range secret.Data {
			consumed[key] = string(value)
		}
		pods, err := kube.CoreV1().Pods(run.namespace).List(consumerContext, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + name})
		assert.NoErr(err)
		if maps.Equal(consumed, expected) {
			for _, pod := range pods.Items {
				if !postgresConsumerReady(pod) {
					continue
				}
				output, _, err := run.sqlAttempt(consumerContext, pod.Name, "SELECT count(*) FROM recovery.accounts;", "")
				if err == nil && output == "257\n" {
					return run.consumer(t, consumerContext, kube, name, secretName)
				}
			}
		}
		select {
		case <-consumerContext.Done():
			t.Fatal("application did not consume all six current CLI connection values and connect to its database")
		case <-time.After(time.Second):
		}
	}
}

func (run postgresAcceptance) cli(t *testing.T, ctx context.Context, arguments ...string) []byte {
	t.Helper()
	args := append([]string{"am", "postgres"}, arguments...)
	args = append(args, "--api-url", run.config.server, "--organization", run.organization, "--space", run.space)
	command := exec.CommandContext(ctx, run.config.binary, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		// env output contains credentials, so it must never enter failure diagnostics.
		t.Fatalf("managed postgres CLI %s failed: %v: %s", arguments[0], err, stderr.String())
	}
	return output
}

func (run postgresAcceptance) database(t *testing.T, ctx context.Context, name string) appapi.Postgres {
	t.Helper()
	var value appapi.Postgres
	if err := yaml.Unmarshal(run.cli(t, ctx, "get", name), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (run postgresAcceptance) catalogue(t *testing.T, ctx context.Context) []appapi.PostgresBackup {
	t.Helper()
	var value []appapi.PostgresBackup
	if err := yaml.Unmarshal(run.cli(t, ctx, "backup", "list", "source"), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (run postgresAcceptance) newBackup(t *testing.T, ctx context.Context, previous []appapi.PostgresBackup, source string) appapi.PostgresBackup {
	t.Helper()
	for {
		for _, candidate := range run.catalogue(t, ctx) {
			if !candidate.Eligible || candidate.SourceID != source || candidate.CompletedAt.IsZero() {
				continue
			}
			if slices.ContainsFunc(previous, func(old appapi.PostgresBackup) bool { return old.ID == candidate.ID }) {
				continue
			}
			return candidate
		}
		select {
		case <-ctx.Done():
			t.Fatal("new full backup did not become eligible in the CLI catalogue")
		case <-time.After(2 * time.Second):
		}
	}
}

// Generated HTTP clients and Kubernetes clients use pointers for connection state.
func (run postgresAcceptance) waitOperation(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, id string) {
	t.Helper()
	for {
		response, err := client.GetPostgresOperationWithResponse(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if response.JSON200 == nil {
			t.Fatalf("operation %s returned status %d", id, response.StatusCode())
		}
		operation := *response.JSON200
		if operation.Status == appapi.Succeeded {
			return
		}
		if operation.Status == appapi.Failed || operation.Status == appapi.Canceled {
			t.Fatalf("operation %s %s: %s", id, operation.Status, operation.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("operation %s timed out", id)
		case <-time.After(time.Second):
		}
	}
}

// The client pod consumes exactly the credentials emitted by the public CLI.
func (run postgresAcceptance) clientPod(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, name string, specification appapi.PostgresInput) string {
	t.Helper()
	var credentials map[string]string
	if err := json.Unmarshal(run.cli(t, ctx, "env", name, "--output", "json"), &credentials); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "DATABASE_URL"} {
		if credentials[key] == "" {
			t.Fatalf("CLI env missing %s", key)
		}
	}
	image := ""
	if specification.Image != nil {
		image = *specification.Image
	}
	if image == "" {
		t.Fatal("normalized database specification missing image")
	}
	environment := make([]corev1.EnvVar, 0, len(credentials))
	for key, value := range credentials {
		environment = append(environment, corev1.EnvVar{Name: key, Value: value})
	}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "recovery-client-" + name, Namespace: run.namespace}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers:    []corev1.Container{{Name: "client", Image: image, Command: []string{"sleep", "1800"}, Env: environment}},
	}}
	if _, err := kube.CoreV1().Pods(run.namespace).Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for {
		current, err := kube.CoreV1().Pods(run.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase == corev1.PodRunning {
			break
		}
		if current.Status.Phase == corev1.PodFailed {
			t.Fatal("SQL client pod failed")
		}
		select {
		case <-ctx.Done():
			t.Fatal("SQL client pod did not start")
		case <-time.After(time.Second):
		}
	}
	if output := run.sql(t, ctx, pod.Name, "SELECT 1;"); output != "1\n" {
		t.Fatalf("SQL readiness returned %q", output)
	}
	return pod.Name
}

func (run postgresAcceptance) sql(t *testing.T, ctx context.Context, pod, sql string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	name, dedicated := strings.CutPrefix(pod, "recovery-client-")
	for {
		password := ""
		if dedicated {
			var credentials map[string]string
			if err := json.Unmarshal(run.cli(t, ctx, "env", name, "--output", "json"), &credentials); err != nil {
				t.Fatal(err)
			}
			password = credentials["PGPASSWORD"]
		}
		output, diagnostic, err := run.sqlAttempt(ctx, pod, sql, password)
		if err == nil {
			return output
		}
		if !dedicated || !strings.Contains(diagnostic, "password authentication failed") {
			t.Fatalf("real SQL execution failed: %v: %s", err, diagnostic)
		}
		select {
		case <-ctx.Done():
			t.Fatal("SQL client did not receive the current CLI password after automatic rotation")
		case <-time.After(time.Second):
		}
	}
}

func (run postgresAcceptance) sqlAttempt(ctx context.Context, pod, sql, password string) (string, string, error) {
	arguments := []string{"psql", "-X", "-qAt", "--set", "ON_ERROR_STOP=1"}
	input := "SET timezone='UTC'; SET extra_float_digits=3; SET bytea_output='hex';\n" + sql
	if password != "" {
		arguments = []string{"sh", "-c", "IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -qAt --set ON_ERROR_STOP=1"}
		input = password + "\n" + input
	}
	commandArguments := []string{"--kubeconfig", run.config.kubeconfig, "--namespace", run.namespace, "exec", "-i", pod, "--"}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(ctx, "kubectl", commandArguments...)
	command.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	return string(output), stderr.String(), err
}

func (run postgresAcceptance) fingerprint(t *testing.T, ctx context.Context, pod string) postgresFingerprint {
	t.Helper()
	result := postgresFingerprint{Tables: map[string]postgresTableFingerprint{}}
	for _, table := range []string{"accounts", "entries", "duplicates", "empty_table"} {
		output := run.sql(t, ctx, pod, "SELECT encode(record_send(row_value), 'hex') FROM recovery."+table+" row_value;")
		var rows []string
		if output != "" {
			for _, encoded := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
				row, err := hex.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(row)
				rows = append(rows, hex.EncodeToString(digest[:]))
			}
		}
		slices.Sort(rows)
		// Fixed-width hashes retain duplicate rows and make concatenation unambiguous.
		digest := sha256.Sum256([]byte(strings.Join(rows, "")))
		result.Tables[table] = postgresTableFingerprint{Count: len(rows), Rows: rows, Hash: hex.EncodeToString(digest[:])}
	}
	result.Schema = run.sql(t, ctx, pod, `
SELECT jsonb_build_array(table_name, column_name, ordinal_position, data_type, udt_name, is_nullable, column_default, is_identity, identity_generation, identity_start, identity_increment, character_maximum_length, numeric_precision, numeric_scale)::text
FROM information_schema.columns WHERE table_schema='recovery' ORDER BY table_name, ordinal_position;
SELECT jsonb_build_array(c.relname, con.conname, con.contype, pg_get_constraintdef(con.oid, true), con.convalidated)::text
FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname='recovery' ORDER BY c.relname, con.conname;
SELECT jsonb_build_array(tablename, indexname, indexdef)::text FROM pg_indexes WHERE schemaname='recovery' ORDER BY tablename, indexname;
SELECT jsonb_build_array(sequencename, data_type, start_value, min_value, max_value, increment_by, cycle, cache_size)::text
FROM pg_sequences WHERE schemaname='recovery' ORDER BY sequencename;`)
	result.Sequences = run.sql(t, ctx, pod, `
SELECT jsonb_build_array('accounts_id_seq', last_value, is_called)::text FROM recovery.accounts_id_seq;
SELECT jsonb_build_array('entries_id_seq', last_value, is_called)::text FROM recovery.entries_id_seq;
SELECT jsonb_build_array('ticket_number', last_value, is_called)::text FROM recovery.ticket_number;`)
	return result
}

// The CronJob template supplies the real service account, bound token and trigger.
func (run postgresAcceptance) restoreCheck(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, source appapi.Postgres) {
	t.Helper()
	assert := is.New(t)
	cron, err := kube.BatchV1().CronJobs(run.namespace).Get(ctx, source.ClusterName+"-check", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(cron.Spec.Schedule, "0 3 * * 0")
	assert.Equal(cron.Spec.ConcurrencyPolicy, batchv1.ForbidConcurrent)
	config, err := clientcmd.BuildConfigFromFlags("", run.config.kubeconfig)
	assert.NoErr(err)
	resources, err := dynamic.NewForConfig(config)
	assert.NoErr(err)
	beforeClusters, beforeVolumes := run.recoveryInventory(t, ctx, kube, resources)
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "acceptance-restore-check-", Namespace: run.namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cron, schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"})},
	}, Spec: *cron.Spec.JobTemplate.Spec.DeepCopy()}
	created, err := kube.BatchV1().Jobs(run.namespace).Create(ctx, &job, metav1.CreateOptions{})
	assert.NoErr(err)
	for {
		current, err := kube.BatchV1().Jobs(run.namespace).Get(ctx, created.Name, metav1.GetOptions{})
		assert.NoErr(err)
		complete := false
		for _, condition := range current.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			if condition.Type == batchv1.JobFailed {
				t.Fatalf("restore-check Job failed: %s: %s", condition.Reason, condition.Message)
			}
			if condition.Type == batchv1.JobComplete {
				complete = true
			}
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("restore-check Job did not complete")
		case <-time.After(2 * time.Second):
		}
	}
	observed := run.database(t, ctx, "source")
	assert.Equal(observed.RestoreCheck.Status, "succeeded")
	if observed.RestoreCheck.Cleanup == nil || observed.RestoreCheck.SnapshotStatus == nil || observed.RestoreCheck.DurationSeconds == nil {
		t.Fatal("restore-check result missing cleanup, snapshot capability or duration")
	}
	assert.Equal(*observed.RestoreCheck.Cleanup, "succeeded")
	assert.Equal(*observed.RestoreCheck.SnapshotStatus, "unsupported")
	assert.True(*observed.RestoreCheck.DurationSeconds > 0)
	afterClusters, afterVolumes := run.recoveryInventory(t, ctx, kube, resources)
	assert.Equal(afterClusters, beforeClusters)
	assert.Equal(afterVolumes, beforeVolumes)
}

func (run postgresAcceptance) recoveryInventory(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, resources *dynamic.DynamicClient) ([]string, []string) {
	t.Helper()
	clusters, err := resources.Resource(schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}).Namespace(run.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := kube.CoreV1().PersistentVolumeClaims(run.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	clusterNames, volumeNames := []string{}, []string{}
	for _, cluster := range clusters.Items {
		clusterNames = append(clusterNames, cluster.GetName())
	}
	for _, volume := range volumes.Items {
		volumeNames = append(volumeNames, volume.Name)
	}
	slices.Sort(clusterNames)
	slices.Sort(volumeNames)
	return clusterNames, volumeNames
}

func (run postgresAcceptance) monitoring(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	history := appapi.GetPostgresMetricsParamsRange("30m")
	logHistory := appapi.GetPostgresLogsParamsRange("30m")
	limit := 50
	for {
		metrics, err := client.GetPostgresMetricsWithResponse(ctx, run.organization, run.space, name, &appapi.GetPostgresMetricsParams{Range: &history})
		if err != nil {
			t.Fatal(err)
		}
		assertApplicationResponse(t, metrics.StatusCode(), metrics.Body, http.StatusOK)
		logs, err := client.GetPostgresLogsWithResponse(ctx, run.organization, run.space, name, &appapi.GetPostgresLogsParams{Range: &logHistory, Limit: &limit})
		if err != nil {
			t.Fatal(err)
		}
		assertApplicationResponse(t, logs.StatusCode(), logs.Body, http.StatusOK)
		if metrics.JSON200 == nil || logs.JSON200 == nil {
			t.Fatal("monitoring response missing metrics or logs")
		}
		value := *metrics.JSON200
		if value.ActivityAvailable && value.DiskAvailable && value.DiskBytes > 0 && value.DiskCapacityBytes > 0 && value.Transactions > 0 && len(value.Usage.CPUCores) > 0 && len(value.Usage.MemoryBytes) > 0 && len(logs.JSON200.Entries) > 0 {
			if len(logs.JSON200.Entries) > limit {
				t.Fatal("logs exceeded requested bound")
			}
			t.Log("restored database exposes real CPU/memory, disk, SQL activity and bounded logs")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("restored monitoring unavailable: activity=%t disk=%t cpuSamples=%d memorySamples=%d logEntries=%d", value.ActivityAvailable, value.DiskAvailable, len(value.Usage.CPUCores), len(value.Usage.MemoryBytes), len(logs.JSON200.Entries))
		case <-time.After(5 * time.Second):
		}
	}
}

// Cleanup has its own deadline because the test context may already be canceled.
func (run postgresAcceptance) cleanup(t *testing.T, client *appapi.ClientWithResponses, kube *kubernetes.Clientset) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	namespace := run.namespace
	for _, name := range []string{"restored", "source"} {
		current, err := client.GetPostgresWithResponse(ctx, run.organization, run.space, name)
		if err != nil {
			t.Errorf("cleanup inspect %s: %v", name, err)
			continue
		}
		if current.JSON200 == nil {
			continue
		}
		if namespace == "" {
			namespace = current.JSON200.Namespace
		}
		if current.JSON200.State == "removed" {
			continue
		}
		response, err := client.DeletePostgresWithResponse(ctx, run.organization, run.space, name)
		if err != nil {
			t.Errorf("cleanup delete %s: %v", name, err)
			continue
		}
		if response.StatusCode() != http.StatusAccepted {
			t.Errorf("cleanup delete %s status %d", name, response.StatusCode())
			continue
		}
		if response.JSON202 == nil {
			t.Errorf("cleanup delete %s missing operation", name)
			continue
		}
		operationContext, operationCancel := context.WithTimeout(ctx, 45*time.Second)
	waiting:
		for {
			operation, err := client.GetPostgresOperationWithResponse(operationContext, response.JSON202.Operation)
			if err != nil {
				t.Errorf("cleanup wait %s: %v", name, err)
				break
			}
			if operation.JSON200 == nil {
				t.Errorf("cleanup operation %s missing", name)
				break
			}
			if operation.JSON200.Status == appapi.Succeeded {
				break
			}
			if operation.JSON200.Status == appapi.Failed {
				t.Errorf("cleanup operation %s: %s", name, operation.JSON200.Message)
				break
			}
			select {
			case <-operationContext.Done():
				t.Errorf("cleanup operation %s timed out", name)
				break waiting
			case <-time.After(time.Second):
			}
		}
		operationCancel()
	}
	if namespace == "" {
		return
	}
	namespaceContext, namespaceCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer namespaceCancel()
	ctx = namespaceContext
	if err := kube.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("cleanup isolated namespace: %v", err)
		return
	}
	for {
		_, err := kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Errorf("cleanup namespace readback: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			t.Errorf("isolated namespace %s still exists; temporary PVC cleanup incomplete", namespace)
			return
		case <-time.After(time.Second):
		}
	}
}
