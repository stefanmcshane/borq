package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestManagementIdentityDeclaration keeps provider choice explicit and inherits
// callback origins without modifying the caller's declaration or durable plan order.
func TestManagementIdentityDeclaration(t *testing.T) {
	manager, _ := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	for _, test := range []struct {
		name      string
		configure func(*ManagementConfiguration)
		baseURL   string
		invalid   bool
	}{
		{"global origin", func(*ManagementConfiguration) {}, "https://borq.example.test", false},
		{"provider override", func(configuration *ManagementConfiguration) {
			configuration.Organizations[0].ManagedPocketId.BaseURL = "https://override.example.test"
		}, "https://override.example.test", false},
		{"separate organization administrator", func(configuration *ManagementConfiguration) {
			configuration.Organizations[0].ManagedPocketId.OrganizationAdminEmail = "organization-admin@example.test"
		}, "https://borq.example.test", false},
		{"external provider", func(configuration *ManagementConfiguration) {
			configuration.Organizations[0].ManagedPocketId = nil
			configuration.Organizations[0].Oidc = &ManagementOidc{IssuerURL: "https://external.example.test", ClientID: "existing-client", AdminEmail: "admin@example.test"}
		}, "https://borq.example.test", false},
		{"both providers", func(configuration *ManagementConfiguration) { configuration.Organizations[0].Oidc = &ManagementOidc{} }, "", true},
		{"missing callback origin", func(configuration *ManagementConfiguration) { configuration.BorqPublicURL = "" }, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			configuration := ManagementConfiguration{BorqPublicURL: "https://borq.example.test", Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: "https://id.example.test", AdminEmail: "admin@example.test"}}}}
			test.configure(&configuration)
			original, err := json.Marshal(configuration)
			assert.NoErr(err)
			normalized, err := manager.normalizeManagementIdentity(t.Context(), configuration)
			if err == nil {
				err = validateManagementConfiguration(normalized)
			}
			if test.invalid {
				assert.True(errors.Is(err, ErrInvalid))
				return
			}
			assert.NoErr(err)
			assert.Equal(normalized.Organizations[0].oidcInput().BaseURL, test.baseURL)
			after, err := json.Marshal(configuration)
			assert.NoErr(err)
			assert.Equal(string(original), string(after))
			if configuration.Organizations[0].ManagedPocketId != nil {
				assert.Equal(normalized.Organizations[0].ManagedPocketId.CertificateIssuer, "letsencrypt-cloudflare")
				adminEmail := configuration.Organizations[0].ManagedPocketId.OrganizationAdminEmail
				if adminEmail == "" {
					adminEmail = "admin@example.test"
				}
				assert.Equal(normalized.Organizations[0].oidcInput().AdminEmail, adminEmail)
				actions := managementActions(normalized)
				assert.Equal(len(actions), 3)
				assert.Equal(actions[0].kind, "organization")
				assert.Equal(actions[1].kind, "pocketid")
				assert.Equal(actions[2].kind, "oidc")
			}
		})
	}
}

// TestManagementIdentityExport preserves provider settings, administrator selection,
// and private references when replaying the exported declaration.
func TestManagementIdentityExport(t *testing.T) {
	assert := is.New(t)
	manager, _, _, _ := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	owner, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
	assert.NoErr(err)
	pocket, _, err := manager.readPocketId(t.Context(), owner.ID)
	assert.NoErr(err)
	policy := organizationLoginPolicy{OrganizationID: owner.ID, AdminEmail: "selected-org-admin@example.test", Issuer: pocket.Status.IssuerURL, ClientID: pocket.ID, BaseURL: pocket.Status.BaseURL, PocketInstanceID: pocket.ID}
	policyWrite, err := documentWrite("organization-login-policy", owner.ID, policy, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{policyWrite}))
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	assert.True(exported.Organizations[0].Oidc == nil)
	assert.True(exported.Organizations[0].ManagedPocketId != nil)
	assert.Equal(exported.Organizations[0].ManagedPocketId.IssuerURL, pocket.Status.IssuerURL)
	assert.Equal(exported.Organizations[0].ManagedPocketId.BaseURL, pocket.Status.BaseURL)
	assert.Equal(exported.Organizations[0].ManagedPocketId.OrganizationAdminEmail, "selected-org-admin@example.test")
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(encoded), "clientSecret"))
	normalized, err := manager.normalizeManagementIdentity(t.Context(), exported)
	assert.NoErr(err)
	assert.Equal(normalized.Organizations[0].oidcInput().AdminEmail, "selected-org-admin@example.test")
	assert.NoErr(matchManagementPocketId(pocket, *normalized.Organizations[0].ManagedPocketId, normalized.Organizations[0].oidcInput()))
}

// TestManagementIdentityAcceptance captures the public nested administrator and
// provider across task persistence and restart without storing a second declaration.
func TestManagementIdentityAcceptance(t *testing.T) {
	assert := is.New(t)
	manager, input := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	configuration := ManagementConfiguration{BorqPublicURL: "https://borq.example.test", Organizations: []ManagementOrganization{{Name: "team", ManagedPocketId: &ManagementPocketId{IssuerURL: "https://id.example.test", AdminEmail: "provider-admin@example.test", OrganizationAdminEmail: "organization-admin@example.test"}}}}
	operation, err := manager.ApplyManagementConfiguration(t.Context(), configuration)
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), input)
	assert.NoErr(err)
	task, _, err := loadDocument[managementTask](t.Context(), manager.database, "management-task", operation.ID)
	assert.NoErr(err)
	assert.True(task.Configuration.Organizations[0].Oidc == nil)
	assert.Equal(task.Configuration.Organizations[0].oidcInput().AdminEmail, "organization-admin@example.test")
	assert.Equal(task.Configuration.Organizations[0].ManagedPocketId.AdminEmail, "provider-admin@example.test")
	assert.Equal(task.Configuration.Organizations[0].ManagedPocketId.OrganizationAdminEmail, "organization-admin@example.test")
	policy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", task.Authentication["team"].OrganizationID)
	assert.NoErr(err)
	assert.Equal(policy.AdminEmail, "organization-admin@example.test")
	assert.Equal(policy.Issuer, "https://id.example.test")
}
