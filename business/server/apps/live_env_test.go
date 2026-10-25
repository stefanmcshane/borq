package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestLiveEnvironmentPlanRetainsIdentityWithoutCredentials owns live-plan resolution and readback.
func TestLiveEnvironmentPlanRetainsIdentityWithoutCredentials(t *testing.T) {
	check := is.New(t)
	manager, cluster, vault, target := newEnvironmentGroupManager(t)
	group := environmentGroupRecord{SpaceID: target.ID, Name: "postgres-orders", PostgresID: "database-incarnation", PostgresName: "orders"}
	write, err := documentWrite("environment-group", target.ID+"/"+group.Name, group, 0)
	check.NoErr(err)
	check.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	vault.mutex.Lock()
	vault.versions[environmentGroupPath(target.ID, group.Name)] = []map[string]string{{"PGPASSWORD": "do-not-save-live-credential"}}
	vault.mutex.Unlock()
	client, err := newClusterClient(Cluster{KubeconfigPath: cluster.kubeconfig})
	check.NoErr(err)
	patch := patchFromJson(t, `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"postgres-orders","version":"latest"}}]}`)
	configuration, entries, _, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: target}, patch)
	check.NoErr(err)
	check.Equal(configuration.Apps[0].Env.Version, "latest")
	check.Equal(entries[0].Snapshot.PostgresID, group.PostgresID)
	encoded, err := json.Marshal(entries)
	check.NoErr(err)
	check.True(!strings.Contains(string(encoded), "do-not-save-live-credential"))
	rendered, err := RenderWithEnv(configuration, target.Namespace, target.ID, "live-plan", "borq", envSnapshots(entries), ResourceScope{})
	check.NoErr(err)
	resource := envExternalSecret(t, rendered)
	check.Equal(manifestText(resource, "spec", "refreshPolicy"), "Periodic")
	check.Equal(manifestText(resource, "spec", "target", "template", "metadata", "annotations", "borq.run/env-version"), "latest")
	check.Equal(resource["spec"].(map[string]any)["target"].(map[string]any)["immutable"], false)
	inspected, err := Inspect(rendered.Manifests)
	check.NoErr(err)
	check.Equal(inspected, configuration)
	check.NoErr(manager.verifyEnvironment(t.Context(), client, entries))
	for _, version := range []string{"1", "0"} {
		patch.Apps = &[]AppPatch{{Name: "backend", Env: &EnvReference{Name: group.Name, Version: version}}}
		_, _, _, err := manager.prepareEnvironment(t.Context(), client, spaceRecord{Space: target}, patch)
		check.True(errors.Is(err, ErrInvalid))
	}
	old := entries
	old[0].Snapshot.PostgresID = ""
	old[0].Snapshot.Version = 1
	check.True(errors.Is(manager.verifyEnvironment(t.Context(), client, old), ErrConflict))
}

// TestLiveEnvironmentRejectsLiteralCredentialSnapshots keeps caller-supplied plans from persisting credentials.
func TestLiveEnvironmentRejectsLiteralCredentialSnapshots(t *testing.T) {
	check := is.New(t)
	configuration, rendered := renderEnvFixture(t)
	configuration.Apps[0].Env.Version = "latest"
	snapshot := rendered.EnvSnapshots[0]
	snapshot.PostgresID, snapshot.Version, snapshot.SnapshotID = "producer", 0, "prod/backend-env/producer"
	_, err := RenderWithEnv(configuration, "shop", "prod", "plan", "borq", []EnvSnapshot{snapshot}, ResourceScope{})
	check.True(err != nil)
}

// TestLiveEnvironmentReadbackRejectsPinnedOrMismatchedProducer prevents manifests from weakening a live binding.
func TestLiveEnvironmentReadbackRejectsPinnedOrMismatchedProducer(t *testing.T) {
	for _, change := range []struct {
		name  string
		path  []string
		value any
	}{
		{"immutable", []string{"spec", "target", "immutable"}, true},
		{"manual refresh", []string{"spec", "refreshPolicy"}, "OnChange"},
		{"different producer", []string{"spec", "target", "template", "metadata", "annotations", "borq.run/postgres-id"}, "another-database"},
		{"pinned identity", []string{"spec", "target", "template", "metadata", "annotations", "borq.run/env-version"}, "1"},
	} {
		t.Run(change.name, func(t *testing.T) {
			check := is.New(t)
			configuration, rendered := renderEnvFixture(t)
			configuration.Apps[0].Env.Version = "latest"
			snapshot := rendered.EnvSnapshots[0]
			snapshot.PostgresID, snapshot.Version, snapshot.SnapshotID = "producer", 0, "prod/backend-env/producer"
			for i := range snapshot.Values {
				snapshot.Values[i].Value = ""
			}
			rendered, err := RenderWithEnv(configuration, "shop", "prod", "plan", "borq", []EnvSnapshot{snapshot}, ResourceScope{})
			check.NoErr(err)
			setManifestField(t, envExternalSecret(t, rendered), change.value, change.path...)
			_, err = Inspect(rendered.Manifests)
			check.True(err != nil)
		})
	}
}
