package apps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type postgresCredentialState struct {
	Phase         string    `json:"phase,omitempty"`
	Namespace     string    `json:"namespace,omitempty"`
	RootRotatedAt time.Time `json:"rootRotatedAt,omitempty"`
	KvMount       string    `json:"kvMount,omitempty"`
}

type postgresCredentialError struct{ message string }

func (failure postgresCredentialError) Error() string { return failure.message }

// RotatePostgresCredentials queues OpenBao rotation and waits for current credentials to reach consumers.
func (manager Manager) RotatePostgresCredentials(ctx context.Context, organization, space, name string) (Operation, error) {
	result, err := manager.acceptPostgres(ctx, organization, space, name, PostgresInput{}, "rotate", postgresTask{})
	if err != nil {
		return Operation{}, err
	}
	return manager.GetPostgresOperation(ctx, result.Operation)
}

func postgresRotationPeriod(policy PostgresCredentialPolicy) time.Duration {
	period, err := time.ParseDuration(policy.RotationPeriod)
	if err != nil || period < time.Minute {
		return 24 * time.Hour
	}
	return period
}

func (manager Manager) savePostgresCredentialState(ctx context.Context, stored postgresRecord) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	key := addonKey(stored.Organization, stored.Space, stored.Postgres.Name)
	current, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	if err != nil {
		return err
	}
	if current.ID != stored.ID || current.ActiveOperation != stored.ActiveOperation || current.Postgres.State == "removed" || current.Postgres.State == "deleting" {
		return ErrConflict
	}
	if current.Credentials == stored.Credentials && current.CredentialsRef == stored.CredentialsRef {
		return nil
	}
	current.Credentials, current.CredentialsRef = stored.Credentials, stored.CredentialsRef
	write, err := documentWrite("postgres", key, current, record.Revision)
	if err != nil {
		return err
	}
	return translateStoreError(manager.database.Commit(ctx, []sqlite.Write{write}))
}

// The OpenBao SDK retains connection state through pointer receivers.
func (manager Manager) postgresVault(ctx context.Context) (*openbao.Client, error) {
	return platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
}

func (manager Manager) reconcilePostgresCredentials(ctx context.Context, stored postgresRecord, client postgres.Client, input postgres.Input) (postgres.Input, error) {
	current, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(stored.Organization, stored.Space, stored.Postgres.Name))
	if err != nil {
		return input, err
	}
	if current.ID != stored.ID || current.ActiveOperation != stored.ActiveOperation {
		return input, ErrConflict
	}
	stored = current
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return input, err
	}
	target, err := newClusterClient(cluster)
	if err != nil {
		return input, err
	}
	provider, err := target.OpenBaoProvider(ctx, "borq-openbao")
	if err != nil {
		return input, err
	}
	auth, _ := provider["auth"].(map[string]any)
	tokenRef, _ := auth["tokenSecretRef"].(map[string]any)
	namespace, _ := tokenRef["namespace"].(string)
	if namespace == "" {
		return input, postgresCredentialError{message: "database credential delivery requires a namespaced OpenBao token reference"}
	}
	input.Credentials.Namespace, input.Credentials.Provider = namespace, provider
	if host := stored.Postgres.Specification.Credentials.Host; host != "" {
		if err := client.SetServerDnsName(ctx, input, host); err != nil {
			return input, err
		}
	}
	ca, err := client.ReadServerCa(ctx, input)
	if err != nil {
		return input, err
	}
	if err := platform.ReconcilePostgresTrust(ctx, platform.PostgresTrustInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Namespace: manager.runtime.platformSettings.Namespace, DatabaseID: stored.ID, Certificate: ca}); err != nil {
		return input, err
	}
	vault, err := manager.postgresVault(ctx)
	if err != nil {
		return input, err
	}
	if err := vault.EnsureDatabaseEngine(ctx, input.Credentials.Mount); err != nil {
		return input, err
	}
	if _, err := platform.EnsureClusterCredentials(ctx, platform.CredentialsInput{ManagementKubeconfigPath: manager.runtime.managementKubeconfigPath, Namespace: manager.runtime.platformSettings.Namespace, TargetID: cluster.ID}); err != nil {
		return input, err
	}
	host, port := stored.Postgres.Specification.Credentials.Host, stored.Postgres.Specification.Credentials.Port
	if host == "" {
		host = input.Name + "-rw." + input.Namespace + ".svc"
	}
	if port == 0 {
		port = 5432
	}
	caPath := "/openbao/postgres-ca/" + stored.ID + ".crt"
	endpoint := url.URL{Scheme: "postgresql", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + input.Database, RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {caPath}}.Encode()}
	connection := openbao.DatabaseConnectionInput{ConnectionUrl: strings.Replace(endpoint.String(), "postgresql://", "postgresql://{{username}}:{{password}}@", 1), Username: "borq_openbao", AllowedRole: input.Credentials.Role, RootCaPath: caPath}
	_, err = vault.ReadDatabaseConnection(ctx, input.Credentials.Mount, input.Credentials.Role)
	newConnection := errors.Is(err, openbao.ErrNotFound)
	if err != nil && !newConnection {
		return input, err
	}
	if newConnection {
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return input, err
		}
		connection.BootstrapPassword = base64.RawURLEncoding.EncodeToString(password)
		if err := client.BootstrapManagementRole(ctx, input, connection.BootstrapPassword); err != nil {
			return input, err
		}
	}
	if err := vault.ConfigureDatabaseConnection(ctx, input.Credentials.Mount, input.Credentials.Role, connection); err != nil {
		return input, postgresCredentialError{message: "OpenBao cannot verify its PostgreSQL connection. Check private SQL routing and apply management settings to mount the database CA trust directory."}
	}
	period := postgresRotationPeriod(stored.Postgres.Specification.Credentials)
	if newConnection || stored.Credentials.RootRotatedAt.IsZero() || time.Since(stored.Credentials.RootRotatedAt) >= period {
		if err := vault.RotateDatabaseRoot(ctx, input.Credentials.Mount, input.Credentials.Role); err != nil {
			return input, err
		}
		stored.Credentials.RootRotatedAt = time.Now().UTC()
	}
	stored.Credentials.Namespace = namespace
	stored.Credentials.KvMount, _ = provider["path"].(string)
	if stored.Credentials.KvMount == "" {
		return input, errors.New("database credential delivery requires a target KV mount")
	}
	if stored.Credentials.Phase == "" {
		stored.Credentials.Phase = "enrolling"
	}
	if err := manager.savePostgresCredentialState(ctx, stored); err != nil {
		return input, err
	}
	if err := client.RetireBootstrapCredentials(ctx, input); err != nil {
		return input, err
	}
	wanted := openbao.StaticRole{Connection: input.Credentials.Role, Username: input.Username, RotationPeriod: period}
	role, err := vault.ReadStaticRole(ctx, input.Credentials.Mount, input.Credentials.Role)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return input, err
	}
	if errors.Is(err, openbao.ErrNotFound) || role != wanted {
		if err := vault.PutStaticRole(ctx, input.Credentials.Mount, input.Credentials.Role, wanted); err != nil {
			return input, err
		}
	}
	stored.Credentials.Phase = "managed"
	if stored.CredentialsRef != "" {
		path, _, _ := strings.Cut(stored.CredentialsRef, "#")
		if err := vault.DeleteKvMetadata(ctx, stored.Credentials.KvMount, path); err != nil {
			return input, err
		}
		stored.CredentialsRef = ""
	}
	if err := manager.savePostgresCredentialState(ctx, stored); err != nil {
		return input, err
	}
	input.BootstrapPassword = ""
	return input, client.ReconcileEnvironment(ctx, input)
}

func (manager Manager) removePostgresCredentials(ctx context.Context, stored postgresRecord, input postgres.Input) error {
	vault, err := manager.postgresVault(ctx)
	if err != nil {
		return err
	}
	if err := vault.DeleteStaticRole(ctx, input.Credentials.Mount, input.Credentials.Role); err != nil {
		return err
	}
	if err := vault.DeleteDatabaseConnection(ctx, input.Credentials.Mount, input.Credentials.Role); err != nil {
		return err
	}
	if stored.CredentialsRef != "" {
		path, _, _ := strings.Cut(stored.CredentialsRef, "#")
		mount := stored.Credentials.KvMount
		if mount == "" {
			_, target, err := manager.environmentGroupConnection(ctx, EnvironmentGroupTarget{Organization: stored.Organization, Space: stored.Space})
			if err != nil {
				return err
			}
			provider, err := target.OpenBaoProvider(ctx, "borq-openbao")
			if err != nil {
				return err
			}
			mount, _ = provider["path"].(string)
		}
		if err := vault.DeleteKvMetadata(ctx, mount, path); err != nil {
			return err
		}
	}
	return platform.ReconcilePostgresTrust(ctx, platform.PostgresTrustInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Namespace: manager.runtime.platformSettings.Namespace, DatabaseID: stored.ID})
}

func (manager Manager) refreshPostgresConsumers(ctx context.Context, stored postgresRecord) (bool, error) {
	return manager.refreshManagedEnvironmentConsumers(ctx, stored.Organization, stored.Space, stored.Postgres.EnvironmentGroup, stored.ID)
}

func (manager Manager) rotatePostgresCredentials(ctx context.Context, input postgres.Input, requestedAt time.Time) error {
	vault, err := manager.postgresVault(ctx)
	if err != nil {
		return err
	}
	current, err := vault.ReadStaticCredentials(ctx, input.Credentials.Mount, input.Credentials.Role)
	if err != nil {
		return err
	}
	if !current.LastRotation.After(requestedAt) {
		if err := vault.RotateStaticRole(ctx, input.Credentials.Mount, input.Credentials.Role); err != nil {
			return fmt.Errorf("rotate PostgreSQL credentials through OpenBao: %w", err)
		}
	}
	return nil
}

func (manager Manager) executePostgresRotation(ctx context.Context, stored postgresRecord, client postgres.Client, input postgres.Input, operation Operation) (postgres.Observation, error) {
	var err error
	input, err = manager.reconcilePostgresCredentials(ctx, stored, client, input)
	if err != nil {
		return postgres.Observation{}, err
	}
	if err := manager.rotatePostgresCredentials(ctx, input, operation.StartedAt); err != nil {
		return postgres.Observation{}, err
	}
	for {
		if err := client.ReconcileEnvironment(ctx, input); err != nil {
			return postgres.Observation{}, err
		}
		ready, err := manager.postgresEnvironmentReady(ctx, stored, client, input)
		if err != nil {
			return postgres.Observation{}, err
		}
		if ready {
			consumers, err := manager.refreshPostgresConsumers(ctx, stored)
			if err != nil {
				return postgres.Observation{}, err
			}
			if consumers {
				return client.Observe(ctx, input)
			}
		}
		operation.Status, operation.Message, operation.UpdatedAt = "waiting", "OpenBao rotated the database password; waiting for credential delivery and application refresh", time.Now().UTC()
		if err := manager.savePostgresOperation(ctx, operation); err != nil {
			return postgres.Observation{}, err
		}
		if err := manager.waitForPoll(ctx); err != nil {
			return postgres.Observation{}, err
		}
	}
}
