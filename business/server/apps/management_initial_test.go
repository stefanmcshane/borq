package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestInitialManagementInputDurability records first-apply credentials in an
// operation-owned immutable Kubernetes snapshot, deduplicates a live request,
// and preserves captured ownership after close/reopen. Disabled infrastructure
// must produce a failed operation, never fabricated component readiness.
func TestInitialManagementInputDurability(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	fixture.input.ManagementBootstrapOnly = true
	fixture.gate = make(chan struct{})
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := managementNetworkDeclaration()
	configuration.Organizations = []ManagementOrganization{{Name: "team", Oidc: &ManagementOidc{AdminEmail: "owner@example.test", IssuerURL: "https://id.example.test", ClientID: "existing-client", ClientSecret: "private-oidc-client-secret"}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	select {
	case <-fixture.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial management input was not read")
	}
	repeated, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	assert.Equal(repeated.ID, operation.ID)
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.True(task.InputRef != "")
	assert.Equal(len(task.Authentication), 1)
	assert.Equal(task.Authentication["team"].Revision, int64(0))
	assert.Equal(len(task.Authentication["team"].OrganizationID), 32)
	owner, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
	assert.NoErr(err)
	assert.Equal(owner.ID, task.Authentication["team"].OrganizationID)
	assert.Equal(owner.AuthenticationState, "queued")
	encoded, err := json.Marshal(task)
	assert.NoErr(err)
	assertManagementInputRedacted(t, encoded)
	fixture.mutex.Lock()
	secret := fixture.secrets[task.InputRef].DeepCopy()
	fixture.mutex.Unlock()
	assert.Equal(secret.Labels["app.kubernetes.io/managed-by"], "borq")
	assert.Equal(secret.Labels["borq.run/management-input"], operation.ID)
	assert.True(secret.Immutable != nil && *secret.Immutable)
	assert.True(strings.Contains(string(secret.Data["configuration.json"]), "private-oidc-client-secret"))
	close(fixture.gate)
	waitManagementResult(t, manager, operation.ID, "failed")
	result, err := manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assertManagementInputRedacted(t, []byte(result.Message))
	assert.True(strings.Contains(result.Message, "infrastructure management is disabled"))
	assert.NoErr(manager.Close())
	assertManagementInputPrivate(t, fixture.input.DatabasePath)
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	retained, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.Equal(retained.Authentication, task.Authentication)
	assert.Equal(retained.InputRef, "")
	client, namespace, err := manager.managementInputClient()
	assert.NoErr(err)
	_, err = client.CoreV1().Secrets(namespace).Get(t.Context(), task.InputRef, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
	result, err = manager.GetManagementOperation(t.Context(), operation.ID)
	assert.NoErr(err)
	assert.Equal(result.Status, "failed")
}

// TestInitialManagementInputGuard rejects a replaced or foreign snapshot before
// network credentials or configuration can be changed by its payload.
func TestInitialManagementInputGuard(t *testing.T) {
	for _, name := range []string{"foreign ownership", "replaced secret uid", "mutable snapshot", "changed content", "changed organization revision", "recreated organization"} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementInputFixture(t)
			manager, err := NewManager(t.Context(), fixture.input)
			assert.NoErr(err)
			defer func() { _ = manager.Close() }()
			configuration := managementNetworkDeclaration()
			owners := map[string]managementAuthentication{}
			if name == "changed organization revision" || name == "recreated organization" {
				owner, err := manager.CreateOrganization(t.Context(), "team")
				assert.NoErr(err)
				configuration.Organizations = []ManagementOrganization{{Name: owner.Name}}
				owners[owner.Name] = managementAuthentication{OrganizationID: owner.ID}
				if name == "changed organization revision" {
					write, err := documentWrite("organization-oidc", owner.ID, organizationOidcRecord{OrganizationID: owner.ID}, 0)
					assert.NoErr(err)
					assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
				} else {
					_, record, err := loadDocument[Organization](t.Context(), manager.database, "organization", owner.Name)
					assert.NoErr(err)
					owner.ID = strings.Repeat("c", 32)
					write, err := documentWrite("organization", owner.Name, owner, record.Revision)
					assert.NoErr(err)
					assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
				}
			}
			digest, err := managementRequestDigest(configuration)
			assert.NoErr(err)
			hash, err := bcrypt.GenerateFromPassword(digest[:], bcrypt.DefaultCost)
			assert.NoErr(err)
			operation := Operation{ID: strings.Repeat("a", 32)}
			task := managementTask{InputRef: "borq-management-input-" + operation.ID, RequestHash: string(hash), Authentication: owners}
			labels := managementInputLabels(operation.ID)
			if name == "foreign ownership" {
				labels["borq.run/management-input"] = strings.Repeat("b", 32)
			} else if name == "changed content" {
				configuration.Network.Cloudflare.ApiToken = "replaced-private-token"
			}
			encoded, err := json.Marshal(configuration)
			assert.NoErr(err)
			client, namespace, err := manager.managementInputClient()
			assert.NoErr(err)
			immutable := name != "mutable snapshot"
			created, err := client.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: task.InputRef, Namespace: namespace, Labels: labels}, Immutable: &immutable, Data: map[string][]byte{"configuration.json": encoded}}, metav1.CreateOptions{})
			assert.NoErr(err)
			task.InputUID = string(created.UID)
			if name == "replaced secret uid" {
				task.InputUID = "previous-secret-uid"
			}
			_, err = manager.prepareInitialManagementConfiguration(t.Context(), operation, task)
			assert.True(err != nil)
			if name == "changed organization revision" {
				assert.True(strings.Contains(err.Error(), "OIDC settings changed"))
			} else if name == "recreated organization" {
				assert.True(strings.Contains(err.Error(), "organization identity changed"))
			} else {
				assert.True(strings.Contains(err.Error(), "input"))
			}
			if name == "changed content" {
				assert.True(strings.Contains(err.Error(), "changed after acceptance"))
			}
			_, err = client.CoreV1().Secrets(namespace).Get(t.Context(), "borq-network-credentials", metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
			assert.True(errors.Is(err, ErrNotFound))
		})
	}
}

// TestManagementInputCleanupOwnership sends UID preconditions when removing a
// private snapshot, preserving a replacement Secret that reused its name.
func TestManagementInputCleanupOwnership(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	client, namespace, err := manager.managementInputClient()
	assert.NoErr(err)
	name := "borq-management-input-" + strings.Repeat("a", 32)
	created, err := client.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: map[string][]byte{"configuration.json": []byte(`{}`)}}, metav1.CreateOptions{})
	assert.NoErr(err)
	task := managementTask{InputRef: name, InputUID: "previous-secret-uid"}
	err = manager.deleteManagementInput(t.Context(), task)
	assert.True(apierrors.IsConflict(err))
	retained, err := client.CoreV1().Secrets(namespace).Get(t.Context(), name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retained.UID, created.UID)
	task.InputUID = string(created.UID)
	assert.NoErr(manager.deleteManagementInput(t.Context(), task))
	_, err = client.CoreV1().Secrets(namespace).Get(t.Context(), name, metav1.GetOptions{})
	assert.True(apierrors.IsNotFound(err))
}

// TestManagementTerminalOperationRejectsStaleSnapshot prevents a delayed queued
// snapshot from restarting durable failed work or changing its recorded outcome.
func TestManagementTerminalOperationRejectsStaleSnapshot(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	now := time.Now().UTC()
	operation := Operation{ID: strings.Repeat("d", 32), Status: "failed", Phase: "management", Message: "original setup failed", StartedAt: now, UpdatedAt: now}
	task := managementTask{Configuration: ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "must-not-be-created"}}}}
	operationWrite, err := documentWrite("management-operation", operation.ID, operation, 0)
	assert.NoErr(err)
	taskWrite, err := documentWrite("management-task", operation.ID, task, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{operationWrite, taskWrite}))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	_, before, err := loadDocument[Operation](t.Context(), manager.database, "management-operation", operation.ID)
	assert.NoErr(err)
	stale := operation
	stale.Status, stale.Message = "queued", "management configuration accepted"
	manager.runManagementOperation(stale)
	current, after, err := loadDocument[Operation](t.Context(), manager.database, "management-operation", operation.ID)
	assert.NoErr(err)
	assert.Equal(current, operation)
	assert.Equal(after.Revision, before.Revision)
	_, _, err = loadDocument[Organization](t.Context(), manager.database, "organization", "must-not-be-created")
	assert.True(errors.Is(err, ErrNotFound))
}
