package apps

import (
	"errors"
	"net/http"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"testing"

	"github.com/matryer/is"
)

// TestVerifyEnvironmentFailsUnavailableOrChangedPinnedVersion detects a pruned
// version or altered storage before any dependent deployment resource changes.
func TestVerifyEnvironmentFailsUnavailableOrChangedPinnedVersion(t *testing.T) {
	assert := is.New(t)
	manager, cluster, vault, environment := newEnvironmentGroupManager(t)
	_, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: environmentGroupTestTarget(), Name: "backend", Values: []Variable{{Name: "XX", Value: "reviewed"}}})
	assert.NoErr(err)
	client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	_, entries, _, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: environment}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend"}}]}`))
	assert.NoErr(err)
	entries[0].Snapshot.Values = []Variable{{Name: "XX", Value: "different"}}
	assert.True(verifyEnvironment(t.Context(), client, entries) != nil)
	entries[0].Snapshot.Version = 42
	assert.True(verifyEnvironment(t.Context(), client, entries) != nil)
	assert.Equal(vault.writes(), 1)
}

// TestPrepareEnvironmentDiscoversManualDefaultConfig treats the vault as the
// source of truth even when the default family was never created through Borq.
func TestPrepareEnvironmentDiscoversManualDefaultConfig(t *testing.T) {
	assert := is.New(t)
	manager, cluster, vault, environment := newEnvironmentGroupManager(t)
	name := "acme-production-backend"
	vault.mutex.Lock()
	vault.versions[environmentGroupPath(environment.ID, name)] = []map[string]string{{"MANUAL": "published-outside-borq"}}
	vault.mutex.Unlock()
	client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	configuration, entries, selectors, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: environment}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1"}]}`))
	assert.NoErr(err)
	assert.Equal(configuration.Apps[0].Env, EnvReference{Name: name, Version: "1"})
	assert.Equal(entries[0].Snapshot.Values, []Variable{{Name: "MANUAL", Value: "published-outside-borq"}})
	assert.Equal(selectors["backend"], EnvReference{Version: "latest"})
	assert.Equal(vault.writes(), 0)
	_, _, err = loadDocument[environmentGroupRecord](t.Context(), manager.database, "environment-group", environment.ID+"/"+name)
	assert.True(errors.Is(err, ErrNotFound))
}

// TestPrepareEnvironmentOnlyIgnoresAbsentUnusedStore keeps minimal apps usable
// without ESO while surfacing broken credentials, access, and known config stores.
func TestPrepareEnvironmentOnlyIgnoresAbsentUnusedStore(t *testing.T) {
	for _, test := range []struct {
		name         string
		missingStore bool
		knownConfig  bool
		missingToken bool
		forbidStore  bool
		wantEmpty    bool
	}{
		{name: "absent unused store", missingStore: true, wantEmpty: true},
		{name: "absent known config store", missingStore: true, knownConfig: true},
		{name: "missing token", missingToken: true},
		{name: "forbidden store", forbidStore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			manager, cluster, _, environment := newEnvironmentGroupManager(t)
			if test.knownConfig {
				assert.NoErr(manager.registerEnvironmentGroup(t.Context(), environment.ID, "acme-production-backend"))
			}
			cluster.mutex.Lock()
			if test.missingStore {
				delete(cluster.resources, "/apis/external-secrets.io/v1/clustersecretstores/borq-openbao")
			}
			if test.missingToken {
				delete(cluster.resources, "/api/v1/namespaces/borq-system/secrets/openbao-token")
			}
			cluster.mutex.Unlock()
			if test.forbidStore {
				previous := cluster.server.Config.Handler
				cluster.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/apis/external-secrets.io/v1/clustersecretstores/borq-openbao" {
						managerStatus(writer, http.StatusForbidden, "Forbidden", "store access denied")
						return
					}
					previous.ServeHTTP(writer, request)
				})
			}
			client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
			assert.NoErr(err)
			configuration, entries, _, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: environment}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1"}]}`))
			if test.wantEmpty {
				assert.NoErr(err)
				assert.Equal(configuration.Apps[0].Env.Version, "0")
				assert.Equal(len(entries), 0)
				return
			}
			assert.True(err != nil)
			if test.missingStore {
				assert.True(errors.Is(err, kubeapps.ErrSecretStoreNotFound))
			} else {
				assert.True(!errors.Is(err, kubeapps.ErrSecretStoreNotFound))
			}
		})
	}
}

// TestPrepareEnvironmentRequiresPinnedDefaultWithMissingStore refuses to
// downgrade an explicitly selected default config version to empty version zero.
func TestPrepareEnvironmentRequiresPinnedDefaultWithMissingStore(t *testing.T) {
	assert := is.New(t)
	manager, cluster, _, environment := newEnvironmentGroupManager(t)
	cluster.mutex.Lock()
	delete(cluster.resources, "/apis/external-secrets.io/v1/clustersecretstores/borq-openbao")
	cluster.mutex.Unlock()
	client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
	assert.NoErr(err)
	configuration, entries, _, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: environment}, patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":{"version":"3"}}]}`))
	assert.True(errors.Is(err, kubeapps.ErrSecretStoreNotFound))
	assert.Equal(configuration, Configuration{})
	assert.Equal(len(entries), 0)
}
