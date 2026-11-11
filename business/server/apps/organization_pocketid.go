package apps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

// OrganizationPocketId exposes managed installation progress without credentials.
type OrganizationPocketId struct {
	// Env identifies the concrete management secret publication selected for this addon.
	Env *EnvReference `json:"env,omitempty"`
	// Available reports whether Borq has a management cluster connection.
	Available bool `json:"available"`
	// State is empty, queued, provisioning, ready, or failed.
	State string `json:"state"`
	// Message gives safe, actionable provisioning progress.
	Message string `json:"message"`
	// IssuerURL is Pocket ID's browser-facing origin, fixed after installation.
	IssuerURL string `json:"issuerUrl"`
	// BaseURL is the Borq origin registered for this installation.
	BaseURL string `json:"borqPublicUrl"`
	// CertificateIssuer selects an existing cert-manager ClusterIssuer for HTTPS.
	CertificateIssuer string `json:"certificateIssuer"`
	// AdminUsername identifies the first administrator whose passkey needs enrollment.
	AdminUsername string `json:"adminUsername"`
	// AdminEmail is the address supplied for the first administrator.
	AdminEmail string `json:"adminEmail"`
}

// OrganizationPocketIdInput starts or retries one organization's managed provider setup.
type OrganizationPocketIdInput struct {
	// Configuration replaces editable environment values. Nil preserves them; an empty map clears overrides.
	Configuration map[string]string `json:"configuration,omitzero"`
	// ConfigurationRevision fences changes to the accepted workload and environment snapshots.
	ConfigurationRevision string `json:"configurationRevision,omitempty"`
	// ConfigurationBaseVersion fences concurrent secret publications.
	ConfigurationBaseVersion int `json:"configurationBaseVersion,omitempty"`
	// Env selects a management secret group; nil preserves the existing binding.
	Env *EnvReference `json:"env,omitempty"`
	// IssuerURL is a dedicated HTTPS origin, or HTTP localhost for development.
	IssuerURL string `json:"issuerUrl"`
	// BaseURL is Borq's public origin for login callbacks, logout returns, and application launch links.
	BaseURL string `json:"borqPublicUrl"`
	// CertificateIssuer names an existing ClusterIssuer; required for HTTPS.
	CertificateIssuer string `json:"certificateIssuer"`
	// AdminUsername defaults to a compatible email username or stable internal alias; explicit names are retained.
	AdminUsername string `json:"adminUsername,omitempty"`
	// AdminEmail identifies the initial human administrator.
	AdminEmail string `json:"adminEmail"`
	// OidcRevision fences automatic setup against changes to the existing provider.
	OidcRevision int64 `json:"oidcRevision"`
}

type organizationPocketIdRecord struct {
	Environment           pocketIdEnvironment  `json:"environment,omitzero"`
	AppliedEnvironmentRef string               `json:"appliedEnvironmentRef,omitempty"`
	ConfigurationApplied  bool                 `json:"configurationApplied,omitempty"`
	ClientLinksConfigured bool                 `json:"clientLinksConfigured,omitempty"`
	ValuesRef             string               `json:"valuesRef,omitempty"`
	AppliedValuesRef      string               `json:"appliedValuesRef,omitempty"`
	ID                    string               `json:"id"`
	Organization          string               `json:"organization"`
	OrganizationID        string               `json:"organizationId"`
	OidcRevision          int64                `json:"oidcRevision"`
	CredentialsStored     bool                 `json:"credentialsStored"`
	BackupConfigured      bool                 `json:"backupConfigured,omitempty"`
	Status                OrganizationPocketId `json:"status"`
}

func (manager Manager) pocketIdAvailable() bool {
	return manager.runtime.infrastructureEnabled || manager.runtime.managementKubeconfigPath != ""
}

func (manager Manager) readPocketId(ctx context.Context, organizationID string) (organizationPocketIdRecord, sqlite.Record, error) {
	stored, record, err := loadDocument[organizationPocketIdRecord](ctx, manager.database, "organization-pocketid", organizationID)
	if err != nil {
		return organizationPocketIdRecord{}, sqlite.Record{}, err
	}
	invalid := errors.New("managed Pocket ID state is invalid; review its ownership and stored configuration before recovery")
	for _, id := range []string{organizationID, stored.ID} {
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			return organizationPocketIdRecord{}, sqlite.Record{}, invalid
		}
	}
	for _, ref := range []string{stored.ValuesRef, stored.AppliedValuesRef} {
		if ref != "" && !validManagementSecretRef("pocketid-values", organizationID, ref) {
			return organizationPocketIdRecord{}, sqlite.Record{}, invalid
		}
	}
	if !stored.Environment.valid(organizationID) || (stored.AppliedEnvironmentRef != "" && !validManagementSecretRef("pocketid-environment", organizationID, stored.AppliedEnvironmentRef)) {
		return organizationPocketIdRecord{}, sqlite.Record{}, invalid
	}
	if stored.OrganizationID != organizationID {
		return organizationPocketIdRecord{}, sqlite.Record{}, invalid
	}
	organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", stored.Organization)
	if err != nil || organization.ID != organizationID || organization.Name != stored.Organization {
		return organizationPocketIdRecord{}, sqlite.Record{}, invalid
	}
	switch stored.Status.State {
	case "queued", "provisioning", "failed":
	case "ready":
		if !stored.CredentialsStored {
			return organizationPocketIdRecord{}, sqlite.Record{}, invalid
		}
	default:
		return organizationPocketIdRecord{}, sqlite.Record{}, invalid
	}
	if validatePocketIdInput(OrganizationPocketIdInput{IssuerURL: stored.Status.IssuerURL, BaseURL: stored.Status.BaseURL, CertificateIssuer: stored.Status.CertificateIssuer,
		AdminUsername: stored.Status.AdminUsername, AdminEmail: stored.Status.AdminEmail, OidcRevision: stored.OidcRevision}) != nil {
		return organizationPocketIdRecord{}, sqlite.Record{}, invalid
	}
	return stored, record, nil
}

// GetOrganizationPocketId returns configuration and durable setup progress for one organization.
func (manager Manager) GetOrganizationPocketId(ctx context.Context, name string) (OrganizationPocketId, error) {
	organization, _, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return OrganizationPocketId{}, err
	}
	stored, _, err := manager.readPocketId(ctx, organization.OrganizationID)
	if errors.Is(err, ErrNotFound) {
		return OrganizationPocketId{Available: manager.pocketIdAvailable()}, nil
	}
	stored.Status.Env = stored.Environment.reference(true)
	stored.Status.Available = manager.pocketIdAvailable()
	return stored.Status, err
}

// SetupOrganizationPocketId durably queues installation; browser disconnects do not cancel it.
func (manager Manager) SetupOrganizationPocketId(ctx context.Context, name string, input OrganizationPocketIdInput) (OrganizationPocketId, error) {
	return manager.setupOrganizationPocketId(ctx, name, input, nil, pocketIdEnvironment{})
}

func (manager Manager) setupOrganizationPocketId(ctx context.Context, name string, input OrganizationPocketIdInput, addon *ManagementPocketId, environment pocketIdEnvironment) (OrganizationPocketId, error) {
	if !manager.pocketIdAvailable() {
		return OrganizationPocketId{}, organizationOidcError{ErrInvalid, "Managed Pocket ID requires a management cluster. Bootstrap management first."}
	}
	input.IssuerURL, input.BaseURL = strings.TrimRight(strings.TrimSpace(input.IssuerURL), "/"), strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.AdminUsername, input.AdminEmail, input.CertificateIssuer = pocketIdAdminUsername(input.AdminUsername, input.AdminEmail), strings.TrimSpace(input.AdminEmail), strings.TrimSpace(input.CertificateIssuer)
	if err := validatePocketIdInput(input); err != nil {
		return OrganizationPocketId{}, organizationOidcError{ErrInvalid, err.Error()}
	}
	if err := manager.validateNetworkPocketIdName(input.IssuerURL); err != nil {
		return OrganizationPocketId{}, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return OrganizationPocketId{}, errors.New("application manager is stopping")
	}
	if address, err := url.Parse(input.IssuerURL); err == nil {
		if err := manager.checkAcceptedApplicationHostname(ctx, address.Hostname(), ""); err != nil {
			return OrganizationPocketId{}, err
		}
	}
	organization, oidcRecord, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return OrganizationPocketId{}, err
	}
	stored, record, err := manager.readPocketId(ctx, organization.OrganizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return OrganizationPocketId{}, err
	}
	// A resumed declarative setup may already have committed its generated OIDC.
	// Only that exact ready provider is reusable after the accepted revision advances.
	resumed := addon != nil && stored.Status.State == "ready" && organization.Settings.IssuerURL == input.IssuerURL && organization.Settings.BaseURL == input.BaseURL && organization.Settings.ClientID == stored.ID
	if oidcRecord.Revision != input.OidcRevision && !resumed {
		return OrganizationPocketId{}, organizationOidcError{ErrConflict, "OIDC settings changed; reload before setting up Pocket ID."}
	}
	if addon != nil {
		if addon.ValuesRef != "" && !validManagementSecretRef("pocketid-values", organization.OrganizationID, addon.ValuesRef) {
			return OrganizationPocketId{}, organizationOidcError{ErrInvalid, "Pocket ID values reference has invalid ownership."}
		}
		if record.Revision != 0 {
			if err := matchManagementPocketId(stored, *addon, ManagementOidc{BaseURL: input.BaseURL}); err != nil {
				return OrganizationPocketId{}, err
			}
		}
	}
	if record.Revision != 0 && (stored.Status.IssuerURL != input.IssuerURL || stored.Status.BaseURL != input.BaseURL || stored.Status.AdminUsername != input.AdminUsername || stored.Status.AdminEmail != input.AdminEmail) {
		return OrganizationPocketId{}, organizationOidcError{ErrConflict, "This organization already has a managed Pocket ID. Its address and initial administrator cannot be replaced here."}
	}
	valuesRef := stored.ValuesRef
	if addon != nil {
		valuesRef = addon.ValuesRef
	}
	if input.Configuration != nil {
		stored.OrganizationID = organization.OrganizationID
		environment, valuesRef, err = manager.publishPocketIdConfiguration(ctx, name, stored, input)
		if err != nil {
			return OrganizationPocketId{}, err
		}
	}
	if addon == nil && input.Env != nil {
		environment, err = manager.resolvePocketIdEnvironment(ctx, organization.OrganizationID, *input.Env)
		if err != nil {
			return OrganizationPocketId{}, err
		}
	}
	if !environment.valid(organization.OrganizationID) || (addon != nil && addon.Env != nil && (environment.Name != addon.Env.Name || environment.Name == "")) {
		return OrganizationPocketId{}, organizationOidcError{ErrInvalid, "Pocket ID environment reference has invalid ownership."}
	}
	if environment.Name != "" {
		if _, err := manager.readPocketIdEnvironment(ctx, organization.OrganizationID, environment); err != nil {
			return OrganizationPocketId{}, err
		}
	}
	if record.Revision != 0 {
		if stored.Status.State != "failed" {
			changed := environment.Name != "" && stored.Environment != environment
			if changed {
				stored.Environment = environment
			}
			if stored.ValuesRef != valuesRef {
				stored.ValuesRef = valuesRef
				changed = true
			}
			stored.Status.Env = stored.Environment.reference(true)
			if changed {
				write, err := documentWrite("organization-pocketid", stored.OrganizationID, stored, record.Revision)
				if err != nil {
					return OrganizationPocketId{}, err
				}
				if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
					return OrganizationPocketId{}, translateStoreError(err)
				}
				select {
				case manager.runtime.pocketIdWake <- struct{}{}:
				default:
				}
			}
			stored.Status.Available = true
			return stored.Status, nil
		}
	} else {
		id, err := newId()
		if err != nil {
			return OrganizationPocketId{}, err
		}
		stored = organizationPocketIdRecord{ID: id, Organization: name, OrganizationID: organization.OrganizationID}
	}
	stored.ValuesRef = valuesRef
	if environment.Name != "" {
		stored.Environment = environment
	}
	stored.ConfigurationApplied = false
	stored.OidcRevision = input.OidcRevision
	stored.Status = OrganizationPocketId{Env: stored.Environment.reference(true), Available: true, State: "queued", Message: "Pocket ID setup is queued.", IssuerURL: input.IssuerURL, BaseURL: input.BaseURL, CertificateIssuer: input.CertificateIssuer, AdminUsername: input.AdminUsername, AdminEmail: input.AdminEmail}
	write, err := documentWrite("organization-pocketid", stored.OrganizationID, stored, record.Revision)
	if err != nil {
		return OrganizationPocketId{}, err
	}
	writes := []sqlite.Write{write}
	if record.Revision == 0 {
		issuer, _ := url.Parse(input.IssuerURL)
		reservation, err := documentWrite("organization-pocketid-host", issuer.Hostname(), map[string]string{"organizationId": stored.OrganizationID, "instanceId": stored.ID}, 0)
		if err != nil {
			return OrganizationPocketId{}, err
		}
		writes = append(writes, reservation)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		if errors.Is(err, sqlite.ErrConflict) {
			return OrganizationPocketId{}, organizationOidcError{ErrConflict, "Pocket ID setup changed or its hostname is already reserved. Reload and use a dedicated hostname."}
		}
		return OrganizationPocketId{}, translateStoreError(err)
	}
	select {
	case manager.runtime.pocketIdWake <- struct{}{}:
	default:
	}
	return stored.Status, nil
}

func pocketIdAdminUsername(username, email string) string {
	if strings.TrimSpace(username) != "" {
		return strings.TrimSpace(username)
	}
	email = strings.TrimSpace(email)
	if validPocketIdUsername(email) {
		return email
	}
	// Upstream usernames exclude valid email characters such as '+' and cap
	// length at 50. Keep email-only setup usable with a stable internal alias.
	digest := sha256.Sum256([]byte(email))
	return fmt.Sprintf("user-%x", digest[:10])
}

func validPocketIdUsername(username string) bool {
	return len(username) <= 50 && regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.@-]*[a-zA-Z0-9])?$`).MatchString(username)
}

func validatePocketIdInput(input OrganizationPocketIdInput) error {
	if input.Configuration != nil {
		if input.Env != nil {
			return errors.New("Supply Pocket ID configuration or a secret group reference, not both")
		}
		if input.ConfigurationRevision == "" || input.ConfigurationBaseVersion < 0 {
			return errors.New("Reload Pocket ID configuration before saving changes")
		}
		if err := platform.ValidatePocketIdEnvironment(input.Configuration); err != nil {
			return err
		}
	}
	if input.Env != nil {
		if err := validateConfigName(input.Env.Name); err != nil {
			return err
		}
		if _, err := configVersion(input.Env.Version); err != nil {
			return err
		}
	}
	for _, address := range []string{input.IssuerURL, input.BaseURL} {
		if err := oidc.ValidateURL(address); err != nil {
			return err
		}
		parsed, _ := url.Parse(address)
		if strings.HasSuffix(parsed.Host, ":") {
			return errors.New("Enter a valid port in the range 1 to 65535")
		}
		if parsed.Port() != "" {
			port, err := strconv.Atoi(parsed.Port())
			if err != nil || port < 1 || port > 65535 {
				return errors.New("Enter a valid port in the range 1 to 65535")
			}
		}
		if parsed.Path != "" || len(address) > 2048 {
			return errors.New("Enter Pocket ID and Borq addresses without a path")
		}
	}
	issuer, _ := url.Parse(input.IssuerURL)
	borq, _ := url.Parse(input.BaseURL)
	if net.ParseIP(issuer.Hostname()) != nil || len(validation.IsDNS1123Subdomain(issuer.Hostname())) != 0 {
		return errors.New("Pocket ID requires a DNS hostname; use localhost for local development")
	}
	if strings.EqualFold(issuer.Hostname(), borq.Hostname()) && (issuer.Hostname() != "localhost" || issuer.Port() == borq.Port()) {
		return errors.New("Pocket ID needs its own address, separate from Borq")
	}
	if issuer.Scheme == "https" && input.CertificateIssuer == "" {
		return errors.New("Select an existing certificate issuer for Pocket ID HTTPS")
	}
	if input.CertificateIssuer != "" && len(validation.IsDNS1123Subdomain(input.CertificateIssuer)) != 0 {
		return errors.New("Enter a valid certificate issuer name")
	}
	username := pocketIdAdminUsername(input.AdminUsername, input.AdminEmail)
	// Pocket ID v2.19.1 accepts email usernames within its username character and length limits.
	if !validPocketIdUsername(username) {
		return errors.New("Pocket ID requires an administrator username of at most 50 letters, numbers, dots, underscores, @ or hyphens, starting and ending with a letter or number; omit username to derive it from the administrator email")
	}
	address, err := mail.ParseAddress(input.AdminEmail)
	if err != nil || address.Address != input.AdminEmail || len(input.AdminEmail) > 254 || input.OidcRevision < 0 {
		return errors.New("Enter a valid administrator email and reload the current OIDC settings")
	}
	return nil
}

func (stored organizationPocketIdRecord) credentialsPath() string {
	return "borq/organization-pocketid/" + stored.OrganizationID + "/" + stored.ID
}

func (stored organizationPocketIdRecord) adminID() string {
	return stored.ID[:8] + "-" + stored.ID[8:12] + "-" + stored.ID[12:16] + "-" + stored.ID[16:20] + "-" + stored.ID[20:]
}

func (manager Manager) pocketIdCredentials(ctx context.Context, stored organizationPocketIdRecord) (map[string]string, error) {
	current, _, err := manager.readPocketId(ctx, stored.OrganizationID)
	if err != nil || current.ID != stored.ID {
		return nil, errors.New("Managed Pocket ID credential ownership is invalid.")
	}
	stored = current
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return nil, errors.New("Management secret storage is unavailable. Retry when OpenBao is ready.")
	}
	entry, err := vault.ReadKv(ctx, "secret", stored.credentialsPath(), 1)
	if errors.Is(err, openbao.ErrNotFound) {
		if stored.CredentialsStored {
			return nil, errors.New("Pocket ID credentials are missing. Restore management secret storage before retrying; existing keys will not be replaced.")
		}
		// Save before deploying: retries must never replace an instance's encryption key.
		values := map[string]string{}
		for _, key := range []string{"encryptionKey", "apiKey", "clientSecret"} {
			secret := make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return nil, err
			}
			values[key] = base64.RawURLEncoding.EncodeToString(secret)
		}
		archive, generationErr := sqliteworkload.NewArchiveKey()
		if generationErr != nil {
			return nil, generationErr
		}
		values["ageIdentity"], values["ageRecipient"] = archive.Identity, archive.Recipient
		if _, err := vault.WriteKv(ctx, "secret", stored.credentialsPath(), openbao.WriteKvInput{Values: values, CheckVersion: 0}); err != nil {
			return nil, errors.New("Could not store Pocket ID credentials. Retry setup.")
		}
		entry.Values, err = values, nil
	}
	if err != nil || len(entry.Values["encryptionKey"]) < 32 || len(entry.Values["apiKey"]) < 32 || len(entry.Values["clientSecret"]) < 32 {
		return nil, errors.New("Pocket ID credentials are unavailable. Restore management secret storage before retrying.")
	}
	archive := sqliteworkload.ArchiveKey{Identity: entry.Values["ageIdentity"], Recipient: entry.Values["ageRecipient"]}
	if err := archive.Validate(); err != nil {
		return nil, err
	}
	if !stored.CredentialsStored {
		current, record, err := manager.readPocketId(ctx, stored.OrganizationID)
		if err != nil {
			return nil, errors.New("Could not record Pocket ID credential storage. Retry setup.")
		}
		current.CredentialsStored = true
		write, err := documentWrite("organization-pocketid", stored.OrganizationID, current, record.Revision)
		if err != nil || manager.database.Commit(ctx, []sqlite.Write{write}) != nil {
			return nil, errors.New("Could not record Pocket ID credential storage. Retry setup.")
		}
	}
	return entry.Values, nil
}

func (manager Manager) pocketIdClient(stored organizationPocketIdRecord, apiKey string) (pocketid.Client, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
	if err != nil {
		return pocketid.Client{}, err
	}
	configuration := connection.RestConfig()
	client, err := rest.HTTPClientFor(&configuration)
	if err != nil {
		return pocketid.Client{}, err
	}
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	return pocketid.New(pocketid.Config{BaseURL: strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:pocketid-" + stored.ID + ":1411/proxy", ApiKey: apiKey, HttpClient: client})
}

// ErrPocketIdAlreadyEnrolled lets declarative setup preserve an administrator's existing passkeys.
var ErrPocketIdAlreadyEnrolled = errors.New("Pocket ID administrator already has a passkey")

// PocketIdAdminEnrollment creates a short-lived initial passkey link only on explicit administrator action.
func (manager Manager) PocketIdAdminEnrollment(ctx context.Context, name string) (string, error) {
	organization, _, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return "", err
	}
	stored, _, err := manager.readPocketId(ctx, organization.OrganizationID)
	if err != nil {
		return "", err
	}
	if stored.Status.State != "ready" {
		return "", organizationOidcError{ErrConflict, "Wait for Pocket ID setup to finish before enrolling the administrator."}
	}
	credentials, err := manager.pocketIdCredentials(ctx, stored)
	if err != nil {
		return "", err
	}
	client, err := manager.pocketIdClient(stored, credentials["apiKey"])
	if err != nil {
		return "", err
	}
	enrolled, err := client.HasPasskey(ctx, stored.adminID())
	if err != nil {
		return "", err
	}
	if enrolled {
		return "", errors.Join(ErrPocketIdAlreadyEnrolled, organizationOidcError{ErrConflict, "The administrator already has a passkey. Open Pocket ID to sign in."})
	}
	token, err := client.NewOneTimeAccess(ctx, stored.adminID())
	if err != nil {
		return "", err
	}
	return stored.Status.IssuerURL + "/lc/" + url.PathEscape(token), nil
}

func (manager Manager) startPocketIdWorker() {
	if !manager.pocketIdAvailable() {
		return
	}
	manager.runtime.workers.Add(1)
	go func() {
		defer manager.runtime.workers.Done()
		// Durable readiness survives teardown; process-local receipts ensure its
		// retained identity resources are reconciled again after every restart.
		reconciled := map[string]bool{}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			manager.provisionPendingPocketId(manager.runtime.context, reconciled)
			select {
			case <-manager.runtime.context.Done():
				return
			case <-ticker.C:
			case <-manager.runtime.pocketIdWake:
			}
		}
	}()
}

func (manager Manager) provisionPendingPocketId(ctx context.Context, reconciled map[string]bool) {
	manager.provisionOrganizationPocketIdClients(ctx)
	records, err := manager.database.List(ctx, "organization-pocketid")
	if err != nil {
		return
	}
	for _, record := range records {
		stored, _, err := manager.readPocketId(ctx, record.Key)
		if err != nil {
			continue
		}
		if stored.Status.State == "ready" && !stored.ClientLinksConfigured {
			if err := manager.configurePocketIdClientLinks(ctx, stored); err != nil {
				_, _ = manager.pocketIdProgress(ctx, stored.OrganizationID, "ready", "Pocket ID is ready, but Borq's logout and launcher links could not be configured. Check the managed instance and retry.")
			}
			stored, _, err = manager.readPocketId(ctx, record.Key)
			if err != nil {
				continue
			}
		}
		upgradeBackup := stored.Status.State == "ready" && (!stored.BackupConfigured || !stored.ConfigurationApplied || stored.ValuesRef != stored.AppliedValuesRef || stored.Environment.CredentialsRef != stored.AppliedEnvironmentRef)
		recoverReady := stored.Status.State == "ready" && !reconciled[stored.ID]
		if !upgradeBackup && !recoverReady && stored.Status.State != "queued" && stored.Status.State != "provisioning" {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if upgradeBackup || recoverReady {
			// Existing providers may already have users and changed OIDC settings.
			// Updating configuration or backups must not repeat first-admin setup or replace OIDC.
			var deployment platform.PocketIdInput
			var credentials map[string]string
			deployment, credentials, err = manager.installPocketId(attempt, stored)
			if err == nil && recoverReady {
				err = manager.republishReadyPocketId(attempt, stored, deployment, credentials)
			}
		} else {
			err = manager.provisionPocketId(attempt, stored)
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			state := "failed"
			if upgradeBackup || recoverReady {
				state = "ready"
			}
			_, _ = manager.pocketIdProgress(ctx, stored.OrganizationID, state, err.Error())
		} else {
			reconciled[stored.ID] = true
			if upgradeBackup || recoverReady {
				_, _ = manager.pocketIdProgress(ctx, stored.OrganizationID, "ready", "Pocket ID is ready. SQLite backups to management RustFS are configured.")
			}
		}
	}
}

func (manager Manager) republishReadyPocketId(ctx context.Context, stored organizationPocketIdRecord, deployment platform.PocketIdInput, credentials map[string]string) error {
	client, err := manager.pocketIdClient(stored, credentials["apiKey"])
	if err != nil {
		return err
	}
	callback := stored.Status.BaseURL + "/api/auth/oidc/callback/" + stableId(stored.OrganizationID+"/"+stored.Status.IssuerURL)
	// Retained storage alone cannot prove its database still contains this
	// identity. Verify the original client and secret before exposing it again.
	if err := client.VerifyClient(ctx, pocketid.OidcClient{ID: stored.ID, CallbackURL: callback, Secret: credentials["clientSecret"],
		LogoutCallbackURL: stored.Status.BaseURL + "/login", LaunchURL: stored.Status.BaseURL + "/"}); err != nil {
		return errors.New("Could not verify the retained Pocket ID identity. Restore its original data and client credentials before retrying.")
	}
	return platform.PublishPocketId(ctx, deployment)
}

func (manager Manager) configurePocketIdClientLinks(ctx context.Context, stored organizationPocketIdRecord) error {
	current, record, err := manager.readPocketId(ctx, stored.OrganizationID)
	if err != nil || current.ID != stored.ID || current.Status.State != "ready" {
		return organizationLoginDenied()
	}
	credentials, err := manager.pocketIdCredentials(ctx, current)
	if err != nil {
		return err
	}
	client, err := manager.pocketIdClient(current, credentials["apiKey"])
	if err != nil {
		return err
	}
	callback := current.Status.BaseURL + "/api/auth/oidc/callback/" + stableId(current.OrganizationID+"/"+current.Status.IssuerURL)
	if err := client.EnsureClientLinks(ctx, pocketid.OidcClient{ID: current.ID, CallbackURL: callback,
		LogoutCallbackURL: current.Status.BaseURL + "/login", LaunchURL: current.Status.BaseURL + "/"}); err != nil {
		return err
	}
	current.ClientLinksConfigured = true
	current.Status.Message = "Pocket ID is ready. Borq's logout return and launcher links are configured."
	write, err := documentWrite("organization-pocketid", current.OrganizationID, current, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) pocketIdProgress(ctx context.Context, organizationID, state, message string) (sqlite.Record, error) {
	stored, record, err := manager.readPocketId(ctx, organizationID)
	if err != nil {
		return sqlite.Record{}, err
	}
	stored.Status.State, stored.Status.Message = state, message
	write, err := documentWrite("organization-pocketid", organizationID, stored, record.Revision)
	if err != nil {
		return sqlite.Record{}, err
	}
	return record, manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) provisionPocketId(ctx context.Context, stored organizationPocketIdRecord) error {
	if _, err := manager.pocketIdProgress(ctx, stored.OrganizationID, "provisioning", "Preparing private storage and starting Pocket ID."); err != nil {
		return errors.New("Could not save Pocket ID setup progress. Retry setup.")
	}
	deployment, credentials, err := manager.installPocketId(ctx, stored)
	if err != nil {
		return err
	}
	client, err := manager.pocketIdClient(stored, credentials["apiKey"])
	if err != nil {
		return errors.New("Could not connect to Pocket ID through the management cluster.")
	}
	if err := client.EnsureAdmin(ctx, pocketid.Admin{ID: stored.adminID(), Username: stored.Status.AdminUsername, Email: stored.Status.AdminEmail}); err != nil {
		return errors.New("Could not configure the Pocket ID administrator. Check the managed instance, then retry.")
	}
	callback := stored.Status.BaseURL + "/api/auth/oidc/callback/" + stableId(stored.OrganizationID+"/"+stored.Status.IssuerURL)
	// Pocket ID limits display names to 50 characters; the immutable ID carries identity.
	clientName := "Borq " + stored.Organization
	if len(clientName) > 50 {
		clientName = clientName[:50]
	}
	if err := client.EnsureClient(ctx, pocketid.OidcClient{ID: stored.ID, Name: clientName, CallbackURL: callback, Secret: credentials["clientSecret"],
		LogoutCallbackURL: stored.Status.BaseURL + "/login", LaunchURL: stored.Status.BaseURL + "/"}); err != nil {
		return errors.New("Could not configure Borq's Pocket ID client. Check the managed instance, then retry.")
	}
	if _, err := manager.pocketIdProgress(ctx, stored.OrganizationID, "provisioning", "Administrator and client created. Publishing Pocket ID and checking its address and certificate."); err != nil {
		return errors.New("Could not save Pocket ID setup progress. Retry setup.")
	}
	// Keep the ingress private until the administrator and its client both exist.
	if err := platform.PublishPocketId(ctx, deployment); err != nil {
		return errors.New("Could not publish Pocket ID. Check the hostname, certificate issuer and existing routes, then retry.")
	}
	// DNS and certificate controllers finish asynchronously. Discovery must succeed before replacing OIDC.
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		_, err = oidc.New(ctx, oidc.Config{Issuer: stored.Status.IssuerURL, ClientID: stored.ID, ClientSecret: credentials["clientSecret"], RedirectURL: callback})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("Pocket ID is deployed, but its public address or certificate is not ready. Check DNS and the certificate issuer, then retry setup.")
		case <-ticker.C:
		}
	}
	return manager.completePocketIdSetup(ctx, stored, credentials["clientSecret"])
}

// Completion and OIDC credentials advance in one transaction, so neither can claim success alone.
func (manager Manager) completePocketIdSetup(ctx context.Context, stored organizationPocketIdRecord, clientSecret string) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	current, record, err := manager.readPocketId(ctx, stored.OrganizationID)
	if err != nil {
		return errors.New("Could not read Pocket ID setup progress. Retry setup.")
	}
	if current.ID != stored.ID || current.OidcRevision != stored.OidcRevision || current.Status.IssuerURL != stored.Status.IssuerURL || current.Status.BaseURL != stored.Status.BaseURL {
		return organizationOidcError{ErrConflict, "Pocket ID setup changed while provisioning. Reload authentication settings."}
	}
	current.Status.State, current.Status.Message = "ready", "Pocket ID is ready and OIDC settings are filled in. Enroll the administrator's passkey, then test sign-in."
	current.ClientLinksConfigured = true
	write, err := documentWrite("organization-pocketid", stored.OrganizationID, current, record.Revision)
	if err != nil {
		return errors.New("Could not save Pocket ID setup progress. Retry setup.")
	}
	writes := []sqlite.Write{write}
	policy, policyRecord, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", current.OrganizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if policyRecord.Revision != 0 {
		if policy.OrganizationID != current.OrganizationID || policy.Issuer != current.Status.IssuerURL ||
			policy.BaseURL != current.Status.BaseURL || policy.PocketOrganizationID != "" ||
			(policy.PocketInstanceID != "" && (policy.PocketInstanceID != current.ID || policy.ClientID != current.ID)) {
			return organizationOidcError{ErrConflict, "The organization's administrator policy belongs to another provider; setup cannot replace it."}
		}
		if policy.PocketInstanceID == "" {
			// Management YAML can select a Borq administrator before the managed client has an ID.
			policy.ClientID, policy.PocketInstanceID = current.ID, current.ID
			policyWrite, err := documentWrite("organization-login-policy", current.OrganizationID, policy, policyRecord.Revision)
			if err != nil {
				return err
			}
			writes = append(writes, policyWrite)
		}
	}
	organization, organizationRecord, err := loadDocument[Organization](ctx, manager.database, "organization", stored.Organization)
	if err != nil || organization.ID != stored.OrganizationID {
		return organizationLoginDenied()
	}
	if organization.AuthenticationState == "queued" {
		organization.AuthenticationState = "ready"
		organizationWrite, err := documentWrite("organization", organization.Name, organization, organizationRecord.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, organizationWrite)
	}
	_, err = manager.putOrganizationOidc(ctx, stored.Organization, OrganizationOidcInput{IssuerURL: stored.Status.IssuerURL, ClientID: stored.ID, ClientSecret: clientSecret, BaseURL: stored.Status.BaseURL, Revision: stored.OidcRevision}, writes)
	if errors.Is(err, ErrConflict) {
		return organizationOidcError{ErrConflict, "OIDC settings changed during setup. Reload and retry Pocket ID setup to replace the current provider explicitly."}
	}
	if err != nil {
		return errors.New("Pocket ID is deployed, but OIDC settings could not be saved. Check management secret storage and retry setup.")
	}
	return nil
}
