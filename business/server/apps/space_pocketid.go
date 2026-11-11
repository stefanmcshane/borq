package apps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PocketIdInput configures a Space-owned SQLite identity provider.
type PocketIdInput struct {
	// IssuerURL is the permanent HTTPS origin used by passkeys and OIDC clients.
	IssuerURL string `json:"issuerUrl"`
	// AdminUsername names the initial administrator; later edits belong in Pocket ID.
	AdminUsername string `json:"adminUsername"`
	// AdminEmail is the initial administrator's contact address.
	AdminEmail string `json:"adminEmail"`
	// Storage reserves local persistent storage and defaults to 1Gi.
	Storage string `json:"storage,omitempty"`
	// StorageClass chooses a block-backed volume; empty uses the cluster default.
	StorageClass string `json:"storageClass,omitempty"`
	// Image pins an official Pocket ID v2 release; updates cannot downgrade it.
	Image string `json:"image,omitempty"`
	// CertificateIssuer selects a ClusterIssuer; empty uses the Space's issuer.
	CertificateIssuer string `json:"certificateIssuer,omitempty"`
}

// PocketId reports accepted configuration without credentials or enrollment links.
type PocketId struct {
	// ID is the immutable instance identity, retained after removal.
	ID string `json:"id"`
	// Name identifies this provider within its Space.
	Name string `json:"name"`
	// Namespace owns the StatefulSet and retained SQLite volume.
	Namespace string `json:"namespace"`
	// Specification is the accepted provider configuration.
	Specification PocketIdInput `json:"specification"`
	// State is pending, ready, failed, deleting, or removed.
	State string `json:"state"`
	// Message explains the last accepted change or reconciliation result.
	Message string `json:"message"`
	// Operation identifies the latest durable operation.
	Operation string `json:"operation"`
	// UpdatedAt records the latest state transition.
	UpdatedAt time.Time `json:"updatedAt"`
}

type spacePocketIdRecord struct {
	PocketId        PocketId `json:"pocketId"`
	Organization    string   `json:"organization"`
	OrganizationID  string   `json:"organizationId"`
	Space           string   `json:"space"`
	SpaceID         string   `json:"spaceId"`
	Cluster         string   `json:"cluster"`
	CredentialsRef  string   `json:"credentialsRef"`
	ActiveOperation string   `json:"activeOperation"`
	Initialized     bool     `json:"initialized"`
}

type spacePocketIdTask struct {
	Key    string `json:"key"`
	Remove bool   `json:"remove"`
}

func normalizeSpacePocketId(input PocketIdInput) (PocketIdInput, error) {
	input.IssuerURL = strings.TrimRight(strings.TrimSpace(input.IssuerURL), "/")
	address, err := url.Parse(input.IssuerURL)
	if err != nil || address.Scheme != "https" || address.Host == "" || len(input.IssuerURL) > 2048 || net.ParseIP(address.Hostname()) != nil || strings.HasSuffix(address.Host, ":") || address.User != nil || address.Path != "" || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || address.Port() != "" && address.Port() != "443" || len(validation.IsDNS1123Subdomain(address.Hostname())) != 0 {
		return input, fmt.Errorf("%w: Pocket ID requires a dedicated HTTPS origin", ErrInvalid)
	}
	input.AdminUsername = strings.TrimSpace(input.AdminUsername)
	if input.AdminUsername == "" {
		input.AdminUsername = "admin"
	}
	input.AdminEmail = strings.TrimSpace(input.AdminEmail)
	email, err := mail.ParseAddress(input.AdminEmail)
	if !validPocketIdUsername(input.AdminUsername) || err != nil || email.Address != input.AdminEmail || len(input.AdminEmail) > 254 {
		return input, fmt.Errorf("%w: Pocket ID requires an administrator username and email address", ErrInvalid)
	}
	if input.Storage == "" {
		input.Storage = "1Gi"
	}
	capacity, err := resource.ParseQuantity(input.Storage)
	if err != nil || capacity.Sign() <= 0 {
		return input, fmt.Errorf("%w: Pocket ID storage must be a positive Kubernetes quantity", ErrInvalid)
	}
	if input.StorageClass != "" && len(validation.IsDNS1123Subdomain(input.StorageClass)) != 0 {
		return input, fmt.Errorf("%w: invalid storage class", ErrInvalid)
	}
	if input.CertificateIssuer != "" && len(validation.IsDNS1123Subdomain(input.CertificateIssuer)) != 0 {
		return input, fmt.Errorf("%w: invalid certificate issuer", ErrInvalid)
	}
	if input.Image == "" {
		input.Image = "ghcr.io/pocket-id/pocket-id:v2.19.1@sha256:f085157f0b6ea4b2e8aa5ecfd99eada0d83a4960f3a3dcfd46014097d0ef7ab5"
	}
	if !regexp.MustCompile(`^ghcr\.io/pocket-id/pocket-id:v2\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)@sha256:[a-f0-9]{64}$`).MatchString(input.Image) {
		return input, fmt.Errorf("%w: image must pin an official Pocket ID v2 release and sha256 digest", ErrInvalid)
	}
	return input, nil
}

func mergeSpacePocketId(previous, input PocketIdInput) PocketIdInput {
	if input.IssuerURL == "" {
		input.IssuerURL = previous.IssuerURL
	}
	if input.AdminUsername == "" {
		input.AdminUsername = previous.AdminUsername
	}
	if input.AdminEmail == "" {
		input.AdminEmail = previous.AdminEmail
	}
	if input.Storage == "" {
		input.Storage = previous.Storage
	}
	if input.StorageClass == "" {
		input.StorageClass = previous.StorageClass
	}
	if input.Image == "" {
		input.Image = previous.Image
	}
	if input.CertificateIssuer == "" {
		input.CertificateIssuer = previous.CertificateIssuer
	}
	return input
}

func spacePocketIdVersion(image string) int64 {
	tag := strings.TrimPrefix(strings.Split(image, "@")[0], "ghcr.io/pocket-id/pocket-id:v2.")
	parts := strings.Split(tag, ".")
	if len(parts) != 2 {
		return -1
	}
	minor, _ := strconv.ParseInt(parts[0], 10, 32)
	patch, _ := strconv.ParseInt(parts[1], 10, 32)
	return minor<<32 | patch
}

// ListPocketId lists providers accepted for exactly one existing Space.
func (manager Manager) ListPocketId(ctx context.Context, organization, space string) ([]PocketId, error) {
	if _, err := manager.GetSpace(ctx, organization, space); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "space-pocketid")
	if err != nil {
		return nil, err
	}
	result := []PocketId{}
	for _, record := range records {
		var stored spacePocketIdRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.Organization == organization && stored.Space == space && stored.PocketId.State != "removed" {
			result = append(result, stored.PocketId)
		}
	}
	return result, nil
}

// GetPocketId returns a Space's accepted provider without reading its secrets.
func (manager Manager) GetPocketId(ctx context.Context, organization, space, name string) (PocketId, error) {
	stored, _, err := manager.readSpacePocketId(ctx, organization, space, name)
	if err != nil {
		return PocketId{}, err
	}
	if stored.PocketId.State == "removed" {
		return PocketId{}, ErrNotFound
	}
	return stored.PocketId, nil
}

func (manager Manager) readSpacePocketId(ctx context.Context, organization, space, name string) (spacePocketIdRecord, sqlite.Record, error) {
	target, err := manager.GetSpace(ctx, organization, space)
	if err != nil {
		return spacePocketIdRecord{}, sqlite.Record{}, err
	}
	stored, record, err := loadDocument[spacePocketIdRecord](ctx, manager.database, "space-pocketid", addonKey(organization, space, name))
	if err != nil {
		return stored, record, err
	}
	if stored.SpaceID != target.ID || stored.Organization != organization || stored.Space != space || stored.Cluster != target.Cluster || stored.PocketId.Namespace != target.Namespace {
		return stored, record, fmt.Errorf("%w: Pocket ID ownership changed", ErrConflict)
	}
	return stored, record, nil
}

// PutPocketId queues installation or an ordered rollover of the existing SQLite provider.
func (manager Manager) PutPocketId(ctx context.Context, organization, space, name string, input PocketIdInput, apply bool) (PocketId, error) {
	return manager.acceptSpacePocketId(ctx, organization, space, name, input, apply, false)
}

// DeletePocketId removes serving resources while retaining the database and encryption key.
func (manager Manager) DeletePocketId(ctx context.Context, organization, space, name string) (PocketId, error) {
	return manager.acceptSpacePocketId(ctx, organization, space, name, PocketIdInput{}, false, true)
}

func (manager Manager) acceptSpacePocketId(ctx context.Context, organization, space, name string, input PocketIdInput, apply, remove bool) (PocketId, error) {
	if err := validateNames(organization, space, name); err != nil {
		return PocketId{}, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing || manager.runtime.destroying {
		return PocketId{}, fmt.Errorf("%w: manager is stopping", ErrConflict)
	}
	target, err := manager.GetSpace(ctx, organization, space)
	if err != nil {
		return PocketId{}, err
	}
	org, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization)
	if err != nil {
		return PocketId{}, err
	}
	if !remove && !org.ManagedAddons {
		return PocketId{}, fmt.Errorf("%w: managed addons are disabled for this organization", ErrConflict)
	}
	key := addonKey(organization, space, name)
	stored, record, err := manager.readSpacePocketId(ctx, organization, space, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return PocketId{}, err
	}
	exists := err == nil
	if remove && (!exists || stored.PocketId.State == "removed") {
		return PocketId{}, ErrNotFound
	}
	if exists && stored.ActiveOperation != "" {
		return PocketId{}, fmt.Errorf("%w: Pocket ID operation is still running", ErrConflict)
	}
	if exists && !apply && !remove {
		return PocketId{}, fmt.Errorf("%w: Pocket ID already exists; apply to reuse its retained data", ErrConflict)
	}
	if exists && stored.OrganizationID != org.ID {
		return PocketId{}, fmt.Errorf("%w: Pocket ID organization ownership changed", ErrConflict)
	}
	if !remove {
		if exists {
			input = mergeSpacePocketId(stored.PocketId.Specification, input)
		}
		if input.CertificateIssuer == "" {
			input.CertificateIssuer = target.CertificateIssuer
		}
		input, err = normalizeSpacePocketId(input)
		if err != nil {
			return PocketId{}, err
		}
		if exists {
			previous := stored.PocketId.Specification
			if input.IssuerURL != previous.IssuerURL || input.AdminUsername != previous.AdminUsername || input.AdminEmail != previous.AdminEmail || input.Storage != previous.Storage || input.StorageClass != previous.StorageClass {
				return PocketId{}, fmt.Errorf("%w: issuer, initial administrator, storage and storage class are immutable", ErrInvalid)
			}
			if spacePocketIdVersion(input.Image) < spacePocketIdVersion(previous.Image) {
				return PocketId{}, fmt.Errorf("%w: Pocket ID database migrations cannot be downgraded", ErrInvalid)
			}
		}
	}
	if !exists {
		id, err := newId()
		if err != nil {
			return PocketId{}, err
		}
		credentials := map[string]string{}
		for _, name := range []string{"encryptionKey", "apiKey"} {
			secret := make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return PocketId{}, err
			}
			credentials[name] = base64.RawURLEncoding.EncodeToString(secret)
		}
		archive, err := sqliteworkload.NewArchiveKey()
		if err != nil {
			return PocketId{}, err
		}
		credentials["ageIdentity"], credentials["ageRecipient"] = archive.Identity, archive.Recipient
		encoded, err := json.Marshal(credentials)
		if err != nil {
			return PocketId{}, err
		}
		ref, err := manager.stageManagementSecret(ctx, "space-pocketid", org.ID, "credentials", string(encoded))
		if err != nil {
			return PocketId{}, err
		}
		stored = spacePocketIdRecord{Organization: organization, OrganizationID: org.ID, Space: space, SpaceID: target.ID, Cluster: target.Cluster, CredentialsRef: ref, PocketId: PocketId{ID: id, Name: name, Namespace: target.Namespace}}
	}
	if !remove {
		stored.PocketId.Specification = input
	}
	id, err := newId()
	if err != nil {
		return PocketId{}, err
	}
	now := time.Now().UTC()
	stored.ActiveOperation, stored.PocketId.Operation, stored.PocketId.UpdatedAt = id, id, now
	stored.PocketId.State, stored.PocketId.Message = "pending", "Pocket ID ordered SQLite rollout accepted; a brief sign-in interruption is expected."
	if remove {
		stored.PocketId.State, stored.PocketId.Message = "deleting", "Removal accepted; SQLite data and credentials will be retained."
	}
	operation := Operation{ID: id, SpaceID: target.ID, Status: "pending", Phase: "pocketid", Message: stored.PocketId.Message, StartedAt: now, UpdatedAt: now}
	writes := []sqlite.Write{}
	for _, document := range []struct {
		collection, key string
		value           any
		revision        int64
	}{
		{"space-pocketid", key, stored, record.Revision}, {"pocketid-operation", id, operation, 0}, {"pocketid-task", id, spacePocketIdTask{Key: key, Remove: remove}, 0},
	} {
		write, err := documentWrite(document.collection, document.key, document.value, document.revision)
		if err != nil {
			return PocketId{}, err
		}
		writes = append(writes, write)
	}
	if !exists {
		address, _ := url.Parse(input.IssuerURL)
		// Shared with organization identity providers so an accepted hostname has one owner.
		write, err := documentWrite("organization-pocketid-host", address.Hostname(), map[string]string{"organizationId": org.ID, "instanceId": stored.PocketId.ID}, 0)
		if err != nil {
			return PocketId{}, err
		}
		writes = append(writes, write)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return PocketId{}, translateStoreError(err)
	}
	if manager.runtime.operationsResumed {
		manager.startSpacePocketIdOperation(operation)
	}
	return stored.PocketId, nil
}

// GetPocketIdOperation reads the saved progress of an accepted Space provider operation.
func (manager Manager) GetPocketIdOperation(ctx context.Context, id string) (Operation, error) {
	operation, _, err := loadDocument[Operation](ctx, manager.database, "pocketid-operation", id)
	return operation, err
}
