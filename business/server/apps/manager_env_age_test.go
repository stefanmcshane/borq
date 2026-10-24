package apps

import (
	"errors"
	"strconv"
	"testing"

	"github.com/matryer/is"
)

// TestManagerRejectsAgedEnvPlanBeforeQueue enforces the published-version window
// while planning and prevents a saved version aging out before apply from
// consuming a revision or a FIFO queue position, even when old values remain.
func TestManagerRejectsAgedEnvPlanBeforeQueue(t *testing.T) {
	assert := is.New(t)
	manager, cluster, vault, environment := newEnvironmentGroupManager(t)
	seedEnvironmentGroupVersions(vault, environment.ID, "backend", 20)
	oldest, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend","version":"10"}}]}`))
	assert.NoErr(err)
	assert.Equal(oldest.Configuration.Apps[0].Env.Version, "10")
	latest, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend"}}]}`))
	assert.NoErr(err)
	assert.Equal(latest.Configuration.Apps[0].Env.Version, "20")
	for _, test := range []struct {
		version string
		want    error
	}{{"9", ErrInvalid}, {"21", ErrNotFound}} {
		_, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend","version":"`+test.version+`"}}]}`))
		assert.True(errors.Is(err, test.want))
	}
	// A historical read succeeds: deployment rejection is policy, not pruning.
	old, err := manager.GetEnvironmentGroup(t.Context(), environmentGroupTestTarget(), "backend", "9")
	assert.NoErr(err)
	assert.Equal(old.Version, 9)
	seedEnvironmentGroupVersions(vault, environment.ID, "backend", 21)
	_, err = manager.Apply(t.Context(), oldest.ID)
	assert.True(errors.Is(err, ErrInvalid))
	unchanged, err := manager.GetSpace(t.Context(), "acme", "production")
	assert.NoErr(err)
	assert.Equal(unchanged.Revision, 0)
	stored, _, err := loadDocument[spaceRecord](t.Context(), manager.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	assert.Equal(stored.ActiveOperation, "")
	assert.Equal(len(stored.Queue), 0)
	_, err = manager.GetOperation(t.Context(), oldest.ID)
	assert.True(errors.Is(err, ErrNotFound))
	eligible, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend","version":"11"}}]}`))
	assert.NoErr(err)
	assert.Equal(eligible.Configuration.Apps[0].Env.Version, "11")
	assert.Equal(cluster.namespaceAttempts(), 0)
	assert.Equal(vault.writes(), 0)
}

func seedEnvironmentGroupVersions(vault *environmentGroupVault, environmentID, name string, count int) {
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	versions := make([]map[string]string, count)
	for index := range versions {
		versions[index] = map[string]string{"VERSION": strconv.Itoa(index + 1)}
	}
	vault.versions[environmentGroupPath(environmentID, name)] = versions
}
