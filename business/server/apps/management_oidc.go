package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/oidc"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Capture the immutable owner and revision at acceptance, before any slow provisioning.
type managementAuthentication struct {
	OrganizationID   string `json:"organizationId"`
	Revision         int64  `json:"revision"`
	PolicyRevision   int64  `json:"policyRevision,omitempty"`
	ImplicitPocketID string `json:"implicitPocketId,omitempty"`
}

func validateManagementAuthentication(configuration ManagementConfiguration) error {
	for _, organization := range configuration.Organizations {
		if organization.Oidc != nil && organization.ManagedPocketId != nil {
			return fmt.Errorf("%w: organization %s must declare oidc or managedPocketId, not both", ErrInvalid, organization.Name)
		}
		if organization.Oidc == nil && organization.ManagedPocketId == nil {
			continue
		}
		addon := organization.ManagedPocketId
		input := organization.oidcInput()
		if input.AdminSubject != "" {
			if err := validateOrganizationOidcSubject(input.AdminSubject); err != nil {
				return err
			}
		}
		if input.ClientSecret != "" && input.ClientSecretRef != "" {
			return fmt.Errorf("%w: supply clientSecret or clientSecretRef, not both", ErrInvalid)
		}
		if len(input.ClientSecret) > 8192 || len(input.ClientID) > 1024 || strings.ContainsAny(input.ClientID, "\r\n\x00") {
			return fmt.Errorf("%w: invalid OIDC client credentials", ErrInvalid)
		}
		if err := oidc.ValidateURL(strings.TrimSpace(input.BaseURL)); err != nil {
			return fmt.Errorf("%w: invalid OIDC borqPublicUrl", ErrInvalid)
		}
		base, _ := url.Parse(strings.TrimSpace(input.BaseURL))
		if base.Path != "" && base.Path != "/" || base.RawPath != "" {
			return fmt.Errorf("%w: OIDC borqPublicUrl must be an origin without a path", ErrInvalid)
		}
		if addon != nil {
			if addon.Values != nil && addon.ValuesRef != "" {
				return fmt.Errorf("%w: supply Pocket ID values or valuesRef, not both", ErrInvalid)
			}
			if err := platform.ValidatePocketIdValues(addon.Values); err != nil {
				return fmt.Errorf("%w: unsupported managed Pocket ID workload values", ErrInvalid)
			}
			if input.AdminEmail != "" {
				if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
					return err
				}
			}

			if err := validatePocketIdInput(managementPocketIdInput(*addon, input, 0)); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
		} else {
			if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
				return err
			}
			if err := oidc.ValidateURL(strings.TrimSpace(input.IssuerURL)); err != nil || strings.TrimSpace(input.ClientID) == "" {
				return fmt.Errorf("%w: external OIDC requires issuerUrl and clientId", ErrInvalid)
			}
		}
	}
	return nil
}

// New identity records are committed atomically with the task. No accepted task can
// accidentally attach credentials to a deleted and recreated organization name.
func (manager Manager) stageManagementAuthentication(ctx context.Context, configuration *ManagementConfiguration) (map[string]managementAuthentication, []sqlite.Write, error) {
	authentication := map[string]managementAuthentication{}
	writes := []sqlite.Write{}
	for index := range configuration.Organizations {
		declared := &configuration.Organizations[index]
		hasIdentity := declared.Oidc != nil || declared.ManagedPocketId != nil
		organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", declared.Name)
		if err == nil && !hasIdentity && organization.AuthenticationState != "queued" && !managementOrganizationHasSecrets(*configuration, declared.Name) {
			continue
		}
		var stored organizationOidcRecord
		var record sqlite.Record
		if errors.Is(err, ErrNotFound) {
			id, err := newId()
			if err != nil {
				return nil, nil, err
			}
			organization = Organization{Name: declared.Name, ID: id, ManagedAddons: true, AuthenticationState: "queued"}
			if !hasIdentity {
				// Organization resources remain usable through the administrator token before identity setup.
				organization.AuthenticationState = ""
			}
			write, err := documentWrite("organization", declared.Name, organization, 0)
			if err != nil {
				return nil, nil, err
			}
			writes = append(writes, write)
		} else if err != nil {
			return nil, nil, err
		} else {
			stored, record, err = manager.organizationOidc(ctx, declared.Name)
			if err != nil {
				return nil, nil, err
			}
			organization.ID = stored.OrganizationID
		}
		authentication[declared.Name] = managementAuthentication{OrganizationID: organization.ID, Revision: record.Revision}
		if !hasIdentity {
			if organization.AuthenticationState == "queued" {
				pocket, pocketRecord, err := manager.readPocketId(ctx, organization.ID)
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, nil, err
				}
				accepted := authentication[declared.Name]
				accepted.ImplicitPocketID = pocket.ID
				if err := manager.matchImplicitManagementPocketId(ctx, declared.Name, accepted, pocket); err != nil {
					return nil, nil, err
				}
				if pocket.Status.State == "failed" {
					// Reapplying explicitly retries once. Resuming this task never requeues a failure.
					pocket.Status.State, pocket.Status.Message = "queued", "Pocket ID setup is queued."
					pocket.ConfigurationApplied = false
					write, err := documentWrite("organization-pocketid", organization.ID, pocket, pocketRecord.Revision)
					if err != nil {
						return nil, nil, err
					}
					writes = append(writes, write)
				}
				authentication[declared.Name] = accepted
			}
			continue
		}
		input := declared.oidcInput()
		input.IssuerURL, input.ClientID = strings.TrimSpace(input.IssuerURL), strings.TrimSpace(input.ClientID)
		input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
		managed := declared.ManagedPocketId != nil
		managedInstanceID := ""
		managedAdministratorEmail := ""
		if addon := declared.ManagedPocketId; addon != nil {
			addon.BaseURL = input.BaseURL
			addon.IssuerURL = strings.TrimRight(strings.TrimSpace(addon.IssuerURL), "/")
			addon.AdminUsername, addon.AdminEmail, addon.CertificateIssuer = strings.TrimSpace(addon.AdminUsername), strings.TrimSpace(addon.AdminEmail), strings.TrimSpace(addon.CertificateIssuer)
			input.IssuerURL = addon.IssuerURL
			if !manager.pocketIdAvailable() {
				return nil, nil, fmt.Errorf("%w: managed Pocket ID requires a management cluster", ErrInvalid)
			}
			existing, _, existingErr := manager.readPocketId(ctx, organization.ID)
			if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
				return nil, nil, existingErr
			}
			if existingErr == nil {
				managedInstanceID = existing.ID
				managedAdministratorEmail = existing.Status.AdminEmail
				if err := matchManagementPocketId(existing, *addon, input); err != nil {
					return nil, nil, err
				}
			}
			if addon.Values != nil {
				encoded, err := json.Marshal(addon.Values)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: invalid Pocket ID values", ErrInvalid)
				}
				// Equal declarations retain their existing receipt, avoiding unnecessary rollouts.
				if existing.ValuesRef != "" {
					previous, err := manager.readManagementSecret(ctx, "pocketid-values", organization.ID, existing.ValuesRef, "values")
					if err != nil {
						return nil, nil, err
					}
					if previous == string(encoded) {
						addon.ValuesRef = existing.ValuesRef
					}
				}
				if addon.ValuesRef == "" {
					addon.ValuesRef, err = manager.stageManagementSecret(ctx, "pocketid-values", organization.ID, "values", string(encoded))
					if err != nil {
						return nil, nil, err
					}
				}
				addon.Values = nil
			} else if addon.ValuesRef != "" {
				if _, err := manager.readPocketIdValues(ctx, organization.ID, addon.ValuesRef); err != nil {
					return nil, nil, err
				}
			} else {
				addon.ValuesRef = existing.ValuesRef
			}
		}
		if !managed {
			selectionWrites, err := manager.stageOrganizationAdminEmail(ctx, organization.ID, OrganizationOidcInput{AdminEmail: input.AdminEmail, IssuerURL: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL})
			if err != nil {
				return nil, nil, err
			}
			_, policyRecord, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
			if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
				return nil, nil, policyErr
			}
			accepted := authentication[declared.Name]
			accepted.PolicyRevision = policyRecord.Revision
			if policyRecord.Revision == 0 {
				writes = append(writes, selectionWrites...)
				accepted.PolicyRevision = 1
			}
			authentication[declared.Name] = accepted
		}
		if managed && input.AdminEmail != "" {
			if err := validateOrganizationAdminEmail(input.AdminEmail); err != nil {
				return nil, nil, err
			}
			policy, policyRecord, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, nil, err
			}
			if policyRecord.Revision == 0 {
				policy = organizationLoginPolicy{OrganizationID: organization.ID, AdminEmail: strings.TrimSpace(input.AdminEmail), Issuer: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL}
				if managedInstanceID != "" {
					enrolled, _, err := loadDocument[organizationLoginAdmin](ctx, manager.database, "organization-login-admin", organization.ID)
					if err != nil && !errors.Is(err, ErrNotFound) {
						return nil, nil, err
					}
					if err == nil && (enrolled.OrganizationID != organization.ID || enrolled.Issuer != input.IssuerURL ||
						!strings.EqualFold(policy.AdminEmail, strings.TrimSpace(managedAdministratorEmail))) {
						return nil, nil, fmt.Errorf("%w: the organization's enrolled administrator cannot be replaced by reapplying settings", ErrConflict)
					}
					policy.ClientID, policy.PocketInstanceID = managedInstanceID, managedInstanceID
				}
				write, err := documentWrite("organization-login-policy", organization.ID, policy, 0)
				if err != nil {
					return nil, nil, err
				}
				writes = append(writes, write)
			} else if policy.OrganizationID != organization.ID || !strings.EqualFold(policy.AdminEmail, strings.TrimSpace(input.AdminEmail)) || policy.Issuer != input.IssuerURL ||
				policy.PocketOrganizationID != "" && (policy.ClientID != input.ClientID || policy.BaseURL != input.BaseURL) ||
				managedInstanceID != "" && policy.PocketInstanceID != "" && (policy.PocketInstanceID != managedInstanceID || policy.ClientID != managedInstanceID || policy.BaseURL != input.BaseURL) {
				return nil, nil, fmt.Errorf("%w: the organization's initial administrator policy cannot be replaced by reapplying settings", ErrConflict)
			} else if managedInstanceID != "" && policy.PocketInstanceID == "" {
				// Ready legacy instances skip setup completion, so bind their policy at acceptance.
				policy.ClientID, policy.PocketInstanceID, policy.BaseURL = managedInstanceID, managedInstanceID, input.BaseURL
				write, err := documentWrite("organization-login-policy", organization.ID, policy, policyRecord.Revision)
				if err != nil {
					return nil, nil, err
				}
				writes = append(writes, write)
			}
		}
		if input.AdminSubject != "" {
			policy, _, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", organization.ID)
			if policyErr == nil && (policy.OrganizationID != organization.ID || policy.Issuer != input.IssuerURL) {
				return nil, nil, organizationOidcError{ErrConflict, "The administrator's saved issuer cannot be replaced by reapplying settings."}
			}
			if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
				return nil, nil, policyErr
			}
			if errors.Is(policyErr, ErrNotFound) && input.AdminEmail == "" {
				return nil, nil, organizationOidcError{ErrInvalid, "Set adminEmail when selecting the first organization administrator."}
			}
			if _, err := manager.stageOrganizationOidcAdministrator(ctx, organization.ID, input.IssuerURL, input.AdminSubject, input.AdminEmail); err != nil {
				return nil, nil, err
			}
		}
		if input.ClientSecret != "" {
			input.ClientSecretRef, err = manager.stageManagementSecret(ctx, "oidc", organization.ID, "clientSecret", input.ClientSecret)
			if err != nil {
				return nil, nil, err
			}
			input.ClientSecret = ""
		} else if input.ClientSecretRef != "" {
			if _, err := manager.readManagementSecret(ctx, "oidc", organization.ID, input.ClientSecretRef, "clientSecret"); err != nil {
				return nil, nil, err
			}
		} else if !managed {
			if stored.Settings.IssuerURL != input.IssuerURL || stored.Settings.ClientID != input.ClientID || stored.CredentialsRef == "" {
				return nil, nil, fmt.Errorf("%w: supply an OIDC client secret when configuring or changing the provider", ErrInvalid)
			}
			input.ClientSecretRef = stored.CredentialsRef
		}
		if !managed {
			declared.Oidc = &input
		}
	}
	return authentication, writes, nil
}

func validManagementSecretRef(purpose, organizationID, ref string) bool {
	prefix := "borq/organization-" + purpose + "/" + organizationID + "/"
	suffix := strings.TrimPrefix(ref, prefix)
	return len(organizationID) == 32 && strings.Trim(organizationID, "0123456789abcdef") == "" && strings.HasPrefix(ref, prefix) && len(suffix) == 32 && strings.Trim(suffix, "0123456789abcdef") == ""
}

func (manager Manager) stageManagementSecret(ctx context.Context, purpose, organizationID, key, value string) (string, error) {
	id, err := newId()
	if err != nil {
		return "", err
	}
	ref := "borq/organization-" + purpose + "/" + organizationID + "/" + id
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return "", errors.New("management secret storage is unavailable")
	}
	if _, err := vault.WriteKv(ctx, "secret", ref, openbao.WriteKvInput{Values: map[string]string{key: value}, CheckVersion: 0}); err != nil {
		return "", errors.New("could not store management authentication configuration")
	}
	return ref, nil
}

func (manager Manager) readManagementSecret(ctx context.Context, purpose, organizationID, ref, key string) (string, error) {
	if !validManagementSecretRef(purpose, organizationID, ref) {
		return "", fmt.Errorf("%w: authentication secret reference does not belong to this organization and purpose", ErrInvalid)
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return "", errors.New("management secret storage is unavailable")
	}
	entry, err := vault.ReadKv(ctx, "secret", ref, 1)
	if err != nil || entry.Values[key] == "" {
		return "", errors.New("management authentication secret is unavailable")
	}
	return entry.Values[key], nil
}

func (manager Manager) managementOidcOwner(ctx context.Context, name string, accepted managementAuthentication) (organizationOidcRecord, sqlite.Record, error) {
	stored, record, err := manager.organizationOidc(ctx, name)
	if err != nil {
		return stored, record, err
	}
	if accepted.OrganizationID == "" || stored.OrganizationID != accepted.OrganizationID {
		return stored, record, fmt.Errorf("%w: organization identity changed after management configuration was accepted", ErrConflict)
	}
	return stored, record, nil
}

func (manager Manager) applyManagementOidc(ctx context.Context, organization ManagementOrganization, accepted managementAuthentication) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	stored, record, err := manager.managementOidcOwner(ctx, organization.Name, accepted)
	if err != nil {
		return err
	}
	input := organization.oidcInput()
	var secret string
	if organization.ManagedPocketId != nil {
		managed, _, err := manager.readPocketId(ctx, accepted.OrganizationID)
		if err != nil {
			return err
		}
		if managed.Status.State != "ready" {
			return fmt.Errorf("%w: managed Pocket ID is not ready", ErrConflict)
		}
		credentials, err := manager.pocketIdCredentials(ctx, managed)
		if err != nil {
			return err
		}
		input.IssuerURL, input.ClientID, secret = managed.Status.IssuerURL, managed.ID, credentials["clientSecret"]
	}
	if input.ClientSecretRef != "" {
		supplied, err := manager.readManagementSecret(ctx, "oidc", accepted.OrganizationID, input.ClientSecretRef, "clientSecret")
		if err != nil {
			return err
		}
		if secret != "" && supplied != secret {
			return fmt.Errorf("%w: OIDC client secret does not match managed Pocket ID", ErrInvalid)
		}
		secret = supplied
	}
	writes := []sqlite.Write{}
	if organization.ManagedPocketId == nil {
		_, policyRecord, err := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", accepted.OrganizationID)
		if err != nil {
			return err
		}
		if accepted.PolicyRevision != 0 && policyRecord.Revision != accepted.PolicyRevision {
			return fmt.Errorf("%w: administrator selection changed after management configuration was accepted; reapply", ErrConflict)
		}
		writes, err = manager.stageOrganizationAdminEmail(ctx, accepted.OrganizationID, OrganizationOidcInput{AdminEmail: input.AdminEmail, IssuerURL: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL})
		if err != nil {
			return err
		}
	}
	if input.AdminSubject != "" {
		policy, _, policyErr := loadDocument[organizationLoginPolicy](ctx, manager.database, "organization-login-policy", accepted.OrganizationID)
		if policyErr != nil || policy.OrganizationID != accepted.OrganizationID || policy.Issuer != input.IssuerURL {
			return organizationLoginDenied()
		}
		administratorWrites, err := manager.stageOrganizationOidcAdministrator(ctx, accepted.OrganizationID, input.IssuerURL, input.AdminSubject, input.AdminEmail)
		if err != nil {
			return err
		}
		if len(administratorWrites) != 0 {
			writes = administratorWrites
		}
		if len(writes) != 0 && record.Revision != accepted.Revision {
			return organizationOidcError{ErrConflict, "OIDC settings changed after management configuration was accepted; reapply before enrolling the administrator."}
		}
	}
	owner, ownerRecord, err := loadDocument[Organization](ctx, manager.database, "organization", organization.Name)
	if err != nil || owner.ID != accepted.OrganizationID {
		return organizationLoginDenied()
	}
	if owner.AuthenticationState == "queued" {
		owner.AuthenticationState = "ready"
		write, err := documentWrite("organization", owner.Name, owner, ownerRecord.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	if stored.Settings.IssuerURL == input.IssuerURL && stored.Settings.ClientID == input.ClientID && stored.Settings.BaseURL == input.BaseURL {
		currentSecret, err := manager.organizationOidcSecret(ctx, stored)
		if err != nil {
			return err
		}
		if secret == currentSecret {
			if len(writes) == 0 {
				return nil
			}
			return translateStoreError(manager.database.Commit(ctx, writes))
		}
	}
	if record.Revision != accepted.Revision {
		return fmt.Errorf("%w: OIDC settings changed after management configuration was accepted; reapply to replace them", ErrConflict)
	}
	_, err = manager.putOrganizationOidc(ctx, organization.Name, OrganizationOidcInput{AdminEmail: input.AdminEmail, IssuerURL: input.IssuerURL, ClientID: input.ClientID, BaseURL: input.BaseURL, ClientSecret: secret, Revision: accepted.Revision}, writes)
	return err
}
