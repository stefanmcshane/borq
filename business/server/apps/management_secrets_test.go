package apps

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementSecretsPersistence owns versioned management publication without a
// deployment space, real SQLite reopening, and organization isolation.
func TestManagementSecretsPersistence(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	team, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	zero := 0
	input := PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "private-management-value", "EMPTY": ""}, BaseVersion: &zero}
	first, err := manager.PutManagementSecret(t.Context(), "team", "smtp", input)
	assert.NoErr(err)
	assert.Equal(first.Version, 1)
	input.BaseVersion = nil
	same, err := manager.PutManagementSecret(t.Context(), "team", "smtp", input)
	assert.NoErr(err)
	assert.Equal(same, first)
	assert.Equal(fixture.vault.writes(), 1)
	input.BaseVersion = &zero
	_, err = manager.PutManagementSecret(t.Context(), "team", "smtp", input)
	assert.True(errors.Is(err, ErrConflict))
	input.BaseVersion, input.Data = &first.Version, map[string]string{"TOKEN": "replacement-management-value"}
	second, err := manager.PutManagementSecret(t.Context(), "team", "smtp", input)
	assert.NoErr(err)
	assert.Equal(second.Version, 2)
	assert.Equal(second.Data, input.Data)
	pinned, err := manager.GetManagementSecret(t.Context(), "team", "smtp", "1")
	assert.NoErr(err)
	assert.Equal(pinned, first)
	resolved, err := manager.resolveManagementSecret(t.Context(), team.ID, "smtp", "latest")
	assert.NoErr(err)
	assert.Equal(resolved, second)
	other, err := manager.PutManagementSecret(t.Context(), "other", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "other-management-value"}})
	assert.NoErr(err)
	assert.Equal(other.Version, 1)
	empty, err := manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}})
	assert.NoErr(err)
	assert.Equal(empty.Version, 3)
	assert.Equal(empty.Data, map[string]string{})
	assert.Equal(fixture.vault.retention(), 11)
	records, err := manager.database.List(t.Context(), "management-secret-group")
	assert.NoErr(err)
	assert.Equal(len(records), 2)
	for _, record := range records {
		assert.True(!strings.Contains(string(record.Value), "TOKEN"))
	}
	for _, collection := range []string{"cluster", "space"} {
		records, err := manager.database.List(t.Context(), collection)
		assert.NoErr(err)
		assert.Equal(len(records), 0)
	}
	assert.NoErr(manager.Close())
	database, err := os.ReadFile(fixture.input.DatabasePath)
	assert.NoErr(err)
	for _, secret := range []string{"private-management-value", "replacement-management-value", "other-management-value"} {
		assert.True(!strings.Contains(string(database), secret))
	}
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	listed, err := manager.ListManagementSecrets(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(listed, []Secret{empty})
	pinned, err = manager.GetManagementSecret(t.Context(), "team", "smtp", "1")
	assert.NoErr(err)
	assert.Equal(pinned, first)
	isolated, err := manager.ListManagementSecrets(t.Context(), "other")
	assert.NoErr(err)
	assert.Equal(isolated, []Secret{other})
}

// TestManagementSecretsConflictsAndHistory protects against a writer racing after
// the metadata read and bounds reads even when an upstream vault retains older entries.
func TestManagementSecretsConflictsAndHistory(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	_, err = manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"PASSWORD": "initial"}})
	assert.NoErr(err)
	fixture.vault.injectConcurrentWrite()
	_, err = manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"PASSWORD": "lost-update"}})
	assert.True(errors.Is(err, ErrConflict))
	current, err := manager.GetManagementSecret(t.Context(), "team", "smtp", "latest")
	assert.NoErr(err)
	assert.Equal(current.Version, 2)
	assert.Equal(current.Data, map[string]string{"CONCURRENT": "preserved"})
	for version := 3; version <= 13; version++ {
		published, err := manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"VERSION": strconv.Itoa(version)}})
		assert.NoErr(err)
		assert.Equal(published.Version, version)
	}
	current, err = manager.GetManagementSecret(t.Context(), "team", "smtp", "latest")
	assert.NoErr(err)
	assert.Equal(current.OldestVersion, 3)
	_, err = manager.GetManagementSecret(t.Context(), "team", "smtp", "2")
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "older than the 10 previous versions"))
	oldest, err := manager.GetManagementSecret(t.Context(), "team", "smtp", "3")
	assert.NoErr(err)
	assert.Equal(oldest.Data, map[string]string{"VERSION": "3"})
	_, err = manager.GetManagementSecret(t.Context(), "team", "smtp", "14")
	assert.True(errors.Is(err, ErrNotFound))
	fixture.denied.Store(true)
	_, err = manager.PutManagementSecret(t.Context(), "team", "unpublished", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "do-not-persist"}})
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), "do-not-persist"))
	fixture.denied.Store(false)
	listed, err := manager.ListManagementSecrets(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(len(listed), 1)
	assert.Equal(listed[0].Name, "smtp")
	_, err = manager.PutManagementSecret(t.Context(), "team", "unpublished", PutSecretInput{Type: "secret-group", Data: map[string]string{}})
	assert.NoErr(err)
}

// TestManagementSecretsOwnership validates names, types and immutable ownership
// before contacting OpenBao, including corrupt metadata and a replaced organization.
func TestManagementSecretsOwnership(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementSecretFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer manager.Close()
	team, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	other, err := manager.CreateOrganization(t.Context(), "other")
	assert.NoErr(err)
	negative := -1
	for _, test := range []struct {
		name  string
		input PutSecretInput
	}{
		{"../smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}}},
		{"smtp", PutSecretInput{Type: "registry", Data: map[string]string{}}},
		{"smtp", PutSecretInput{Type: "secret-group"}},
		{"smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"invalid=name": "private"}}},
		{"smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}, BaseVersion: &negative}},
	} {
		_, err := manager.PutManagementSecret(t.Context(), "team", test.name, test.input)
		assert.True(errors.Is(err, ErrInvalid))
	}
	for _, version := range []string{"0", "01", "-1", "latest/other"} {
		_, err := manager.GetManagementSecret(t.Context(), "team", "smtp", version)
		assert.True(errors.Is(err, ErrInvalid))
	}
	_, err = manager.resolveManagementSecret(t.Context(), "../"+team.ID, "smtp", "1")
	assert.True(errors.Is(err, ErrInvalid))
	_, err = manager.PutManagementSecret(t.Context(), "missing", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}})
	assert.True(errors.Is(err, ErrNotFound))
	broken, err := manager.CreateOrganization(t.Context(), "broken")
	assert.NoErr(err)
	for _, corrupt := range []Organization{{Name: "broken", ID: "../" + broken.ID}, {Name: "other", ID: broken.ID}} {
		_, record, err := loadDocument[Organization](t.Context(), manager.database, "organization", "broken")
		assert.NoErr(err)
		write, err := documentWrite("organization", "broken", corrupt, record.Revision)
		assert.NoErr(err)
		assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
		_, err = manager.PutManagementSecret(t.Context(), "broken", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}})
		assert.True(err != nil && strings.Contains(err.Error(), "organization ownership is invalid"))
	}
	registered, err := manager.database.List(t.Context(), "management-secret-group")
	assert.NoErr(err)
	assert.Equal(len(registered), 0)
	assert.Equal(fixture.contacts.Load(), int64(0))
	_, err = manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{"TOKEN": "private"}})
	assert.NoErr(err)
	contacts := fixture.contacts.Load()
	_, err = manager.resolveManagementSecret(t.Context(), other.ID, "smtp", "latest")
	assert.True(errors.Is(err, ErrNotFound))
	stored, record, err := loadDocument[managementSecretGroup](t.Context(), manager.database, "management-secret-group", team.ID+"/smtp")
	assert.NoErr(err)
	stored.OrganizationID = other.ID
	write, err := documentWrite("management-secret-group", team.ID+"/smtp", stored, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.GetManagementSecret(t.Context(), "team", "smtp", "latest")
	assert.True(err != nil && strings.Contains(err.Error(), "ownership is invalid"))
	_, err = manager.PutManagementSecret(t.Context(), "team", "smtp", PutSecretInput{Type: "secret-group", Data: map[string]string{}})
	assert.True(err != nil && strings.Contains(err.Error(), "ownership is invalid"))
	_, organizationRecord, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
	assert.NoErr(err)
	replacement := team
	replacement.ID, err = newId()
	assert.NoErr(err)
	write, err = documentWrite("organization", "team", replacement, organizationRecord.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.resolveManagementSecret(t.Context(), team.ID, "smtp", "1")
	assert.True(errors.Is(err, ErrNotFound))
	assert.True(strings.Contains(err.Error(), "organization no longer exists"))
	assert.Equal(fixture.contacts.Load(), contacts)
}

// This fixture models only management-vault HTTP access and KV version/CAS protocol,
// reusing the existing group history fixture without creating any Kubernetes workload.
type managementSecretFixture struct {
	input    NewInput
	vault    *environmentGroupVault
	contacts atomic.Int64
	denied   atomic.Bool
}

func newManagementSecretFixture(t *testing.T) *managementSecretFixture {
	t.Helper()
	fixture := &managementSecretFixture{vault: &environmentGroupVault{versions: map[string][]map[string]string{}}}
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.contacts.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if fixture.denied.Load() {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
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
		if _, path, exists := strings.Cut(request.URL.Path, "/proxy/v1/secret/"); exists {
			request.URL.Path = environmentGroupProxyPrefix() + path
			fixture.vault.serve(writer, request)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(endpoint.Close)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: vault\n  cluster:\n    server: %s\ncontexts:\n- name: vault\n  context:\n    cluster: vault\n    user: vault\ncurrent-context: vault\nusers:\n- name: vault\n  user: {}\n", endpoint.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.input = NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: kubeconfig, PlatformSettings: platform.Settings{Namespace: "borq"}}
	return fixture
}
