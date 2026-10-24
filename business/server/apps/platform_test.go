package apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
)

// TestStoreClusterKubeconfig protects uploaded credentials and makes repeated registration inert.
func TestStoreClusterKubeconfig(t *testing.T) {
	is := is.New(t)
	path := filepath.Join(t.TempDir(), "cluster.kubeconfig")
	source := `apiVersion: v1
kind: Config
clusters:
- name: target
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
users:
- name: target
  user:
    token: first-private-credential
contexts:
- name: target
  context:
    cluster: target
    user: target
current-context: target
`
	created, err := storeClusterKubeconfig(path, source)
	is.NoErr(err)
	is.True(created)
	info, err := os.Stat(path)
	is.NoErr(err)
	is.Equal(info.Mode().Perm(), os.FileMode(0600))
	saved, err := os.ReadFile(path)
	is.NoErr(err)
	is.True(strings.Contains(string(saved), "first-private-credential"))
	// Comments and formatting do not turn equivalent credentials into a conflict.
	created, err = storeClusterKubeconfig(path, "# same cluster\n"+source)
	is.NoErr(err)
	is.True(!created)
	afterRetry, err := os.ReadFile(path)
	is.NoErr(err)
	is.Equal(string(afterRetry), string(saved))
	created, err = storeClusterKubeconfig(path, strings.ReplaceAll(source, "first-private-credential", "different-private-credential"))
	is.True(errors.Is(err, ErrConflict))
	is.True(!created)
	afterConflict, err := os.ReadFile(path)
	is.NoErr(err)
	is.Equal(string(afterConflict), string(saved))
	_, err = storeClusterKubeconfig(filepath.Join(t.TempDir(), "invalid"), "clusters: [malformed")
	is.True(errors.Is(err, ErrInvalid))
}

// TestAuthorizeTelemetry scopes bearer keys to the matching stored cluster digest.
func TestAuthorizeTelemetry(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	firstID := strings.Repeat("a", 32)
	secondID := strings.Repeat("b", 32)
	firstToken := "first-telemetry-private-key"
	secondToken := "second-telemetry-private-key"
	for _, item := range []struct{ id, token string }{{firstID, firstToken}, {secondID, secondToken}} {
		digest := sha256.Sum256([]byte(item.token))
		is.NoErr(manager.saveComponentRecord(ctx, item.id, componentRecord{
			Installation:  ComponentInstallation{ClusterID: item.id, Status: "ready"},
			TelemetryHash: hex.EncodeToString(digest[:]),
		}))
	}
	cases := []struct {
		id, token string
		allowed   bool
	}{
		{firstID, firstToken, true}, {secondID, secondToken, true},
		{firstID, secondToken, false}, {secondID, firstToken, false},
		{firstID, "", false}, {firstID, "wrong", false}, {strings.Repeat("c", 32), firstToken, false},
	}
	for _, item := range cases {
		allowed, err := manager.AuthorizeTelemetry(ctx, item.id, item.token)
		is.NoErr(err)
		is.Equal(allowed, item.allowed)
	}
	is.NoErr(manager.saveComponentRecord(ctx, firstID, componentRecord{Installation: ComponentInstallation{ClusterID: firstID, Status: "failed"}}))
	allowed, err := manager.AuthorizeTelemetry(ctx, firstID, firstToken)
	is.NoErr(err)
	is.True(!allowed)
}

// TestManagerRejectsInvalidInfrastructureBeforeDatabaseCreation fails configuration
// errors before any persistent state or credential renewal worker is created.
func TestManagerRejectsInvalidInfrastructureBeforeDatabaseCreation(t *testing.T) {
	valid := platform.Settings{OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834"}
	for _, current := range []struct {
		name       string
		settings   platform.Settings
		kubeconfig string
	}{
		{name: "missing destinations"},
		{name: "invalid profile", settings: platform.Settings{Profile: "invalid", OpenBaoURL: valid.OpenBaoURL, TelemetryURL: valid.TelemetryURL}},
		{name: "embedded credentials", settings: platform.Settings{OpenBaoURL: "https://root:secret@vault.test", TelemetryURL: valid.TelemetryURL}},
		{name: "missing kubeconfig", settings: valid, kubeconfig: filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(current.name, func(t *testing.T) {
			is := is.New(t)
			database := filepath.Join(t.TempDir(), "state", "borq.sqlite")
			_, err := NewManager(context.Background(), NewInput{DatabasePath: database, InfrastructureEnabled: true,
				ManagementKubeconfigPath: current.kubeconfig, PlatformSettings: current.settings})
			is.True(err != nil)
			_, err = os.Stat(database)
			is.True(os.IsNotExist(err))
			_, err = os.Stat(filepath.Dir(database))
			is.True(os.IsNotExist(err))
		})
	}
}
