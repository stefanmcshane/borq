package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestClusterTailscalePersistence proves private credentials survive reopening,
// remain absent from durable public records, and rotate without changing connection identity.
func TestClusterTailscalePersistence(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagerCluster(t, managerClusterOptions{})
	path := filepath.Join(t.TempDir(), "borq.sqlite")
	manager := openTestManager(t, path, time.Second)
	_, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	input := ClusterInput{Organization: "acme", Name: "apps", KubeconfigPath: fixture.kubeconfig, Tailscale: &ClusterTailscale{ClientID: "target-client-one", ClientSecret: "target-secret-one"}}
	first, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	assert.True(first.Tailscale != nil && first.Tailscale.ClientID == "" && first.Tailscale.ClientSecret == "")
	file := filepath.Join(manager.runtime.credentialsDir, first.Tailscale.CredentialsRef+".json")
	info, err := os.Stat(file)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0o600))
	assert.NoErr(manager.Close())
	manager = openTestManager(t, path, time.Second)
	registered, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", "acme/apps")
	assert.NoErr(err)
	credentials, err := manager.readClusterTailscale(registered)
	assert.NoErr(err)
	assert.True(credentials.ClientID == input.Tailscale.ClientID && credentials.ClientSecret == input.Tailscale.ClientSecret)
	repeated, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	assert.Equal(repeated.Tailscale.CredentialsRef, first.Tailscale.CredentialsRef)
	input.Tailscale = nil
	preserved, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	assert.Equal(preserved.Tailscale.CredentialsRef, first.Tailscale.CredentialsRef)
	input.Tailscale = &ClusterTailscale{ClientID: "target-client-two", ClientSecret: "target-secret-two"}
	rotated, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	assert.True(rotated.Tailscale.CredentialsRef != first.Tailscale.CredentialsRef)
	entries, err := os.ReadDir(manager.runtime.credentialsDir)
	assert.NoErr(err)
	assert.Equal(len(entries), 2)
	input.Context = "alternate"
	input.Tailscale = &ClusterTailscale{ClientID: "rejected-client", ClientSecret: "rejected-secret"}
	_, err = manager.RegisterCluster(ctx, input)
	assert.True(errors.Is(err, ErrConflict) && strings.Contains(err.Error(), "different connection"))
	after, err := os.ReadDir(manager.runtime.credentialsDir)
	assert.NoErr(err)
	assert.Equal(len(after), len(entries))
	input.Context, input.Name, input.Tailscale = "", "other", rotated.Tailscale
	_, err = manager.RegisterCluster(ctx, input)
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "belong to this cluster"))
	configuration, err := manager.GetManagementConfiguration(ctx)
	assert.NoErr(err)
	encoded, err := json.Marshal(configuration)
	assert.NoErr(err)
	assert.True(strings.Contains(string(encoded), rotated.Tailscale.CredentialsRef))
	assert.True(!strings.Contains(string(encoded), "target-client-") && !strings.Contains(string(encoded), "target-secret-"))
	records, err := manager.database.List(ctx, "cluster")
	assert.NoErr(err)
	for _, record := range records {
		assert.True(!strings.Contains(string(record.Value), "target-client-") && !strings.Contains(string(record.Value), "target-secret-"))
	}
	assert.NoErr(manager.Close())
	stored, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(stored), "target-client-") && !strings.Contains(string(stored), "target-secret-"))
}

// TestClusterTailscaleActiveRotation prevents an installing run from reporting
// success with superseded credentials, including registration-only CLI requests.
func TestClusterTailscaleActiveRotation(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	fixture := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	_, err := manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	reconcile := false
	input := ClusterInput{Organization: "acme", Name: "apps", KubeconfigPath: fixture.kubeconfig, Reconcile: &reconcile, Tailscale: &ClusterTailscale{ClientID: "active-client", ClientSecret: "active-secret"}}
	initial, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	for _, status := range []string{"queued", "diffing", "installing"} {
		state := componentRecord{Installation: ComponentInstallation{ClusterID: initial.ID, Status: status}}
		assert.NoErr(manager.saveComponentRecord(ctx, initial.ID, state))
		unchanged, err := manager.RegisterCluster(ctx, input)
		assert.NoErr(err)
		assert.Equal(unchanged.Tailscale.CredentialsRef, initial.Tailscale.CredentialsRef)
		rotation := input
		rotation.Tailscale = &ClusterTailscale{ClientID: "replacement-client", ClientSecret: "replacement-secret"}
		_, err = manager.RegisterCluster(ctx, rotation)
		assert.True(errors.Is(err, ErrConflict) && strings.Contains(err.Error(), "reconciliation is active"))
		current, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", "acme/apps")
		assert.NoErr(err)
		assert.Equal(current.Tailscale.CredentialsRef, initial.Tailscale.CredentialsRef)
		files, err := os.ReadDir(manager.runtime.credentialsDir)
		assert.NoErr(err)
		assert.Equal(len(files), 1)
	}
}

// TestClusterTailscaleManagementResume protects accepted tasks from overwriting
// newer rotations after restart; omitted credentials intentionally preserve the latest pair.
func TestClusterTailscaleManagementResume(t *testing.T) {
	for _, supplied := range []bool{true, false} {
		t.Run(map[bool]string{true: "stale-explicit", false: "omitted-preserves-current"}[supplied], func(t *testing.T) {
			assert := is.New(t)
			ctx := context.Background()
			storage := newManagementStorageFixture(t)
			storage.listGate, storage.gateAt = make(chan struct{}), 1
			target := newManagerCluster(t, managerClusterOptions{})
			settings := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementKubeconfigPath: storage.kubeconfig, PlatformSettings: platform.Settings{Profile: "dev", Namespace: "borq"}, PollInterval: time.Millisecond}
			manager, err := NewManager(ctx, settings)
			assert.NoErr(err)
			t.Cleanup(func() { _ = manager.Close() })
			_, err = manager.CreateOrganization(ctx, "acme")
			assert.NoErr(err)
			input := ClusterInput{Organization: "acme", Name: "apps", KubeconfigPath: target.kubeconfig, Tailscale: &ClusterTailscale{ClientID: "initial-client", ClientSecret: "initial-secret"}}
			initial, err := manager.RegisterCluster(ctx, input)
			assert.NoErr(err)
			pending := input
			pending.Tailscale = nil
			if supplied {
				pending.Tailscale = &ClusterTailscale{ClientID: "queued-client", ClientSecret: "queued-secret"}
			}
			operation, err := manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{S3: &ManagementS3Input{Endpoint: storage.s3.URL, Region: "us-east-1", Bucket: "backups", PathStyle: true, AccessKey: "storage-access", SecretKey: "storage-secret"}, Organizations: []ManagementOrganization{{Name: "acme", Clusters: []ClusterInput{pending}}}})
			assert.NoErr(err)
			deadline := time.Now().Add(5 * time.Second)
			blocked := false
			for time.Now().Before(deadline) {
				storage.mutex.Lock()
				blocked = storage.listCount >= 1
				storage.mutex.Unlock()
				if blocked {
					break
				}
				time.Sleep(time.Millisecond)
			}
			assert.True(blocked)
			input.Tailscale = &ClusterTailscale{ClientID: "newest-client", ClientSecret: "newest-secret"}
			latest, err := manager.RegisterCluster(ctx, input)
			assert.NoErr(err)
			assert.True(latest.Tailscale.CredentialsRef != initial.Tailscale.CredentialsRef)
			assert.NoErr(manager.Close())
			close(storage.listGate)
			manager, err = NewManager(ctx, settings)
			assert.NoErr(err)
			status := "succeeded"
			if supplied {
				status = "failed"
			}
			waitManagementResult(t, manager, operation.ID, status)
			result, err := manager.GetManagementOperation(ctx, operation.ID)
			assert.NoErr(err)
			if supplied {
				assert.True(strings.Contains(result.Message, "credentials changed after management configuration was accepted"))
			}
			current, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", "acme/apps")
			assert.NoErr(err)
			assert.Equal(current.Tailscale.CredentialsRef, latest.Tailscale.CredentialsRef)
			task, record, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
			assert.NoErr(err)
			assert.True(!strings.Contains(string(record.Value), "queued-client") && !strings.Contains(string(record.Value), "queued-secret"))
			if supplied {
				assert.Equal(task.ClusterTailscaleBases["acme/apps"], initial.Tailscale.CredentialsRef)
				credentials, err := manager.readClusterTailscale(task.Configuration.Organizations[0].Clusters[0])
				assert.NoErr(err)
				assert.True(credentials.ClientID == "queued-client")
			}
		})
	}
}

// TestClusterTailscaleSelection distinguishes physical convergence from matching
// labels and proves remote operators never borrow the management OAuth pair.
func TestClusterTailscaleSelection(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	management := newManagerCluster(t, managerClusterOptions{})
	remote := newManagerCluster(t, managerClusterOptions{})
	for index, fixture := range []*managerCluster{management, remote} {
		fixture.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": []string{"physical-management", "physical-remote"}[index], "labels": map[string]any{"borq.run/management": "true"}}})
	}
	management.store(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "borq-network-credentials", "namespace": platform.ManagementNamespace}, "data": map[string]any{"tailscaleClientId": base64.StdEncoding.EncodeToString([]byte("management-client")), "tailscaleClientSecret": base64.StdEncoding.EncodeToString([]byte("management-secret"))}})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	manager.runtime.managementKubeconfigPath = management.kubeconfig
	manager.runtime.platformSettings.Network.Tailscale = &platform.TailscaleSettings{}
	settings := manager.runtime.platformSettings
	local := Cluster{Organization: "acme", Name: "local", KubeconfigPath: management.kubeconfig}
	selected, converged, err := manager.clusterPlatformSettings(ctx, local, settings)
	assert.NoErr(err)
	assert.True(converged && selected.Network.Tailscale != nil)
	credentials, err := manager.clusterOperatorCredentials(ctx, local, converged)
	assert.NoErr(err)
	assert.True(credentials.ClientID == "management-client")
	_, err = manager.CreateOrganization(ctx, "acme")
	assert.NoErr(err)
	input := ClusterInput{Organization: "acme", Name: "remote", KubeconfigPath: remote.kubeconfig, Tailscale: &ClusterTailscale{ClientID: "management-client", ClientSecret: "different-secret"}}
	_, err = manager.RegisterCluster(ctx, input)
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "separate Tailscale OAuth application"))
	input.Tailscale = &ClusterTailscale{ClientID: "remote-client", ClientSecret: "remote-secret"}
	cluster, err := manager.RegisterCluster(ctx, input)
	assert.NoErr(err)
	credentials, err = manager.clusterOperatorCredentials(ctx, cluster, false)
	assert.NoErr(err)
	assert.True(credentials.ClientID == "remote-client" && credentials.ClientSecret == "remote-secret")
	settings.Network = platform.NetworkSettings{}
	selected, converged, err = manager.clusterPlatformSettings(ctx, cluster, settings)
	assert.NoErr(err)
	assert.True(!converged && selected.Network.Tailscale != nil && settings.Network.Tailscale == nil)
	cluster.Tailscale = nil
	selected, _, err = manager.clusterPlatformSettings(ctx, cluster, manager.runtime.platformSettings)
	assert.NoErr(err)
	assert.True(selected.Network.Tailscale == nil && manager.runtime.platformSettings.Network.Tailscale != nil)
	settings = manager.runtime.platformSettings
	settings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.com"}
	selected, _, err = manager.clusterPlatformSettings(ctx, cluster, settings)
	assert.NoErr(err)
	assert.True(selected.Network.Cloudflare == nil && settings.Network.Cloudflare != nil)
	// Namespace UIDs are API-supplied data; copying management's UID grants no authority.
	remote.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "physical-management"}})
	selected, converged, err = manager.clusterPlatformSettings(ctx, cluster, settings)
	assert.NoErr(err)
	if converged || selected.Network.Tailscale != nil || selected.Network.Cloudflare != nil {
		t.Fatal("untrusted API with copied management UID inherited management network authority")
	}
	_, err = manager.clusterOperatorCredentials(ctx, cluster, converged)
	assert.True(errors.Is(err, ErrInvalid))

	manager.runtime.managementBootstrapOnly = true
	settings = platform.Settings{Namespace: "borq-mgmt"}
	selected, converged, err = manager.clusterPlatformSettings(ctx, local, settings)
	assert.NoErr(err)
	assert.True(converged && selected.Network.Tailscale == nil)
	assert.Equal(selected.OpenBaoURL, "http://openbao.borq-mgmt.svc:8200")
	assert.Equal(selected.TelemetryURL, "http://borq.borq-mgmt.svc:80")
	assert.Equal(selected.S3Endpoint, "http://rustfs-svc.borq-mgmt.svc:9000")
	selected, converged, err = manager.clusterPlatformSettings(ctx, cluster, settings)
	assert.NoErr(err)
	assert.True(!converged && selected.OpenBaoURL == "" && selected.TelemetryURL == "" && selected.S3Endpoint == "")
	settings.OpenBaoURL, settings.TelemetryURL, settings.S3Endpoint = "https://vault.example.test", "https://ingest.example.test", "https://s3.example.test"
	selected, converged, err = manager.clusterPlatformSettings(ctx, local, settings)
	assert.NoErr(err)
	assert.True(converged)
	assert.Equal(selected.OpenBaoURL, settings.OpenBaoURL)
	assert.Equal(selected.TelemetryURL, settings.TelemetryURL)
	assert.Equal(selected.S3Endpoint, settings.S3Endpoint)
}
