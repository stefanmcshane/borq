package apps

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) pocketIdBackupStorage(stored organizationPocketIdRecord) objectstore.Config {
	settings := manager.runtime.platformSettings
	namespace := settings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	// Provision through the advertised built-in RustFS endpoint when running outside
	// Kubernetes. The deployed sidecar always uses RustFS's management Service DNS.
	endpoint := settings.S3Endpoint
	if endpoint == "" && settings.ApiUrl != "" {
		address, err := url.Parse(settings.ApiUrl)
		if err == nil && address.Hostname() != "" {
			endpoint = (&url.URL{Scheme: "http", Host: net.JoinHostPort(address.Hostname(), strconv.Itoa(platform.RustfsNodePort))}).String()
		}
	}
	if endpoint == "" {
		endpoint = "http://rustfs-svc." + namespace + ".svc:9000"
	}
	return objectstore.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "borq-pocketid-" + stored.ID, Prefix: "sqlite", PathStyle: true}
}

func (manager Manager) pocketIdBackupCredentials(ctx context.Context, stored organizationPocketIdRecord) (objectstore.Credentials, error) {
	current, _, err := manager.readPocketId(ctx, stored.OrganizationID)
	if err != nil || current.ID != stored.ID {
		return objectstore.Credentials{}, errors.New("Managed Pocket ID backup ownership is invalid.")
	}
	stored = current
	failure := errors.New("Pocket ID SQLite backup setup could not reach management RustFS or its stored credentials. Check management storage; existing identity data is retained.")
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return objectstore.Credentials{}, failure
	}
	backupPath := stored.credentialsPath() + "/backup"
	entry, err := vault.ReadKv(ctx, "secret", backupPath, 1)
	storage := manager.pocketIdBackupStorage(stored)
	if errors.Is(err, openbao.ErrNotFound) {
		if stored.BackupConfigured {
			return objectstore.Credentials{}, errors.New("Pocket ID backup credentials are missing. Restore management secret storage before retrying.")
		}
		root, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
		if err != nil {
			return objectstore.Credentials{}, failure
		}
		storage.Credentials = objectstore.Credentials{AccessKey: root.Values["RUSTFS_ACCESS_KEY"], SecretKey: root.Values["RUSTFS_SECRET_KEY"]}
		client, err := objectstore.New(storage)
		if err != nil {
			return objectstore.Credentials{}, failure
		}
		if err := client.EnsureBucket(ctx); err != nil {
			return objectstore.Credentials{}, failure
		}
		credentials, err := objectstore.RustfsCredentials(ctx, storage)
		if err != nil {
			return objectstore.Credentials{}, failure
		}
		entry.Values = map[string]string{"accessKey": credentials.AccessKey, "secretKey": credentials.SecretKey}
		if _, err := vault.WriteKv(ctx, "secret", backupPath, openbao.WriteKvInput{Values: entry.Values, CheckVersion: 0}); err != nil {
			return objectstore.Credentials{}, failure
		}
	} else if err != nil {
		return objectstore.Credentials{}, failure
	}
	storage.Credentials = objectstore.Credentials{AccessKey: entry.Values["accessKey"], SecretKey: entry.Values["secretKey"]}
	client, err := objectstore.New(storage)
	if err != nil {
		return objectstore.Credentials{}, failure
	}
	if err := client.Validate(ctx); err != nil {
		return objectstore.Credentials{}, failure
	}
	return storage.Credentials, nil
}

func (manager Manager) installPocketId(ctx context.Context, stored organizationPocketIdRecord) (platform.PocketIdInput, map[string]string, error) {
	credentials, err := manager.pocketIdCredentials(ctx, stored)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	backupCredentials, err := manager.pocketIdBackupCredentials(ctx, stored)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
	if err != nil {
		return platform.PocketIdInput{}, nil, errors.New("Could not connect to the management cluster. Retry when it is reachable.")
	}
	values, err := manager.readPocketIdValues(ctx, stored.OrganizationID, stored.ValuesRef)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	environment, err := manager.readPocketIdEnvironment(ctx, stored.OrganizationID, stored.Environment)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	environment, err = pocketIdConfiguration(stored.Organization, values, environment)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return platform.PocketIdInput{}, nil, err
	}
	deployment := platform.PocketIdInput{Connection: connection, Namespace: settings.Namespace, InstanceID: stored.ID, IssuerURL: stored.Status.IssuerURL, StorageClass: settings.StorageClass,
		RequireExistingStorage: stored.Status.State == "ready",
		Tunnel:                 settings.Network.Cloudflare != nil && settings.Network.Cloudflare.ExposureMode() == "tunnel",
		ArchiveKey:             sqliteworkload.ArchiveKey{Identity: credentials["ageIdentity"], Recipient: credentials["ageRecipient"]},
		EncryptionKey:          credentials["encryptionKey"], ApiKey: credentials["apiKey"], ClusterIssuer: stored.Status.CertificateIssuer, BackupCredentials: backupCredentials, Values: values, Environment: environment, Bootstrapping: stored.Status.State != "ready"}
	if _, err := platform.InstallPocketId(ctx, deployment); err != nil {
		return platform.PocketIdInput{}, nil, errors.New("Pocket ID could not start. Check management cluster storage, image pulls and resource ownership, then retry.")
	}
	current, record, err := manager.readPocketId(ctx, stored.OrganizationID)
	if err != nil || current.ID != stored.ID {
		return platform.PocketIdInput{}, nil, errors.New("Could not read Pocket ID backup setup progress.")
	}
	if !current.BackupConfigured || (!deployment.Bootstrapping && (!current.ConfigurationApplied || current.AppliedValuesRef != stored.ValuesRef || current.AppliedEnvironmentRef != stored.Environment.CredentialsRef)) {
		current.BackupConfigured = true
		if !deployment.Bootstrapping {
			current.AppliedValuesRef = stored.ValuesRef
			current.AppliedEnvironmentRef = stored.Environment.CredentialsRef
			current.ConfigurationApplied = true
		}
		write, err := documentWrite("organization-pocketid", current.OrganizationID, current, record.Revision)
		if err != nil || manager.database.Commit(ctx, []sqlite.Write{write}) != nil {
			return platform.PocketIdInput{}, nil, errors.New("Could not save Pocket ID backup setup progress. Retry setup.")
		}
	}
	return deployment, credentials, nil
}
