package apps

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestProductionPostgresStorageScope permits management Service DNS only after
// server-owned connection authority and physical identity establish co-location.
// A remote API echoing the same namespace UID cannot claim that authority.
func TestProductionPostgresStorageScope(t *testing.T) {
	for _, name := range []string{"same cluster", "remote copied identity", "different context", "missing physical identity", "loopback", "other service", "public storage"} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			management := newManagerCluster(t, managerClusterOptions{})
			management.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "management-identity"}})
			manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: management.kubeconfig, PlatformSettings: platform.Settings{Profile: "production", Namespace: "borq"}})
			assert.NoErr(err)
			defer manager.Close()
			cluster := Cluster{Organization: "team", Name: "apps", KubeconfigPath: management.kubeconfig}
			storage := ManagementS3{Endpoint: "http://rustfs-svc.borq.svc:9000", CredentialsRef: "borq/management/s3/" + strings.Repeat("a", 32)}
			switch name {
			case "remote copied identity":
				remote := newManagerCluster(t, managerClusterOptions{})
				remote.store(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "management-identity"}})
				cluster.KubeconfigPath = remote.kubeconfig
			case "different context":
				cluster.Context = "unverified"
			case "missing physical identity":
				management.mutex.Lock()
				delete(management.resources, "/api/v1/namespaces/kube-system")
				management.mutex.Unlock()
			case "loopback":
				storage.Endpoint = "http://127.0.0.1:9000"
			case "other service":
				storage.Endpoint = "http://rustfs-svc.other.svc:9000"
			case "public storage":
				storage.Endpoint = "https://primary-s3.example.test"
			}
			write, err := documentWrite("cluster", "team/apps", cluster, 0)
			assert.NoErr(err)
			assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
			err = manager.validatePostgresStorage(t.Context(), storage, Space{Organization: "team", Cluster: "apps"})
			if name == "same cluster" || name == "public storage" {
				assert.NoErr(err)
				return
			}
			assert.True(err != nil)
			if name != "missing physical identity" {
				assert.True(errors.Is(err, ErrInvalid))
			} else {
				assert.True(strings.Contains(err.Error(), "physical cluster identity unavailable"))
			}
		})
	}
}
