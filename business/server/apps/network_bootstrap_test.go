package apps

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestNetworkNamesBeforeStaging prevents invalid DNS authority from publishing secrets or accepting work.
func TestNetworkNamesBeforeStaging(t *testing.T) {
	assert := is.New(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: "/must-not-be-read",
		PlatformSettings: platform.Settings{Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, Cloudflare: &platform.CloudflareSettings{WildcardDomain: "example.test"}}}})
	assert.NoErr(err)
	defer manager.Close()
	cluster := newManagerCluster(t, managerClusterOptions{})
	write, err := documentWrite("cluster", "team/target", Cluster{ID: platform.ClusterID("team", "target"), Organization: "team", Name: "target", KubeconfigPath: cluster.kubeconfig}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	space := initialSpace("team", "production")
	space.Cluster = "target"
	_, _, _, err = manager.prepareEnvironment(t.Context(), kubeapps.Client{}, spaceRecord{Space: space}, patchFromJson(t, `{"apps":[{"name":"web","image":"web:v1","port":8080,"ingresses":[{"name":"public","host":"example.test"}]}]}`))
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(ApplicationRequestMessage(err), "ClusterIssuer borq"))
	_, err = manager.SetupOrganizationPocketId(t.Context(), "team", OrganizationPocketIdInput{IssuerURL: "https://id.management.example.test", BaseURL: "https://borq.example.test", AdminEmail: "admin@example.test", CertificateIssuer: "cloudflare"})
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "one name below example.test"))
	configuration := managementOidcManagedDeclaration()
	configuration.Organizations[0].ManagedPocketId.IssuerURL = "https://id.management.example.test"
	_, err = manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.True(errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "one name below example.test"))
	for _, kind := range []string{"management-operation", "organization", "organization-pocketid", "environment-group"} {
		records, err := manager.database.List(t.Context(), kind)
		assert.NoErr(err)
		assert.Equal(len(records), 0)
	}
	assert.NoErr(manager.validateNetworkHostname("*."+platform.ClusterID("team", "target")+".example.test", platform.ClusterID("team", "target")))
	assert.NoErr(manager.validateNetworkPocketIdName("https://id.example.test"))
	assert.NoErr(manager.validateNetworkPocketIdName("https://id.unmanaged.test"))
}

// TestNetworkRestartRefreshesLinkedControllers verifies durable ready targets are requeued after credential rotation, preserving disabled targets.
func TestNetworkRestartRefreshesLinkedControllers(t *testing.T) {
	assert := is.New(t)
	target := newComponentTarget(t)
	input := componentManagerInput(t, target.kubeconfig)
	input.PlatformSettings.Network = platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}, Cloudflare: &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "admin@example.test", WebhookImage: "borq:test"}}
	manager, err := NewManager(t.Context(), input)
	assert.NoErr(err)
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	disabled := false
	for _, name := range []string{"ready", "disabled"} {
		cluster, err := manager.RegisterCluster(t.Context(), ClusterInput{Organization: "team", Name: name, KubeconfigPath: target.kubeconfig, Reconcile: &disabled})
		assert.NoErr(err)
		if name == "ready" {
			assert.NoErr(manager.saveComponentRecord(t.Context(), cluster.ID, componentRecord{Installation: ComponentInstallation{ClusterID: cluster.ID, ID: "previous", Status: "ready"}}))
		}
	}
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	defer manager.Close()
	assert.NoErr(manager.recoverComponentRuns(t.Context()))
	ready, _, err := loadDocument[componentRecord](t.Context(), manager.database, "components", platform.ClusterID("team", "ready"))
	assert.NoErr(err)
	assert.Equal(ready.Installation.Status, "queued")
	assert.True(ready.Installation.ID != "previous")
	assert.True(!ready.Upgrade)
	untouched, _, err := loadDocument[componentRecord](t.Context(), manager.database, "components", platform.ClusterID("team", "disabled"))
	assert.NoErr(err)
	assert.Equal(untouched.Installation.Status, "disabled")
	assert.Equal(target.writes.Load(), int64(0))
}
