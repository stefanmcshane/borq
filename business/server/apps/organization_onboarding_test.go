package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestOrganizationOnboardingExternal owns external provider validation, atomic failure and external administrator enrollment.
func TestOrganizationOnboardingExternal(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	_, err := manager.OnboardOrganization(t.Context(), "missing-admin", OrganizationOnboardingInput{Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: identity.Issuer, ClientID: "external", ClientSecret: "private-secret", BaseURL: "https://borq.test"}})
	assert.True(errors.Is(err, ErrInvalid))
	_, _, err = loadDocument[Organization](t.Context(), manager.database, "organization", "missing-admin")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.OnboardOrganization(t.Context(), "invalid-provider", OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: "http://foreign.test", ClientID: "external", ClientSecret: "private-secret", BaseURL: "https://borq.test"}})
	assert.True(errors.Is(err, ErrInvalid))
	_, _, err = loadDocument[Organization](t.Context(), manager.database, "organization", "invalid-provider")
	assert.True(errors.Is(err, ErrNotFound))
	input := OrganizationOnboardingInput{AdminEmail: "owner@example.test", Oidc: &OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: identity.Issuer, ClientID: "external", ClientSecret: "private-external-secret", BaseURL: "https://borq.test"}}
	organization, err := manager.OnboardOrganization(t.Context(), "external", input)
	assert.NoErr(err)
	assert.Equal(organization.AuthenticationState, "ready")
	prepared, err := manager.PrepareOrganizationLogin(t.Context(), "external", "")
	assert.NoErr(err)
	assert.True(prepared.Generation != generation)
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "external", prepared.Generation, identity)
	assert.NoErr(err)
	assert.Equal(login.OrganizationID, organization.ID)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	provider, record, err := manager.organizationOidc(t.Context(), "external")
	assert.NoErr(err)
	assert.True(!strings.Contains(string(record.Value), input.Oidc.ClientSecret))
	secret, err := manager.organizationOidcSecret(t.Context(), provider)
	assert.NoErr(err)
	assert.Equal(secret, input.Oidc.ClientSecret)
	for _, change := range []string{"client", "base address"} {
		settings, err := manager.GetOrganizationOidc(t.Context(), "external")
		assert.NoErr(err)
		updated := OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: settings.IssuerURL, ClientID: settings.ClientID, ClientSecret: input.Oidc.ClientSecret, BaseURL: settings.BaseURL, Revision: settings.Revision}
		if change == "client" {
			updated.ClientID = "replacement-client"
		} else {
			updated.BaseURL = "https://new-borq.test"
		}
		_, err = manager.PutOrganizationOidc(t.Context(), "external", updated)
		assert.NoErr(err)
		assert.True(manager.ValidateOrganizationLogin(t.Context(), login) != nil)
		prepared, err = manager.PrepareOrganizationLogin(t.Context(), "external", "")
		assert.NoErr(err)
		login, err = manager.AuthorizeOrganizationLogin(t.Context(), "external", prepared.Generation, identity)
		assert.NoErr(err)
		assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	}
}

// TestOrganizationOnboardingSourceSelection keeps provider reuse explicit and leaves ordinary creation under administrator-token access.
func TestOrganizationOnboardingSourceSelection(t *testing.T) {
	assert := is.New(t)
	manager, _, _, _ := organizationLoginFixture(t)
	defer manager.Close()
	manager.runtime.platformSettings.ClusterName = "management"
	manager.runtime.platformSettings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "management@example.test"}
	source, err := manager.organizationPocketIdSource(t.Context(), "team")
	assert.NoErr(err)
	other, err := manager.CreateOrganization(t.Context(), "another")
	assert.NoErr(err)
	source.Organization, source.OrganizationID = other.Name, other.ID
	source.ID, err = newId()
	assert.NoErr(err)
	source.Status.IssuerURL = "https://another-id.test"
	source.Status.CertificateIssuer = "issuer"
	write, err := documentWrite("organization-pocketid", other.ID, source, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	created, err := manager.OnboardOrganization(t.Context(), "dedicated", OrganizationOnboardingInput{})
	assert.NoErr(err)
	_, _, err = manager.readPocketId(t.Context(), created.ID)
	assert.True(errors.Is(err, ErrNotFound))
	assert.Equal(created.AuthenticationState, "")
	_, _, err = loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", created.ID)
	assert.True(errors.Is(err, ErrNotFound))
	created, err = manager.OnboardOrganization(t.Context(), "selected", OrganizationOnboardingInput{PocketIdOrganization: "team", AdminEmail: "organization-admin@example.test"})
	assert.NoErr(err)
	policy, _, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", created.ID)
	assert.NoErr(err)
	assert.Equal(policy.PocketOrganization, "team")
	assert.Equal(policy.AdminEmail, "organization-admin@example.test")
	_, err = manager.OnboardOrganization(t.Context(), "missing-owner", OrganizationOnboardingInput{PocketIdOrganization: "missing"})
	assert.True(errors.Is(err, ErrInvalid))
}

// TestManagementOrganizationOnboarding preserves administrator requirements
// when a management file explicitly requests an external identity provider.
func TestManagementOrganizationOnboarding(t *testing.T) {
	assert := is.New(t)
	manager, _ := pocketIdQueueFixture(t)
	defer manager.Close()
	_, err := manager.ApplyManagementConfiguration(t.Context(), ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "missing-admin", Oidc: &ManagementOidc{IssuerURL: "https://external.test", ClientID: "external", ClientSecret: "private-external-secret", BaseURL: "https://borq.test"}}}})
	assert.True(errors.Is(err, ErrInvalid))
	_, _, err = loadDocument[Organization](t.Context(), manager.database, "organization", "missing-admin")
	assert.True(errors.Is(err, ErrNotFound))
	_, err = manager.CreateOrganization(t.Context(), "metadata")
	assert.NoErr(err)
	provider := managementOidcDiscovery(t)
	issuer := strings.Replace(provider.URL, "127.0.0.1", "localhost", 1)
	declaration := ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "metadata", Oidc: &ManagementOidc{IssuerURL: issuer, ClientID: "external", ClientSecret: "private-external-secret", BaseURL: "https://borq.test"}}}}
	_, err = manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "administrator's email"))
	declaration.Organizations[0].Oidc.AdminEmail = "owner@example.test"
	operation, err := manager.ApplyManagementConfiguration(t.Context(), declaration)
	assert.NoErr(err)
	waitManagementResult(t, manager, operation.ID, "succeeded")
	saved, policy, err := manager.organizationLoginProvider(t.Context(), "metadata")
	assert.NoErr(err)
	assert.Equal(saved.Settings.ClientID, "external")
	assert.Equal(policy.AdminEmail, "owner@example.test")
}

// TestOrganizationOnboardingManaged owns atomic first-provider queueing and hostname reservation, without claiming controller readiness.
func TestOrganizationOnboardingManaged(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	defer func() { _ = manager.Close() }()
	_, err := manager.OnboardOrganization(t.Context(), "missing-provider", OrganizationOnboardingInput{AdminEmail: "owner@example.test"})
	assert.True(errors.Is(err, ErrInvalid))
	input := OrganizationOnboardingInput{AdminEmail: "organization-admin@example.test", PocketId: &OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "cloudflare", AdminEmail: "management@example.test"}}
	organization, err := manager.OnboardOrganization(t.Context(), "first", input)
	assert.NoErr(err)
	assert.Equal(organization.AuthenticationState, "queued")
	pocket, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(pocket.Status.State, "queued")
	assert.Equal(pocket.Status.AdminEmail, "management@example.test")
	policy, policyRecord, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.NoErr(err)
	assert.Equal(policy.AdminEmail, "organization-admin@example.test")
	assert.Equal(policy.PocketInstanceID, pocket.ID)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	reopened, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(reopened, pocket)
	_, err = manager.OnboardOrganization(t.Context(), "first", OrganizationOnboardingInput{AdminEmail: "replacement@example.test"})
	assert.NoErr(err)
	unchanged, unchangedRecord, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.NoErr(err)
	assert.Equal(unchanged, policy)
	assert.Equal(unchangedRecord.Revision, policyRecord.Revision)
	_, err = manager.PrepareOrganizationLogin(t.Context(), "first", "")
	assert.True(err != nil)
	_, err = manager.OnboardOrganization(t.Context(), "conflict", input)
	assert.True(errors.Is(err, ErrConflict))
	for _, name := range []string{"missing-provider", "conflict"} {
		_, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", name)
		assert.True(errors.Is(err, ErrNotFound))
	}
}

// TestOrganizationOnboardingWithoutProvider keeps ordinary creation independent of public networking and identity hosting after restart.
func TestOrganizationOnboardingWithoutProvider(t *testing.T) {
	for _, publicNetwork := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "public"}[publicNetwork], func(t *testing.T) {
			assert := is.New(t)
			manager, configuration := pocketIdQueueFixture(t)
			assert.NoErr(manager.Close())
			if publicNetwork {
				configuration.PlatformSettings.ClusterName = "management"
				configuration.PlatformSettings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "cert-admin@example.test"}
			}
			manager, err := NewManager(t.Context(), configuration)
			assert.NoErr(err)
			defer func() { _ = manager.Close() }()
			organization, err := manager.OnboardOrganization(t.Context(), "team", OrganizationOnboardingInput{})
			assert.NoErr(err)
			assert.Equal(organization.AuthenticationState, "")
			assert.NoErr(manager.Close())
			manager, err = NewManager(t.Context(), configuration)
			assert.NoErr(err)
			reopened, _, err := loadDocument[Organization](t.Context(), manager.database, "organization", "team")
			assert.NoErr(err)
			assert.Equal(reopened, organization)
			for _, collection := range []string{"organization-pocketid", "organization-login-policy", "organization-oidc", "organization-pocketid-host"} {
				records, err := manager.database.List(t.Context(), collection)
				assert.NoErr(err)
				assert.Equal(len(records), 0)
			}
			names, err := manager.ListOrganizationSignInNames(t.Context())
			assert.NoErr(err)
			assert.Equal(len(names), 0)
			repeated, err := manager.OnboardOrganization(t.Context(), "team", OrganizationOnboardingInput{})
			assert.NoErr(err)
			assert.Equal(repeated, organization)
		})
	}
}

// TestOrganizationOnboardingReservedHostname rejects explicit provider addresses reserved for management routing.
func TestOrganizationOnboardingReservedHostname(t *testing.T) {
	assert := is.New(t)
	manager, configuration := pocketIdQueueFixture(t)
	assert.NoErr(manager.Close())
	configuration.PlatformSettings.ClusterName = "id-collision"
	configuration.PlatformSettings.Network.Cloudflare = &platform.CloudflareSettings{WildcardDomain: "example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "cert-admin@example.test"}
	manager, err := NewManager(t.Context(), configuration)
	assert.NoErr(err)
	defer manager.Close()
	_, err = manager.OnboardOrganization(t.Context(), "collision", OrganizationOnboardingInput{PocketId: &OrganizationPocketIdInput{IssuerURL: "https://id-collision.example.test", BaseURL: "https://borq.example.test", AdminEmail: "owner@example.test", CertificateIssuer: "letsencrypt-cloudflare"}})
	assert.True(errors.Is(err, ErrInvalid))
	assert.True(strings.Contains(err.Error(), "reserved"))
	_, _, err = loadDocument[Organization](t.Context(), manager.database, "organization", "collision")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestOrganizationOnboardingSharedClient drives real Pocket ID HTTP provisioning and durable metadata upgrades without replacing existing sessions or credentials.
// Stored installation readiness is fixture input; this test does not simulate Kubernetes controllers.
func TestOrganizationOnboardingSharedClient(t *testing.T) {
	assert := is.New(t)
	manager, configuration, sourceGeneration, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	source, err := manager.organizationPocketIdSource(t.Context(), "team")
	assert.NoErr(err)
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	archive, err := sqliteworkload.NewArchiveKey()
	assert.NoErr(err)
	_, err = vault.WriteKv(t.Context(), "secret", source.credentialsPath(), openbao.WriteKvInput{Values: map[string]string{"apiKey": strings.Repeat("k", 32), "clientSecret": strings.Repeat("s", 32), "encryptionKey": strings.Repeat("e", 32), "ageIdentity": archive.Identity, "ageRecipient": archive.Recipient}})
	assert.NoErr(err)
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: configuration.ManagementKubeconfigPath}, logger.Logger{})
	assert.NoErr(err)
	originalHost := connection.RestConfig().Host
	original, err := url.Parse(originalHost)
	assert.NoErr(err)
	proxy := httputil.NewSingleHostReverseProxy(original)
	var registered map[string]any
	sourceClient := map[string]any{"id": source.ID, "name": "Borq team", "callbackURLs": []string{"https://borq.test/api/auth/oidc/callback/" + stableId(source.OrganizationID+"/"+identity.Issuer)}, "isPublic": false, "pkceEnabled": true}
	storedSecret, createdClients, secretWrites, userMutations := "", 0, 0, 0
	updatedClients := map[string]int{}
	service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, endpoint, pocketRequest := strings.Cut(request.URL.Path, "/services/http:pocketid-"+source.ID+":1411/proxy")
		if !pocketRequest {
			proxy.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(endpoint, "/api/users"):
			if request.Method != http.MethodGet {
				userMutations++
			}
			writer.WriteHeader(http.StatusForbidden)
		case endpoint == "/api/oidc/token":
			assert.NoErr(request.ParseForm())
			if storedSecret == "" || request.Form.Get("client_secret") != storedSecret {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			assert.Equal(request.Form.Get("client_id"), registered["id"])
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
		case endpoint == "/api/oidc/clients" && request.Method == http.MethodPost:
			assert.Equal(request.Header.Get("X-API-KEY"), strings.Repeat("k", 32))
			assert.NoErr(json.NewDecoder(request.Body).Decode(&registered))
			createdClients++
			_ = json.NewEncoder(writer).Encode(registered)
		case strings.HasSuffix(endpoint, "/secrets"):
			var payload struct{ Secret string }
			assert.NoErr(json.NewDecoder(request.Body).Decode(&payload))
			storedSecret = payload.Secret
			secretWrites++
			writer.WriteHeader(http.StatusCreated)
		case endpoint == "/api/oidc/clients/"+source.ID:
			if request.Method == http.MethodPut {
				assert.NoErr(json.NewDecoder(request.Body).Decode(&sourceClient))
				updatedClients[source.ID]++
			}
			_ = json.NewEncoder(writer).Encode(sourceClient)
		case registered == nil:
			writer.WriteHeader(http.StatusNotFound)
		default:
			assert.Equal(endpoint, "/api/oidc/clients/"+registered["id"].(string))
			if request.Method == http.MethodPut {
				assert.NoErr(json.NewDecoder(request.Body).Decode(&registered))
				updatedClients[registered["id"].(string)]++
			}
			_ = json.NewEncoder(writer).Encode(registered)
		}
	}))
	defer service.Close()
	kubeconfig, err := os.ReadFile(configuration.ManagementKubeconfigPath)
	assert.NoErr(err)
	assert.NoErr(os.WriteFile(configuration.ManagementKubeconfigPath, []byte(strings.Replace(string(kubeconfig), originalHost, service.URL, 1)), 0o600))
	organization, err := manager.OnboardOrganization(t.Context(), "second", OrganizationOnboardingInput{PocketIdOrganization: "team", AdminEmail: "second-admin@example.test"})
	assert.NoErr(err)
	assert.Equal(organization.AuthenticationState, "queued")
	pending, record, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	assert.True(pending.ClientID != source.ID)
	assert.True(!strings.Contains(string(record.Value), "clientSecret"))
	_, _, err = manager.readPocketId(t.Context(), organization.ID)
	assert.True(errors.Is(err, ErrNotFound))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	reopened, reopenedRecord, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	assert.Equal(reopened, pending)
	assert.NoErr(manager.provisionOrganizationPocketIdClient(t.Context(), reopened, reopenedRecord.Revision))
	manager.provisionOrganizationPocketIdClients(t.Context())
	assert.Equal(createdClients, 1)
	assert.Equal(registered["id"], pending.ClientID)
	callbacks := registered["callbackURLs"].([]any)
	assert.Equal(callbacks, []any{"https://borq.test/api/auth/oidc/callback/" + stableId(organization.ID+"/"+identity.Issuer)})
	prepared, err := manager.PrepareOrganizationLogin(t.Context(), "second", "")
	assert.NoErr(err)
	other, err := manager.AuthorizeOrganizationLogin(t.Context(), "second", prepared.Generation, identity)
	assert.NoErr(err)
	permissions, err := manager.OrganizationLoginPermissions(t.Context(), other)
	assert.NoErr(err)
	assert.Equal(permissions, []Permission{})
	sharedIdentity := identity
	sharedIdentity.Email, sharedIdentity.Subject = "second-admin@example.test", "second-account"
	unverifiedSharedIdentity := sharedIdentity
	unverifiedSharedIdentity.EmailVerified = false
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "second", prepared.Generation, unverifiedSharedIdentity)
	assert.True(err != nil)
	sharedLogin, err := manager.AuthorizeOrganizationLogin(t.Context(), "second", prepared.Generation, sharedIdentity)
	assert.NoErr(err)
	assert.Equal(userMutations, 0)
	sourceLogin, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", sourceGeneration, identity)
	assert.NoErr(err)
	sourceOidc, _, err := manager.organizationOidc(t.Context(), "team")
	assert.NoErr(err)
	sourceSecret, err := manager.organizationOidcSecret(t.Context(), sourceOidc)
	assert.NoErr(err)
	sharedOidc, _, err := manager.organizationOidc(t.Context(), "second")
	assert.NoErr(err)
	sharedSecret, err := manager.organizationOidcSecret(t.Context(), sharedOidc)
	assert.NoErr(err)
	// Simulate records and provider links written before launcher/logout metadata existed.
	shared, sharedRecord, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	assert.True(shared.ClientLinksConfigured)
	shared.ClientLinksConfigured = false
	legacyWrite, err := documentWrite("organization-pocketid-client", organization.ID, shared, sharedRecord.Revision)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(legacyWrite.Value), "clientLinksConfigured"))
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{legacyWrite}))
	delete(registered, "logoutCallbackURLs")
	delete(registered, "launchURL")
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	legacySource, _, err := manager.readPocketId(t.Context(), source.OrganizationID)
	assert.NoErr(err)
	assert.True(!legacySource.ClientLinksConfigured)
	legacyShared, _, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	assert.True(!legacyShared.ClientLinksConfigured)
	manager.provisionPendingPocketId(t.Context(), map[string]bool{})
	assert.Equal(updatedClients[source.ID], 1)
	assert.Equal(updatedClients[shared.ClientID], 1)
	assert.Equal(createdClients, 1)
	assert.Equal(secretWrites, 1)
	assert.Equal(userMutations, 0)
	assert.Equal(sourceClient["logoutCallbackURLs"], []any{"https://borq.test/login"})
	assert.Equal(sourceClient["launchURL"], "https://borq.test/")
	assert.Equal(registered["logoutCallbackURLs"], []any{"https://borq.test/login"})
	assert.Equal(registered["launchURL"], "https://borq.test/")
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	savedSource, _, err := manager.readPocketId(t.Context(), source.OrganizationID)
	assert.NoErr(err)
	assert.True(savedSource.ClientLinksConfigured)
	savedShared, _, err := loadDocument[organizationPocketIdClient](t.Context(), manager.database, "organization-pocketid-client", organization.ID)
	assert.NoErr(err)
	assert.True(savedShared.ClientLinksConfigured)
	for _, expected := range []struct {
		name   string
		oidc   organizationOidcRecord
		secret string
		login  OrganizationLogin
	}{{"team", sourceOidc, sourceSecret, sourceLogin}, {"second", sharedOidc, sharedSecret, sharedLogin}} {
		current, _, err := manager.organizationOidc(t.Context(), expected.name)
		assert.NoErr(err)
		assert.Equal(current, expected.oidc)
		secret, err := manager.organizationOidcSecret(t.Context(), current)
		assert.NoErr(err)
		assert.Equal(secret, expected.secret)
		assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), expected.login))
	}
	manager.provisionPendingPocketId(t.Context(), map[string]bool{})
	assert.Equal(updatedClients[source.ID], 1)
	assert.Equal(updatedClients[shared.ClientID], 1)
	assert.Equal(createdClients, 1)
	assert.Equal(secretWrites, 1)
	policy, policyRecord, err := loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.NoErr(err)
	policy.PocketInstanceID = "other-installation"
	write, err := documentWrite("organization-login-policy", organization.ID, policy, policyRecord.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, err = manager.PrepareOrganizationLogin(t.Context(), "second", "")
	assert.True(err != nil)
}

// TestOrganizationOidcAfterCreation enables external sign-in after token-only setup and keeps first-administrator selection atomic and durable.
func TestOrganizationOidcAfterCreation(t *testing.T) {
	assert := is.New(t)
	manager, configuration, _, identity := organizationLoginFixture(t)
	defer func() { _ = manager.Close() }()
	organization, err := manager.OnboardOrganization(t.Context(), "later", OrganizationOnboardingInput{})
	assert.NoErr(err)
	input := OrganizationOidcInput{AdminEmail: "owner@example.test", IssuerURL: "http://invalid-provider.test", ClientID: "later-client", ClientSecret: "later-private-secret", BaseURL: "https://borq.test"}
	_, err = manager.PutOrganizationOidc(t.Context(), "later", input)
	assert.True(err != nil)
	_, _, err = loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", organization.ID)
	assert.True(errors.Is(err, ErrNotFound))
	input.IssuerURL = identity.Issuer
	saved, err := manager.PutOrganizationOidc(t.Context(), "later", input)
	assert.NoErr(err)
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	prepared, err := manager.PrepareOrganizationLogin(t.Context(), "later", "")
	assert.NoErr(err)
	for _, rejected := range []oidc.Identity{
		{Issuer: "https://other-provider.test", Subject: identity.Subject, Email: "other@example.test", EmailVerified: true, ExpiresAt: identity.ExpiresAt},
		{Issuer: identity.Issuer, Subject: identity.Subject, Email: "owner@example.test", EmailVerified: false, ExpiresAt: identity.ExpiresAt},
	} {
		_, err := manager.AuthorizeOrganizationLogin(t.Context(), "later", prepared.Generation, rejected)
		assert.True(err != nil)
	}
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "later", prepared.Generation, identity)
	assert.NoErr(err)
	assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	input.Revision = saved.Revision
	input.AdminEmail = "replacement@example.test"
	saved, err = manager.PutOrganizationOidc(t.Context(), "later", input)
	assert.NoErr(err)
	input.Revision = saved.Revision
	input.AdminEmail, input.ClientSecret = "", ""
	_, err = manager.PutOrganizationOidc(t.Context(), "later", input)
	assert.True(errors.Is(err, ErrInvalid))
	prepared, err = manager.PrepareOrganizationLogin(t.Context(), "later", "")
	assert.NoErr(err)
	_, err = manager.AuthorizeOrganizationLogin(t.Context(), "later", prepared.Generation, identity)
	assert.NoErr(err)
}

// TestOrganizationOidcPreservesLegacyAdministrator prevents late setup from detaching a managed provider or replacing a previously selected account.
func TestOrganizationOidcPreservesLegacyAdministrator(t *testing.T) {
	assert := is.New(t)
	manager, _, generation, identity := organizationLoginFixture(t)
	defer manager.Close()
	source, err := manager.organizationPocketIdSource(t.Context(), "team")
	assert.NoErr(err)
	identity.Subject, identity.Email, identity.EmailVerified = source.adminID(), source.Status.AdminEmail, false
	login, err := manager.AuthorizeOrganizationLogin(t.Context(), "team", generation, identity)
	assert.NoErr(err)
	saved, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	for _, email := range []string{source.Status.AdminEmail, "replacement@example.test"} {
		_, err := manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: email, IssuerURL: saved.IssuerURL, ClientID: saved.ClientID, BaseURL: saved.BaseURL, Revision: saved.Revision})
		assert.True(errors.Is(err, ErrConflict))
		assert.True(strings.Contains(err.Error(), "already has a managed sign-in provider"))
		assert.NoErr(manager.ValidateOrganizationLogin(t.Context(), login))
	}
	_, pocketRecord, err := manager.readPocketId(t.Context(), source.OrganizationID)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{{Collection: "organization-pocketid", Key: source.OrganizationID, ExpectedRevision: pocketRecord.Revision, Delete: true}}))
	_, err = manager.PutOrganizationOidc(t.Context(), "team", OrganizationOidcInput{AdminEmail: "replacement@example.test", IssuerURL: saved.IssuerURL, ClientID: saved.ClientID, BaseURL: saved.BaseURL, Revision: saved.Revision})
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "already has an enrolled administrator"))
	unchanged, err := manager.GetOrganizationOidc(t.Context(), "team")
	assert.NoErr(err)
	assert.Equal(unchanged, saved)
	_, _, err = loadDocument[organizationLoginPolicy](t.Context(), manager.database, "organization-login-policy", source.OrganizationID)
	assert.True(errors.Is(err, ErrNotFound))
}
