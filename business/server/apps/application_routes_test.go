package apps

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestApplicationRoutePlanScope owns validation before staging or accepting an
// application plan; model rendering does not establish issuer permissions.
func TestApplicationRoutePlanScope(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	policy, err := manager.GetApplicationRoutePolicy(t.Context(), "acme", "production", "web")
	assert.NoErr(err)
	assert.Equal(policy.CertificateIssuer, "borq")
	assert.Equal(policy.Domains, []string{"borq.io", "example.com", "example.test"})
	reviewed, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("web.borq.io")))
	assert.NoErr(err)
	for _, hostname := range []string{"web.borq.io", "web.other.test", "deep.web.borq.io", "*.borq.io", "borq.io"} {
		t.Run(hostname, func(t *testing.T) {
			assert := is.New(t)
			_, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch(hostname)))
			if hostname == "web.borq.io" {
				assert.NoErr(err)
				return
			}
			assert.True(errors.Is(err, ErrInvalid))
			assert.True(strings.Contains(ApplicationRequestMessage(err), "ClusterIssuer borq"))
		})
	}
	cluster.mutex.Lock()
	delete(cluster.resources, "/apis/cert-manager.io/v1/clusterissuers/borq")
	cluster.mutex.Unlock()
	policy, err = manager.GetApplicationRoutePolicy(t.Context(), "acme", "production", "web")
	assert.NoErr(err)
	assert.Equal(policy.CertificateIssuer, "borq")
	assert.Equal(policy.Domains, []string{})
	_, err = manager.Apply(t.Context(), reviewed.ID)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "ClusterIssuer borq"))
	_, err = manager.GetOperation(t.Context(), reviewed.ID)
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"private","image":"traefik/whoami:v1.11.0","port":80}]}`))
	assert.NoErr(err)
	space, err := manager.GetSpace(t.Context(), "acme", "production")
	assert.NoErr(err)
	assert.Equal(space.Revision, 0)
	assert.Equal(len(space.Configuration.Apps), 0)
	assert.Equal(cluster.namespaceAttempts(), 0)
}

// TestApplicationRouteClusterAuthority keeps remote issuer edits inside the
// delegated cluster subtree while permitting trusted same-cluster public names.
func TestApplicationRouteClusterAuthority(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			assert := is.New(t)
			cluster := newManagerCluster(t, managerClusterOptions{})
			cluster.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "physical-cluster-identity"}})
			input := NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PlatformSettings: platform.Settings{Network: platform.NetworkSettings{Cloudflare: &platform.CloudflareSettings{WildcardDomain: "borq.io"}}}}
			if trusted {
				input.ManagementKubeconfigPath = cluster.kubeconfig
			}
			manager, err := NewManager(t.Context(), input)
			assert.NoErr(err)
			defer manager.Close()
			space := createManagerEnvironment(t, manager, cluster, "production")
			policy, err := manager.GetApplicationRoutePolicy(t.Context(), "acme", "production", "web")
			assert.NoErr(err)
			root := "borq.io"
			if !trusted {
				root = platform.ClusterID("acme", "local") + ".borq.io"
			}
			assert.Equal(policy.Domains, []string{root, "example.com", "example.test"})
			_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("web."+root)))
			assert.NoErr(err)
			if trusted {
				return
			}
			for _, hostname := range []string{"web.borq.io", "web." + platform.ClusterID("other", "target") + ".borq.io"} {
				_, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch(hostname)))
				assert.True(errors.Is(err, ErrInvalid))
				assert.True(strings.Contains(ApplicationRequestMessage(err), root))
			}
			// An ancestor zone must not grant a remote cluster the management apex.
			cluster.mutex.Lock()
			cluster.resources["/apis/cert-manager.io/v1/clusterissuers/"+space.CertificateIssuer] = map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer", "metadata": map[string]any{"name": space.CertificateIssuer},
				"spec": map[string]any{"acme": map[string]any{"solvers": []any{map[string]any{"selector": map[string]any{"dnsZones": []any{"io"}}, "dns01": map[string]any{"cloudflare": map[string]any{}}}}}}}
			cluster.mutex.Unlock()
			_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("borq.io")))
			assert.True(errors.Is(err, ErrInvalid))
			if !strings.Contains(ApplicationRequestMessage(err), "remote cluster") {
				t.Fatalf("expected DNS authority rejection, got %v", err)
			}
			unchanged, err := manager.GetSpace(t.Context(), space.Organization, space.Name)
			assert.NoErr(err)
			assert.Equal(unchanged.Revision, 0)
		})
	}
}

// TestApplicationRouteAcceptanceRechecksOwnership prevents two already reviewed
// plans in separate spaces from accepting the same public hostname.
func TestApplicationRouteAcceptanceRechecksOwnership(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: cluster.kubeconfig, RolloutTimeout: time.Second, PollInterval: 5 * time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	createManagerEnvironment(t, manager, cluster, "one")
	_, err = manager.CreateSpace(t.Context(), SpaceInput{Organization: "acme", Name: "two", Cluster: "local"})
	assert.NoErr(err)
	first, err := manager.Plan(t.Context(), managerPlanInput(t, "one", applicationRouteTestPatch("web.borq.io")))
	assert.NoErr(err)
	second, err := manager.Plan(t.Context(), managerPlanInput(t, "two", applicationRouteTestPatch("web.borq.io")))
	assert.NoErr(err)
	accepted, err := manager.Apply(t.Context(), first.ID)
	assert.NoErr(err)
	assert.Equal(accepted.PlanID, first.ID)
	_, err = manager.Apply(t.Context(), second.ID)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "another space"))
	other, err := manager.GetSpace(t.Context(), "acme", "two")
	assert.NoErr(err)
	assert.Equal(other.Revision, 0)
	_, err = manager.GetOperation(t.Context(), second.ID)
	assert.True(errors.Is(err, ErrNotFound))
	waitManagerOperation(t, manager, first.ID, "failed")
	_, err = manager.SetupOrganizationPocketId(t.Context(), "acme", OrganizationPocketIdInput{IssuerURL: "https://web.borq.io", BaseURL: "https://control.borq.io", CertificateIssuer: "borq", AdminEmail: "admin@example.test"})
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "another space"))
	_, err = manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{BorqPublicURL: "https://web.borq.io"})
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "another space"))
}

// TestApplicationRouteProtectedHostnames keeps existing and pending management
// identities reserved before application staging, including task-only addresses.
func TestApplicationRouteProtectedHostnames(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	for _, record := range []struct {
		collection string
		key        string
		value      any
	}{
		{"management-network-settings", "shared", managementNetworkConfiguration{BorqPublicURL: "https://control.borq.io"}},
		{"organization-pocketid", "existing", organizationPocketIdRecord{Status: OrganizationPocketId{IssuerURL: "https://id.borq.io"}}},
		{"management-operation", "pending", Operation{ID: "pending", Status: "queued"}},
		{"management-task", "pending", managementTask{Hostnames: []string{"pending.borq.io"}, Configuration: ManagementConfiguration{BorqPublicURL: "https://future.borq.io", Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: "https://next-id.borq.io"}}}}}},
	} {
		write, err := documentWrite(record.collection, record.key, record.value, 0)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	}
	for hostname, message := range map[string]string{"control.borq.io": "Borq server", "id.borq.io": "Pocket ID", "pending.borq.io": "management setup", "future.borq.io": "management setup", "next-id.borq.io": "management setup"} {
		t.Run(hostname, func(t *testing.T) {
			assert := is.New(t)
			_, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch(hostname)))
			assert.True(errors.Is(err, ErrInvalid))
			assert.True(strings.Contains(ApplicationRequestMessage(err), message))
		})
	}
	assert.Equal(cluster.namespaceAttempts(), 0)
}

// TestApplicationRouteSpacePocketIdOwnership protects a queued Space provider
// both when planning a route and when accepting a plan reviewed before setup.
func TestApplicationRouteSpacePocketIdOwnership(t *testing.T) {
	assert := is.New(t)
	storage := newManagementStorageFixture(t)
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: storage.kubeconfig,
		PlatformSettings: platform.Settings{Namespace: "borq", Profile: "dev", OpenBaoURL: "http://vault.test"}})
	assert.NoErr(err)
	defer func() { assert.NoErr(manager.Close()) }()
	cluster := newManagerCluster(t, managerClusterOptions{})
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("id.example.test")))
	assert.NoErr(err)
	provider, err := manager.PutPocketId(t.Context(), "acme", "production", "identity", PocketIdInput{IssuerURL: "https://id.example.test", AdminEmail: "admin@example.test"}, false)
	assert.NoErr(err)
	assert.Equal(provider.State, "pending")
	_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("id.example.test")))
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "reserved for Pocket ID"))
	_, err = manager.Apply(t.Context(), plan.ID)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(ApplicationRequestMessage(err), "reserved for Pocket ID"))
	unchanged, err := manager.GetSpace(t.Context(), "acme", "production")
	assert.NoErr(err)
	assert.Equal(unchanged.Revision, space.Revision)
	_, err = manager.GetOperation(t.Context(), plan.ID)
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("other.example.test")))
	assert.NoErr(err)
}

// TestApplicationRouteHistoricalOwnership interprets accepted operation history
// from storage without fabricating a certificate or deployment controller result.
func TestApplicationRouteHistoricalOwnership(t *testing.T) {
	for name, test := range map[string]struct {
		status                 string
		lastSuccessfulRevision int
		reserved               bool
	}{
		"failed accepted revision":         {"failed", 4, true},
		"unfinished accepted revision":     {"applying", 4, true},
		"later failed replacement":         {"failed", 3, true},
		"later successful replacement":     {"failed", 6, false},
		"unfinished survives later record": {"applying", 6, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			cluster := newManagerCluster(t, managerClusterOptions{})
			manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
			owner := createManagerEnvironment(t, manager, cluster, "owner")
			_, err := manager.CreateSpace(t.Context(), SpaceInput{Organization: "acme", Name: "requester", Cluster: "local"})
			assert.NoErr(err)
			owner.Revision, owner.LastSuccessfulRevision = 6, test.lastSuccessfulRevision
			stored, record, err := loadDocument[spaceRecord](t.Context(), manager.database, "space", "acme/owner")
			assert.NoErr(err)
			stored.Space = owner
			configuration, err := Merge(Configuration{Apps: []App{}}, patchFromJson(t, applicationRouteTestPatch("old.borq.io")))
			assert.NoErr(err)
			for _, entry := range []struct {
				collection string
				key        string
				value      any
				revision   int64
			}{
				{"space", "acme/owner", stored, record.Revision},
				{"plan", "historical", planRecord{Plan: Plan{ID: "historical", Space: owner, BaseRevision: 4, Configuration: configuration}}, 0},
				{"operation", "historical", Operation{ID: "historical", PlanID: "historical", SpaceID: owner.ID, Status: test.status}, 0},
			} {
				write, err := documentWrite(entry.collection, entry.key, entry.value, entry.revision)
				assert.NoErr(err)
				assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			}
			_, err = manager.Plan(t.Context(), managerPlanInput(t, "requester", applicationRouteTestPatch("old.borq.io")))
			if !test.reserved {
				assert.NoErr(err)
				return
			}
			assert.True(errors.Is(err, ErrInvalid))
			assert.True(strings.Contains(ApplicationRequestMessage(err), "deployed or unfinished revision"))
		})
	}
}

func applicationRouteTestPatch(hostname string) string {
	return fmt.Sprintf(`{"apps":[{"name":"web","image":"traefik/whoami:v1.11.0","port":80,"ingresses":[{"name":"public","host":%q,"path":"/"}]}]}`, hostname)
}

// TestApplicationRouteWildcardOwnership reserves every Gateway wildcard suffix,
// including deeper hostnames supported by Gateway API matching.
func TestApplicationRouteWildcardOwnership(t *testing.T) {
	for _, names := range [][2]string{
		{"*.example.com", "web.team.example.com"},
		{"web.team.example.com", "*.example.com"},
		{"*.team.example.com", "*.example.com"},
	} {
		assert := is.New(t)
		assert.True(applicationHostnameOverlap(names[0], names[1]))
	}
	assert := is.New(t)
	assert.True(!applicationHostnameOverlap("*.example.com", "example.com"))
	assert.True(!applicationHostnameOverlap("*.example.com", "web.other-example.com"))
}

// TestApplicationRouteDeletionWithoutIssuer keeps cleanup available after the
// issuer disappears, even when empty deletion values lack the tunnel snapshot.
func TestApplicationRouteDeletionWithoutIssuer(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{forbidNamespace: true})
	cluster.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "management-physical-cluster"}})
	manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: cluster.kubeconfig,
		PlatformSettings: platform.Settings{Network: platform.NetworkSettings{Cloudflare: &platform.CloudflareSettings{Exposure: "tunnel", WildcardDomain: "borq.io"}}}, RolloutTimeout: time.Second, PollInterval: 5 * time.Millisecond})
	assert.NoErr(err)
	defer manager.Close()
	space := createManagerEnvironment(t, manager, cluster, "production")
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", applicationRouteTestPatch("web.borq.io")))
	assert.NoErr(err)
	_, err = manager.Apply(t.Context(), plan.ID)
	assert.NoErr(err)
	waitManagerOperation(t, manager, plan.ID, "failed")
	cluster.mutex.Lock()
	delete(cluster.resources, "/apis/cert-manager.io/v1/clusterissuers/"+space.CertificateIssuer)
	cluster.mutex.Unlock()
	operation, err := manager.DeleteApp(t.Context(), "acme", "production", "web")
	assert.NoErr(err)
	assert.Equal(operation.Phase, "deleting")
}
