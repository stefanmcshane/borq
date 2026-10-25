package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementS3Persistence verifies vault-only credentials, failed rotation isolation, and reopen/export behavior.
func TestManagementS3Persistence(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagementStorageFixture(t)
	databasePath := filepath.Join(t.TempDir(), "borq.sqlite")
	input := NewInput{DatabasePath: databasePath, ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Profile: "dev", Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	storage, err := manager.PutManagementS3(ctx, ManagementS3Input{Endpoint: fixture.s3.URL, Region: "us-east-1", Bucket: "backups", Prefix: "borq", PathStyle: true, AccessKey: "first-access", SecretKey: "first-secret"})
	assert.NoErr(err)
	assert.True(strings.HasPrefix(storage.CredentialsRef, "borq/management/s3/"))
	_, err = manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	// Production must reject this loopback fixture before granting shared storage.
	manager.runtime.platformSettings.Profile = "production"
	_, err = manager.SetOrganizationManagedAddons(ctx, "acme", true)
	assert.True(errors.Is(err, ErrInvalid))
	manager.runtime.platformSettings.Profile = "dev"
	granted, err := manager.SetOrganizationManagedAddons(ctx, "acme", true)
	assert.NoErr(err)
	assert.True(granted.ManagedAddons)
	assert.True(granted.ID != "")
	scoped, _, err := manager.ResolvePostgresStorage(ctx, "acme")
	assert.NoErr(err)
	assert.Equal(scoped.Prefix, "borq/organizations/"+granted.ID+"/postgres")
	fixture.mutex.Lock()
	fixture.denied = true
	fixture.mutex.Unlock()
	_, err = manager.PutManagementS3(ctx, ManagementS3Input{Endpoint: fixture.s3.URL, Region: "us-east-1", Bucket: "backups", Prefix: "borq", PathStyle: true, AccessKey: "second-access", SecretKey: "second-secret"})
	assert.True(errors.Is(err, ErrInvalid))
	current, err := manager.GetManagementS3(ctx)
	assert.NoErr(err)
	assert.Equal(current.CredentialsRef, storage.CredentialsRef)
	for _, collection := range []string{"management-s3", "management-s3-rotation", "management-task", "management-operation"} {
		records, err := manager.database.List(ctx, collection)
		assert.NoErr(err)
		for _, record := range records {
			assert.True(!strings.Contains(string(record.Value), "first-secret"))
			assert.True(!strings.Contains(string(record.Value), "first-access"))
			assert.True(!strings.Contains(string(record.Value), "second-secret"))
		}
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	defer manager.Close()
	current, err = manager.GetManagementS3(ctx)
	assert.NoErr(err)
	assert.Equal(current, storage)
	configuration, err := manager.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "first-secret"))
	assert.True(!strings.Contains(string(encoded), "first-access"))
	assert.Equal(configuration.S3.CredentialsRef, storage.CredentialsRef)
}

// TestManagementNamedApplyPreservesResources proves aggregate progress and omission semantics through durable state.
func TestManagementNamedApplyPreservesResources(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "borq.sqlite")
	manager, err := NewManager(ctx, NewInput{DatabasePath: databasePath, PollInterval: time.Millisecond})
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, "retained")
	assert.NoErr(err)
	accepted, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "new"}}})
	assert.NoErr(err)
	waitManagementResult(t, manager, accepted.ID, "succeeded")
	accepted, err = manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{}})
	assert.NoErr(err)
	waitManagementResult(t, manager, accepted.ID, "succeeded")
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, NewInput{DatabasePath: databasePath, PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	organizations, err := manager.ListOrganizations(ctx)
	assert.NoErr(err)
	assert.Equal(len(organizations), 2)
	assert.Equal(organizations[0].Name, "new")
	assert.Equal(organizations[1].Name, "retained")
	operation, err := manager.GetManagementOperation(ctx, accepted.ID)
	assert.NoErr(err)
	assert.Equal(operation.Status, "succeeded")
}

// TestManagementApplyResumesAfterRestart proves a real accepted task resumes from a disconnected S3 operation.
func TestManagementApplyResumesAfterRestart(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagementStorageFixture(t)
	fixture.listGate = make(chan struct{})
	fixture.gateAt = 2
	input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: fixture.kubeconfig, PlatformSettings: platform.Settings{Profile: "dev", Namespace: "borq"}, PollInterval: time.Millisecond}
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{S3: &ManagementS3Input{Endpoint: fixture.s3.URL, Region: "us-east-1", Bucket: "backups", PathStyle: true, AccessKey: "restart-access", SecretKey: "restart-secret"}, Organizations: []ManagementOrganization{{Name: "resumed"}}})
	assert.NoErr(err)
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		fixture.mutex.Lock()
		blocked = fixture.listCount >= 2
		fixture.mutex.Unlock()
		if blocked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assert.True(blocked)
	pending, err := manager.GetManagementOperation(ctx, operation.ID)
	assert.NoErr(err)
	assert.True(!operationFinished(pending.Status))
	inline, err := os.ReadFile(fixture.kubeconfig)
	assert.NoErr(err)
	_, err = manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "rejected", Clusters: []ClusterInput{{Name: "remote", Kubeconfig: string(inline)}}}}})
	assert.True(errors.Is(err, ErrConflict))
	_, err = os.Stat(manager.runtime.credentialsDir)
	assert.True(errors.Is(err, os.ErrNotExist))
	assert.NoErr(manager.Close())
	close(fixture.listGate)
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	defer manager.Close()
	waitManagementResult(t, manager, operation.ID, "succeeded")
	organizations, err := manager.ListOrganizations(ctx)
	assert.NoErr(err)
	assert.Equal(len(organizations), 1)
	assert.Equal(organizations[0].Name, "resumed")
	task, err := manager.database.Get(ctx, "management-task", operation.ID)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(task.Value), "restart-access"))
	assert.True(!strings.Contains(string(task.Value), "restart-secret"))
}

// TestManagementStorageDependencyGuard blocks storage removal, moves, and grant revocation before touching consumers.
func TestManagementStorageDependencyGuard(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite")})
	assert.NoErr(err)
	defer manager.Close()
	organization, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	organization.ManagedAddons = true
	write, err := documentWrite("organization", "acme", organization, 1)
	assert.NoErr(err)
	archive, err := documentWrite("postgres-archive", "preserved", map[string]string{"organization": "acme"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{write, archive}))
	assert.True(errors.Is(manager.DeleteManagementS3(ctx), ErrConflict))
	_, err = manager.SetOrganizationManagedAddons(ctx, "acme", false)
	assert.True(errors.Is(err, ErrConflict))
	current, err := manager.GetManagementS3(ctx)
	assert.NoErr(err)
	moved := current
	moved.Bucket = "elsewhere"
	_, err = manager.activateManagementS3(ctx, moved)
	assert.True(errors.Is(err, ErrConflict))
}

func waitManagementResult(t *testing.T, manager Manager, id, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := manager.GetManagementOperation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if operationFinished(operation.Status) {
			if operation.Status != status {
				t.Fatalf("operation %s: %s", operation.Status, operation.Message)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("management operation did not finish")
}

// The fixture models only Kubernetes vault access and the S3/OpenBao HTTP contracts, never controller success.
type managementStorageFixture struct {
	mutex      sync.Mutex
	s3         *httptest.Server
	kubeconfig string
	denied     bool
	listGate   chan struct{}
	gateAt     int
	listCount  int
}

func newManagementStorageFixture(t *testing.T) *managementStorageFixture {
	t.Helper()
	fixture := &managementStorageFixture{}
	objects := map[string][]byte{}
	fixture.s3 = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mutex.Lock()
		if request.URL.Query().Get("list-type") == "2" {
			fixture.listCount++
			gate, blocked, denied := fixture.listGate, fixture.listCount == fixture.gateAt, fixture.denied
			fixture.mutex.Unlock()
			if denied {
				writer.WriteHeader(http.StatusForbidden)
				return
			}
			if gate != nil && blocked {
				select {
				case <-gate:
				case <-request.Context().Done():
					return
				}
			}
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = writer.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
			return
		}
		defer fixture.mutex.Unlock()
		if fixture.denied {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		switch request.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(request.Body)
			objects[request.URL.Path] = data
		case http.MethodGet:
			_, _ = writer.Write(objects[request.URL.Path])
		case http.MethodDelete:
			delete(objects, request.URL.Path)
			writer.WriteHeader(204)
		default:
			writer.WriteHeader(405)
		}
	}))
	t.Cleanup(fixture.s3.Close)
	documents := map[string]map[string]string{}
	vault := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mutex.Lock()
		defer fixture.mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if discovery, exists := managerDiscovery(request.URL.Path); exists {
			_ = json.NewEncoder(writer).Encode(discovery)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/secrets/borq-openbao-admin") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": "borq-openbao-admin", "namespace": "borq"}, "data": map[string]string{"token": base64.StdEncoding.EncodeToString([]byte("admin"))}})
			return
		}
		if strings.HasSuffix(request.URL.Path, "/sys/seal-status") {
			_, _ = writer.Write([]byte(`{"initialized":true,"sealed":false}`))
			return
		}
		marker := "/proxy/v1/secret/data/"
		_, key, ok := strings.Cut(request.URL.Path, marker)
		if !ok {
			writer.WriteHeader(404)
			return
		}
		if request.Method == http.MethodPut {
			var body struct {
				Data struct {
					Values map[string]string `json:"values"`
				} `json:"data"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				writer.WriteHeader(400)
				return
			}
			documents[key] = body.Data.Values
			_, _ = writer.Write([]byte(`{"data":{"version":1}}`))
			return
		}
		values, exists := documents[key]
		if !exists {
			writer.WriteHeader(404)
			_, _ = writer.Write([]byte(`{"errors":["not found"]}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"values": values}, "metadata": map[string]int{"version": 1}}})
	}))
	t.Cleanup(vault.Close)
	fixture.kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	document := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: vault\n  cluster:\n    server: %s\ncontexts:\n- name: vault\n  context:\n    cluster: vault\n    user: vault\ncurrent-context: vault\nusers:\n- name: vault\n  user: {}\n", vault.URL)
	if err := os.WriteFile(fixture.kubeconfig, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestManagementInlineClusterRetry verifies failed validation leaves credentials replaceable.
func TestManagementInlineClusterRetry(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	manager, err := NewManager(ctx, NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PollInterval: time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	target := newComponentTarget(t)
	valid, err := os.ReadFile(target.kubeconfig)
	assert.NoErr(err)
	lines := strings.Split(string(valid), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "    server:") {
			lines[index] = "    server: http://127.0.0.1:1"
		}
	}
	bad := strings.Join(lines, "\n")
	configuration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "acme", Clusters: []ClusterInput{{Name: "remote", Kubeconfig: bad}}}}}
	first, err := manager.ApplyManagementConfiguration(ctx, configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, first.ID, "failed")
	files, err := os.ReadDir(manager.runtime.credentialsDir)
	assert.NoErr(err)
	assert.Equal(len(files), 0)
	configuration.Organizations[0].Clusters[0].Kubeconfig = string(valid)
	second, err := manager.ApplyManagementConfiguration(ctx, configuration)
	assert.NoErr(err)
	waitManagementResult(t, manager, second.ID, "succeeded")
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", "acme/remote")
	assert.NoErr(err)
	assert.True(strings.HasPrefix(filepath.Base(cluster.KubeconfigPath), "management-"))
	_, err = os.Stat(cluster.KubeconfigPath)
	assert.NoErr(err)
	task, err := manager.database.Get(ctx, "management-task", second.ID)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(task.Value), "current-context"))
}

// TestManagementComponentWaitRestartDeadline checks durable queue ownership and the original timeout.
func TestManagementComponentWaitRestartDeadline(t *testing.T) {
	for _, outcome := range []string{"failed", "replaced", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			assert := is.New(t)
			ctx := context.Background()
			target := newComponentTarget(t)
			input := componentManagerInput(t, target.kubeconfig)
			input.PollInterval = time.Millisecond
			manager, err := NewManager(ctx, input)
			assert.NoErr(err)
			_, err = manager.CreateOrganization(ctx, "acme")
			assert.NoErr(err)
			cluster, err := manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig})
			assert.NoErr(err)
			queued, err := manager.GetClusterComponents(ctx, "acme", "remote")
			assert.NoErr(err)
			now := time.Now().UTC()
			operation := Operation{ID: "aggregate", Status: "queued", Phase: "management", StartedAt: now, UpdatedAt: now}
			task := managementTask{Configuration: ManagementConfiguration{Components: map[string]platform.ComponentOverride{"alloy": {}}}, Deadline: now.Add(300 * time.Millisecond), ComponentsStarted: true, ComponentRuns: []managementComponentRun{{Organization: "acme", Cluster: "remote", ID: queued.ID}}}
			operationWrite, err := documentWrite("management-operation", operation.ID, operation, 0)
			assert.NoErr(err)
			taskWrite, err := documentWrite("management-task", operation.ID, task, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(ctx, []sqlite.Write{operationWrite, taskWrite}))
			manager.startManagementOperation(operation)
			for {
				pending, err := manager.GetManagementOperation(ctx, operation.ID)
				assert.NoErr(err)
				assert.True(!operationFinished(pending.Status))
				if pending.Status == "running" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			assert.NoErr(manager.Close())
			input.RolloutTimeout = time.Hour
			manager, err = NewManager(ctx, input)
			assert.NoErr(err)
			defer manager.Close()
			reopened, _, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
			assert.NoErr(err)
			assert.Equal(reopened.Deadline, task.Deadline)
			assert.Equal(reopened.ComponentRuns[0].ID, queued.ID)
			if outcome != "timeout" {
				stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", cluster.ID)
				assert.NoErr(err)
				if outcome == "failed" {
					stored.Installation.Status = "failed"
				} else {
					stored.Installation.ID = "different-run"
				}
				assert.NoErr(manager.saveComponentRecord(ctx, cluster.ID, stored))
			}
			assert.NoErr(manager.resumeManagementOperations(ctx))
			waitManagementResult(t, manager, operation.ID, "failed")
			result, err := manager.GetManagementOperation(ctx, operation.ID)
			assert.NoErr(err)
			expected := "target component reconciliation failed"
			if outcome == "timeout" {
				expected = "exceeded its execution timeout"
			}
			if outcome == "replaced" {
				expected = "reconciliation was replaced"
			}
			assert.True(strings.Contains(result.Message, expected))
		})
	}
}

// TestManagementComponentQueueOwnership keeps another accepted configuration from sharing an active upgrade.
func TestManagementComponentQueueOwnership(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	target := newComponentTarget(t)
	input := componentManagerInput(t, target.kubeconfig)
	manager, err := NewManager(ctx, input)
	assert.NoErr(err)
	_, err = manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	disabled := false
	cluster, err := manager.RegisterCluster(ctx, ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: target.kubeconfig, Reconcile: &disabled})
	assert.NoErr(err)
	queued, err := manager.queueClusterComponentsSelection(ctx, cluster, true, []string{"alloy"}, "first")
	assert.NoErr(err)
	_, err = manager.queueClusterComponentsSelection(ctx, cluster, true, []string{"alloy"}, "second")
	assert.True(errors.Is(err, ErrConflict))
	assert.NoErr(manager.Close())
	manager, err = NewManager(ctx, input)
	assert.NoErr(err)
	defer manager.Close()
	repeated, err := manager.queueClusterComponentsSelection(ctx, cluster, true, []string{"alloy"}, "first")
	assert.NoErr(err)
	assert.Equal(repeated.ID, queued.ID)
	stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", cluster.ID)
	assert.NoErr(err)
	assert.Equal(stored.UpgradeComponents, []string{"alloy"})
}
