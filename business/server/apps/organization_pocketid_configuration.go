package apps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// OrganizationPocketIdConfiguration is a privileged editor response containing
// application secrets. Generated authentication and encryption keys are excluded.
type OrganizationPocketIdConfiguration struct {
	// Values contains the effective editable environment, including legacy configuration.
	Values map[string]string `json:"values"`
	// Managed describes non-secret runtime settings controlled by Borq.
	Managed map[string]string `json:"managed"`
	// Revision identifies the accepted configuration independently of setup progress.
	Revision string `json:"revision"`
	// BaseVersion fences the next publication against concurrent secret edits.
	BaseVersion int `json:"baseVersion"`
	// CertificateIssuers lists ready ClusterIssuers in management.
	CertificateIssuers []string `json:"certificateIssuers"`
	// DefaultCertificateIssuer is empty unless a suitable issuer is unambiguous.
	DefaultCertificateIssuer string `json:"defaultCertificateIssuer"`
}

func (stored organizationPocketIdRecord) configurationRevision() string {
	// References are immutable and contain no secret values. Including ownership
	// prevents a form from being reused for a recreated organization or instance.
	encoded, _ := json.Marshal([]string{stored.OrganizationID, stored.ID, stored.ValuesRef, stored.Environment.CredentialsRef})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (stored organizationPocketIdRecord) configurationGroup() string {
	if stored.Environment.Name != "" {
		return stored.Environment.Name
	}
	return "pocketid-" + stored.OrganizationID
}

// GetOrganizationPocketIdConfiguration reads effective settings for explicit
// administrator editing; ordinary status and management exports remain redacted.
func (manager Manager) GetOrganizationPocketIdConfiguration(ctx context.Context, name string) (OrganizationPocketIdConfiguration, error) {
	organization, _, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return OrganizationPocketIdConfiguration{}, err
	}
	stored, _, err := manager.readPocketId(ctx, organization.OrganizationID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return OrganizationPocketIdConfiguration{}, err
	}
	stored.OrganizationID = organization.OrganizationID
	values, err := manager.readPocketIdValues(ctx, stored.OrganizationID, stored.ValuesRef)
	if err != nil {
		return OrganizationPocketIdConfiguration{}, err
	}
	environment, err := manager.readPocketIdEnvironment(ctx, stored.OrganizationID, stored.Environment)
	if err != nil {
		return OrganizationPocketIdConfiguration{}, err
	}
	configuration, err := pocketIdConfiguration(name, values, environment)
	if err != nil {
		return OrganizationPocketIdConfiguration{}, err
	}
	group, err := manager.resolveManagementSecret(ctx, stored.OrganizationID, stored.configurationGroup(), "latest")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return OrganizationPocketIdConfiguration{}, err
	}
	if stored.Environment.Name == "" && group.Version > 0 {
		// A publication can outlive a failed queue write. Read it before allowing
		// a retry to replace it, instead of silently clearing those values.
		configuration, err = pocketIdConfiguration(name, values, group.Data)
		if err != nil {
			return OrganizationPocketIdConfiguration{}, err
		}
	}
	result := OrganizationPocketIdConfiguration{Values: configuration, Managed: platform.PocketIdManagedEnvironment(stored.Status.IssuerURL),
		Revision: stored.configurationRevision(), BaseVersion: group.Version, CertificateIssuers: []string{}}
	if manager.pocketIdAvailable() {
		// Discovery is a hint, not a prerequisite for editing saved values. Never
		// invent an issuer when the cluster or cert-manager is unavailable.
		lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
		if err == nil {
			issuers, err := platform.DiscoverPocketIdCertificateIssuers(lookup, connection)
			if err == nil {
				result.CertificateIssuers, result.DefaultCertificateIssuer = issuers.Names, issuers.Default
			}
		}
	}
	return result, nil
}

func pocketIdConfiguration(organization string, values map[string]any, environment map[string]string) (map[string]string, error) {
	configuration, err := platform.PocketIdConfiguration(values, environment)
	if err != nil {
		return nil, err
	}
	if _, configured := configuration["APP_NAME"]; !configured {
		configuration["APP_NAME"] = organization
	}
	return configuration, nil
}

// Called with the manager lock after identity and OIDC checks. Publication CAS
// also fences writers using the management-secret API, which has its own lock.
func (manager Manager) publishPocketIdConfiguration(ctx context.Context, name string, stored organizationPocketIdRecord, input OrganizationPocketIdInput) (pocketIdEnvironment, string, error) {
	if input.ConfigurationRevision != stored.configurationRevision() {
		return pocketIdEnvironment{}, "", organizationOidcError{ErrConflict, "Pocket ID configuration changed; reload before saving."}
	}
	values, err := manager.readPocketIdValues(ctx, stored.OrganizationID, stored.ValuesRef)
	if err != nil {
		return pocketIdEnvironment{}, "", err
	}
	valuesRef := stored.ValuesRef
	if _, exists := values["config"]; exists {
		// The editor replaces the whole effective map. Remove the legacy config
		// fallback so deleting an entry really removes it, while preserving all
		// other workload overrides from declarative configuration.
		delete(values, "config")
		encoded, err := json.Marshal(values)
		if err != nil {
			return pocketIdEnvironment{}, "", err
		}
		valuesRef, err = manager.stageManagementSecret(ctx, "pocketid-values", stored.OrganizationID, "values", string(encoded))
		if err != nil {
			return pocketIdEnvironment{}, "", err
		}
	}
	// BaseVersion is a pointer because zero must explicitly require creation.
	group, err := manager.PutManagementSecret(ctx, name, stored.configurationGroup(), PutSecretInput{Type: "secret-group", Data: input.Configuration, BaseVersion: &input.ConfigurationBaseVersion})
	if err != nil {
		return pocketIdEnvironment{}, "", err
	}
	environment, err := manager.stagePocketIdEnvironment(ctx, stored.OrganizationID, EnvReference{Name: group.Name, Version: "latest"}, group)
	return environment, valuesRef, err
}
