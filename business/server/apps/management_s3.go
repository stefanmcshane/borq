package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// ErrManagementStorageInUse lets transports explain primary storage dependencies
// without exposing private provider diagnostics from wrapped errors.
var ErrManagementStorageInUse = errors.New("Primary S3 storage is still in use by management services, databases, or retained backups. Offsite backups can be disabled separately in Settings.")

// GetManagementS3 returns safe storage settings, defaulting every profile to persistent built-in RustFS.
func (manager Manager) GetManagementS3(ctx context.Context) (ManagementS3, error) {
	stored, _, err := loadDocument[ManagementS3](ctx, manager.database, "management-s3", "shared")
	if !errors.Is(err, ErrNotFound) {
		return stored, err
	}
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	endpoint := manager.runtime.platformSettings.S3Endpoint
	if endpoint == "" && manager.runtime.platformSettings.ApiUrl != "" {
		advertised, parseErr := url.Parse(manager.runtime.platformSettings.ApiUrl)
		if parseErr != nil || advertised.Hostname() == "" {
			return ManagementS3{}, fmt.Errorf("%w: management API URL cannot advertise built-in S3", ErrInvalid)
		}
		endpoint = (&url.URL{Scheme: "http", Host: net.JoinHostPort(advertised.Hostname(), strconv.Itoa(platform.RustfsNodePort))}).String()
	}
	if endpoint == "" {
		endpoint = "http://rustfs-svc." + namespace + ".svc:9000"
	}
	return ManagementS3{Endpoint: endpoint, Region: "us-east-1", Bucket: platform.ManagementBucketName(manager.runtime.platformSettings.ClusterName), PathStyle: true, CredentialsRef: "borq/platform/rustfs"}, nil
}

// ResolveManagementS3 retrieves credentials only at an explicit internal execution boundary.
func (manager Manager) ResolveManagementS3(ctx context.Context, storage ManagementS3) (objectstore.Config, error) {
	if !validStorageCredentialsRef(storage.CredentialsRef) {
		return objectstore.Config{}, fmt.Errorf("%w: invalid management S3 credentials reference", ErrInvalid)
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return objectstore.Config{}, err
	}
	entry, err := vault.ReadKv(ctx, "secret", storage.CredentialsRef, 0)
	if err != nil {
		return objectstore.Config{}, err
	}
	credentials := objectstore.Credentials{AccessKey: entry.Values["accessKey"], SecretKey: entry.Values["secretKey"], SessionToken: entry.Values["sessionToken"]}
	if storage.CredentialsRef == "borq/platform/rustfs" {
		credentials = objectstore.Credentials{AccessKey: entry.Values["RUSTFS_ACCESS_KEY"], SecretKey: entry.Values["RUSTFS_SECRET_KEY"]}
	}
	return storageClientConfig(storage, credentials), nil
}

// ResolvePostgresStorage enforces the organization grant and returns its stable isolated prefix.
func (manager Manager) ResolvePostgresStorage(ctx context.Context, organization, space string) (ManagementS3, objectstore.Config, error) {
	owner, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization)
	if err != nil {
		return ManagementS3{}, objectstore.Config{}, err
	}
	if !owner.ManagedAddons {
		return ManagementS3{}, objectstore.Config{}, postgresValidationError{message: "Managed PostgreSQL is disabled for this organization. Set managedAddons to true in management configuration to enable it."}
	}
	owner, err = manager.ensureOrganizationIdentity(ctx, owner)
	if err != nil {
		return ManagementS3{}, objectstore.Config{}, err
	}
	rotation, _, rotationErr := loadDocument[storageRotation](ctx, manager.database, "management-s3-rotation", "shared")
	if rotationErr != nil && !errors.Is(rotationErr, ErrNotFound) {
		return ManagementS3{}, objectstore.Config{}, rotationErr
	}
	if rotation.Status == "staged" {
		return ManagementS3{}, objectstore.Config{}, fmt.Errorf("%w: shared S3 credential rotation is still pending", ErrConflict)
	}
	target, err := manager.GetSpace(ctx, organization, space)
	if err != nil {
		return ManagementS3{}, objectstore.Config{}, err
	}
	storage, err := manager.ensureSpaceStorage(ctx, target)
	if err != nil {
		return ManagementS3{}, objectstore.Config{}, err
	}
	if err := manager.validatePostgresStorage(ctx, storage, target); err != nil {
		return ManagementS3{}, objectstore.Config{}, err
	}
	storage.Prefix = path.Join(storage.Prefix, "organizations", owner.ID, "postgres")
	config, err := manager.ResolveManagementS3(ctx, storage)
	config.ServerSideEncryption = "AES256"
	return storage, config, err
}

// PutManagementS3 queues a connection test before saving primary storage settings.
func (manager Manager) PutManagementS3(ctx context.Context, input ManagementS3Input) (Operation, error) {
	return manager.ApplyManagementConfiguration(ctx, ManagementConfiguration{S3: &input})
}

func (manager Manager) prepareManagementS3(ctx context.Context, input ManagementS3Input) (ManagementS3, error) {
	storage := ManagementS3{Endpoint: strings.TrimRight(input.Endpoint, "/"), Region: input.Region, Bucket: input.Bucket, Prefix: strings.Trim(input.Prefix, "/"), PathStyle: input.PathStyle, CredentialsRef: input.CredentialsRef}
	inline := input.AccessKey != "" || input.SecretKey != "" || input.SessionToken != ""
	if inline && input.CredentialsRef != "" {
		return ManagementS3{}, fmt.Errorf("%w: supply inline S3 credentials or credentialsRef, not both", ErrInvalid)
	}
	if inline && (input.AccessKey == "" || input.SecretKey == "") {
		return ManagementS3{}, fmt.Errorf("%w: S3 accessKey and secretKey are required together", ErrInvalid)
	}
	if !inline && !validStorageCredentialsRef(input.CredentialsRef) {
		return ManagementS3{}, fmt.Errorf("%w: S3 credentialsRef is required", ErrInvalid)
	}
	if err := manager.validateManagementStorage(storage); err != nil {
		return ManagementS3{}, err
	}
	if err := storageClientConfig(storage, objectstore.Credentials{}).Validate(); err != nil {
		return ManagementS3{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if inline {
		identity, err := newId()
		if err != nil {
			return ManagementS3{}, err
		}
		storage.CredentialsRef = "borq/management/s3/" + identity
		vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
		if err != nil {
			return ManagementS3{}, err
		}
		if _, err := vault.WriteKv(ctx, "secret", storage.CredentialsRef, openbao.WriteKvInput{Values: map[string]string{"accessKey": input.AccessKey, "secretKey": input.SecretKey, "sessionToken": input.SessionToken}}); err != nil {
			return ManagementS3{}, err
		}
	}
	return storage, nil
}

func (manager Manager) testManagementS3(ctx context.Context, storage ManagementS3) error {
	config, err := manager.ResolveManagementS3(ctx, storage)
	if err != nil {
		return fmt.Errorf("%w: Primary S3 connection test could not read the stored credentials. Saved settings are unchanged.", ErrInvalid)
	}
	client, err := objectstore.New(config)
	if err != nil {
		return fmt.Errorf("%w: Primary S3 connection test failed. Check the connection settings and credentials. Saved settings are unchanged.", ErrInvalid)
	}
	if err := client.Validate(ctx); err != nil {
		return fmt.Errorf("%w: Primary S3 connection test failed. Check the endpoint, bucket, credentials, and list, write, read, and delete permissions. Saved settings are unchanged.", ErrInvalid)
	}
	return nil
}

func (manager Manager) activateManagementS3(ctx context.Context, storage ManagementS3) (ManagementS3, error) {
	before, previous, err := loadDocument[ManagementS3](ctx, manager.database, "management-s3", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ManagementS3{}, err
	}
	if errors.Is(err, ErrNotFound) {
		before, _ = manager.GetManagementS3(ctx)
	}
	dependencies, err := manager.storageDependencies(ctx)
	if err != nil {
		return ManagementS3{}, err
	}
	managementDependency, err := manager.managementStorageDependency(ctx)
	if err != nil {
		return ManagementS3{}, err
	}
	if (dependencies || (managementDependency && before.Bucket != "")) && !sameStorageLocation(before, storage) {
		return ManagementS3{}, fmt.Errorf("%w: shared S3 location is used by management logs, live addons or preserved archives", ErrConflict)
	}
	return manager.commitManagementS3(ctx, before, storage, previous.Revision)
}

// DeleteManagementS3 refuses to orphan any live or preserved managed database.
func (manager Manager) DeleteManagementS3(ctx context.Context) error {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	dependencies, err := manager.storageDependencies(ctx)
	if err != nil {
		return err
	}
	if dependencies {
		return errors.Join(ErrConflict, ErrManagementStorageInUse)
	}
	components, _, componentErr := loadDocument[componentRecord](ctx, manager.database, "components", "management")
	if componentErr != nil && !errors.Is(componentErr, ErrNotFound) {
		return componentErr
	}
	if components.Installation.ClusterID != "" {
		return errors.Join(ErrConflict, ErrManagementStorageInUse)
	}
	_, record, err := loadDocument[ManagementS3](ctx, manager.database, "management-s3", "shared")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{{Collection: "management-s3", Key: "shared", ExpectedRevision: record.Revision, Delete: true}})
}

func (manager Manager) storageDependencies(ctx context.Context) (bool, error) {
	if retained, err := manager.spaceStorageInUse(ctx); retained || err != nil {
		return retained, err
	}
	if retained, err := manager.sqliteStorageInUse(ctx); retained || err != nil {
		return retained, err
	}
	for _, collection := range []string{"postgres", "postgres-archive"} {
		records, err := manager.database.List(ctx, collection)
		if err != nil {
			return false, err
		}
		for _, record := range records {
			if collection == "postgres-archive" {
				return true, nil
			}
			var item struct {
				Postgres struct {
					State string `json:"state"`
				} `json:"postgres"`
			}
			if err := json.Unmarshal(record.Value, &item); err != nil {
				return false, err
			}
			if item.Postgres.State != "removed" {
				return true, nil
			}
		}
	}
	return false, nil
}

func validStorageCredentialsRef(reference string) bool {
	if reference == "borq/platform/rustfs" {
		return true
	}
	return strings.HasPrefix(reference, "borq/management/s3/") && len(strings.TrimPrefix(reference, "borq/management/s3/")) == 32 && strings.Trim(strings.TrimPrefix(reference, "borq/management/s3/"), "0123456789abcdef") == ""
}
func storageClientConfig(storage ManagementS3, credentials objectstore.Credentials) objectstore.Config {
	return objectstore.Config{Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: storage.Prefix, PathStyle: storage.PathStyle, Credentials: credentials}
}
func sameStorageLocation(before, after ManagementS3) bool {
	return before.Endpoint == after.Endpoint && before.Region == after.Region && before.Bucket == after.Bucket && before.Prefix == after.Prefix && before.PathStyle == after.PathStyle
}
func (manager Manager) validateProductionStorage(storage ManagementS3) error {
	profile := manager.runtime.platformSettings.Profile
	if profile == "" || profile == "dev" {
		return nil
	}
	endpoint, err := url.Parse(storage.Endpoint)
	if err != nil {
		return fmt.Errorf("%w: invalid S3 endpoint", ErrInvalid)
	}
	host := strings.ToLower(endpoint.Hostname())
	address := net.ParseIP(host)
	if (storage.CredentialsRef == "borq/platform/rustfs" && host == "") || host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") || (address != nil && address.IsLoopback()) {
		return fmt.Errorf("%w: primary S3 storage uses a local endpoint; production deployment clusters require a reachable primary s3.endpoint. offsite.repository configures a separate backup destination", ErrInvalid)
	}
	return nil
}

func (manager Manager) managementStorageService(storage ManagementS3) bool {
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	return storage.Endpoint == "http://rustfs-svc."+namespace+".svc:9000" ||
		storage.Endpoint == "http://rustfs-svc."+namespace+".svc.cluster.local:9000"
}

func (manager Manager) validateManagementStorage(storage ManagementS3) error {
	// Management can use its own RustFS Service. A grant has no deployment
	// target yet; the PostgreSQL storage boundary checks the actual cluster.
	if storage.CredentialsRef == "borq/platform/rustfs" && manager.managementStorageService(storage) {
		return nil
	}
	return manager.validateProductionStorage(storage)
}

func (manager Manager) validatePostgresStorage(ctx context.Context, storage ManagementS3, space Space) error {
	err := manager.validateProductionStorage(storage)
	if err == nil || !manager.managementStorageService(storage) {
		return err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
	if err != nil {
		return err
	}
	local, err := manager.applicationOnManagementCluster(ctx, cluster)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("%w: primary RustFS uses management-cluster DNS; configure a primary s3.endpoint reachable from this deployment cluster. offsite.repository configures a separate backup destination", ErrInvalid)
	}
	return nil
}

func (manager Manager) managementStorageDependency(ctx context.Context) (bool, error) {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings.Profile != "" && settings.Profile != "dev" && settings.S3.Bucket == "" {
		return false, nil
	}

	components, _, err := loadDocument[componentRecord](ctx, manager.database, "components", "management")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, component := range components.Installation.Result.Components {
		if component.Name == "loki" && component.Status != "absent" {
			return true, nil
		}
	}
	return false, nil
}
