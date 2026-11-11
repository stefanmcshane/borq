package apps

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestAddonValidation protects pinned public chart identities and Kubernetes selectors before network work.
func TestAddonValidation(t *testing.T) {
	for _, input := range []struct {
		name    string
		change  func(AddonInput) AddonInput
		message string
	}{
		{"http", func(value AddonInput) AddonInput { return value }, ""},
		{"oci", func(value AddonInput) AddonInput {
			value.Repository = "oci://registry.example.com/charts"
			return value
		}, ""},
		{"credentials", func(value AddonInput) AddonInput {
			value.Repository = "https://user:password@example.com/charts"
			return value
		}, "without credentials"},
		{"file", func(value AddonInput) AddonInput { value.Repository = "file:///tmp/chart"; return value }, "repository must"},
		{"chart path", func(value AddonInput) AddonInput { value.Chart = "../other"; return value }, "chart must"},
		{"latest", func(value AddonInput) AddonInput { value.Version = "latest"; return value }, "exact chart version"},
		{"range", func(value AddonInput) AddonInput { value.Version = ">=1.2.3"; return value }, "exact chart version"},
		{"selector", func(value AddonInput) AddonInput {
			value.PodSelector = map[string]string{"invalid/key/label": "value"}
			return value
		}, "valid Kubernetes labels"},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			err := validateAddonInput(input.change(AddonInput{Repository: "https://charts.example.com", Chart: "service", Version: "1.2.3"}))
			if input.message == "" {
				assert.NoErr(err)
				return
			}
			assert.True(errors.Is(err, ErrInvalid))
			assert.True(strings.Contains(err.Error(), input.message))
		})
	}
}

// TestAddonFailedOperationPersists proves failed installs retain accepted desired state
// through a real database close/reopen. Reinstalling cannot overwrite that state;
// an explicit apply can replace it.
func TestAddonFailedOperationPersists(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	cluster := newManagerCluster(t, managerClusterOptions{})
	path := filepath.Join(t.TempDir(), "addons.sqlite")
	manager := openTestManager(t, path, time.Second)
	createManagerEnvironment(t, manager, cluster, "production")
	// Close the HTTP fixture before installation to exercise a real connection failure,
	// without fabricating Helm/controller results or downloading an unreachable chart.
	cluster.server.Close()
	input := AddonInput{Repository: "https://charts.example.com", Chart: "service", Version: "1.2.3", Values: map[string]any{"message": "saved override"}}
	accepted, err := manager.PutAddon(ctx, "acme", "production", "shared", input, false)
	assert.NoErr(err)
	assert.Equal(accepted.State, "pending")
	assert.Equal(accepted.ReleaseName, "addon-shared")
	assert.True(accepted.Operation != "")
	deadline := time.Now().Add(5 * time.Second)
	var operation Operation
	for time.Now().Before(deadline) {
		operation, err = manager.GetAddonOperation(ctx, accepted.Operation)
		assert.NoErr(err)
		if operation.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(operation.Status, "failed")
	assert.True(operation.Message != "")
	assert.NoErr(manager.Close())
	reopened := openTestManager(t, path, time.Second)
	persisted, err := reopened.GetAddon(ctx, "acme", "production", "shared")
	assert.NoErr(err)
	assert.Equal(persisted.State, "failed")
	assert.Equal(persisted.Specification.Values["message"], "saved override")
	assert.Equal(persisted.Operation, accepted.Operation)
	savedOperation, err := reopened.GetAddonOperation(ctx, accepted.Operation)
	assert.NoErr(err)
	assert.Equal(savedOperation, operation)
	history, err := reopened.GetAddonEvents(ctx, TelemetryInput{Organization: "acme", Space: "production", Application: "shared"})
	assert.NoErr(err)
	assert.Equal(len(history.Events), 2)
	assert.Equal(history.Events[0].Kind, "deployment")
	assert.Equal(history.Events[1].Kind, "failed")
	assert.Equal(history.Events[1].Deployment, accepted.Operation)
	assert.True(strings.Contains(history.EventsNotice, "Kubernetes lifecycle history is unavailable"))
	_, err = reopened.GetAddonEvents(ctx, TelemetryInput{Organization: "acme", Space: "other", Application: "shared"})
	assert.True(errors.Is(err, ErrNotFound))
	_, err = reopened.GetAddon(ctx, "acme", "other", "shared")
	assert.True(errors.Is(err, ErrNotFound))
	listed, err := reopened.ListAddons(ctx, "acme", "production")
	assert.NoErr(err)
	assert.Equal(len(listed), 1)
	input.Version = "1.2.4"
	input.Values = map[string]any{"message": "accidental overwrite"}
	_, err = reopened.PutAddon(ctx, "acme", "production", "shared", input, false)
	assert.True(errors.Is(err, ErrAddonExists))
	assert.True(errors.Is(err, ErrConflict))
	unchanged, err := reopened.GetAddon(ctx, "acme", "production", "shared")
	assert.NoErr(err)
	assert.Equal(unchanged, persisted)
	applied, err := reopened.PutAddon(ctx, "acme", "production", "shared", input, true)
	assert.NoErr(err)
	assert.True(applied.Operation != persisted.Operation)
	assert.Equal(applied.Specification.Version, "1.2.4")
}

// TestAddonInstallsReserveNamesWithinSpace gives concurrent installs one winner
// without reserving the same name in another space or fabricating Helm success.
func TestAddonInstallsReserveNamesWithinSpace(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cluster := newManagerCluster(t, managerClusterOptions{})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "addons.sqlite"), time.Second)
	production := createManagerEnvironment(t, manager, cluster, "production")
	staging, err := manager.CreateSpace(ctx, SpaceInput{Organization: "acme", Name: "staging", Cluster: "local"})
	assert.NoErr(err)
	cluster.server.Close()
	input := AddonInput{Repository: "https://charts.example.com", Chart: "service", Version: "1.2.3"}
	type result struct {
		addon Addon
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			addon, err := manager.PutAddon(ctx, "acme", "production", "shared", input, false)
			results <- result{addon: addon, err: err}
		}()
	}
	close(start)
	var accepted Addon
	conflicts := 0
	for range 2 {
		select {
		case result := <-results:
			if errors.Is(result.err, ErrAddonExists) {
				conflicts++
				continue
			}
			assert.NoErr(result.err)
			accepted = result.addon
		case <-ctx.Done():
			t.Fatal("concurrent chart installation acceptance timed out")
		}
	}
	assert.Equal(conflicts, 1)
	saved, err := manager.GetAddon(ctx, "acme", "production", "shared")
	assert.NoErr(err)
	assert.Equal(saved.Operation, accepted.Operation)
	operations, err := manager.database.List(ctx, "addon-operation")
	assert.NoErr(err)
	assert.Equal(len(operations), 1)
	other, err := manager.PutAddon(ctx, "acme", "staging", "shared", input, false)
	assert.NoErr(err)
	assert.Equal(other.Name, accepted.Name)
	assert.Equal(accepted.Namespace, production.Namespace)
	assert.Equal(other.Namespace, staging.Namespace)
	assert.True(other.Namespace != accepted.Namespace)
	history, err := manager.GetAddonEvents(ctx, TelemetryInput{Organization: "acme", Space: "staging", Application: "shared"})
	assert.NoErr(err)
	assert.True(len(history.Events) > 0)
	for _, event := range history.Events {
		assert.Equal(event.Deployment, other.Operation)
	}
}
