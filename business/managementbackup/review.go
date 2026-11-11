package managementbackup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// PendingOperation exposes identities and states for offline review without printing queued payloads or credentials.
type PendingOperation struct {
	// Collection, Key and Status identify retained operations without replaying them.
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Status     string `json:"status"`
}

// Inspect reads retained operation state while the recovery marker prevents normal manager startup.
func Inspect(ctx context.Context, directory string) ([]PendingOperation, error) {
	if _, err := os.Stat(filepath.Join(directory, "RECOVERY_REQUIRED")); err != nil {
		return nil, errors.New("offline inspection requires a recovered data directory with its replay barrier")
	}
	database, err := sqlite.New(ctx, filepath.Join(directory, "borq.sqlite"))
	if err != nil {
		return nil, err
	}
	defer database.Close()
	operations := []PendingOperation{}
	for _, collection := range []string{"operation", "management-operation", "postgres-operation", "addon-operation", "components"} {
		records, err := database.ListBounded(ctx, collection, 10000, 32<<20)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			var value struct {
				Status       string `json:"status"`
				Installation struct {
					Status string `json:"status"`
				} `json:"installation"`
			}
			if json.Unmarshal(record.Value, &value) != nil {
				return nil, errors.New("invalid retained operation state")
			}
			if collection == "components" {
				value.Status = value.Installation.Status
			}
			operations = append(operations, PendingOperation{Collection: collection, Key: record.Key, Status: value.Status})
		}
	}
	return operations, nil
}

// Rebind replaces the recovered shared RustFS connection and its matching archive locations before replay is permitted.
// It also updates the restored vault's built-in storage key using independently supplied original administrative access.
func Rebind(ctx context.Context, directory string, storage objectstore.Config, recovery OpenBaoRecovery) error {
	if _, err := os.Stat(filepath.Join(directory, "RECOVERY_REQUIRED")); err != nil {
		return errors.New("storage rebinding requires the recovery replay barrier")
	}
	if storage.Credentials.SessionToken != "" {
		return errors.New("RustFS rebinding requires persistent access and secret keys without a session token")
	}
	client, err := objectstore.New(storage)
	if err != nil {
		return err
	}
	if err := client.Validate(ctx); err != nil {
		return errors.New("new recovery storage did not pass normal access checks")
	}
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: recovery.Address, Token: recovery.OriginalToken})
	if err != nil {
		return errors.New("open restored vault for storage rebinding")
	}
	if err := vault.CheckToken(ctx); err != nil {
		return err
	}
	database, err := sqlite.New(ctx, filepath.Join(directory, "borq.sqlite"))
	if err != nil {
		return err
	}
	defer database.Close()
	storageRecords, err := database.ListBounded(ctx, "management-s3", 1, 1<<20)
	if err != nil {
		return err
	}
	var previous sqlite.Record
	for _, record := range storageRecords {
		if record.Key != "shared" {
			return errors.New("invalid recovered storage connection identity")
		}
		previous = record
	}
	var old struct {
		Endpoint       string `json:"endpoint"`
		Bucket         string `json:"bucket"`
		Prefix         string `json:"prefix"`
		CredentialsRef string `json:"credentialsRef"`
	}
	if len(previous.Value) > 0 && json.Unmarshal(previous.Value, &old) != nil {
		return errors.New("invalid recovered storage connection")
	}
	updated := map[string]any{"endpoint": storage.Endpoint, "region": storage.Region, "bucket": storage.Bucket, "prefix": storage.Prefix, "pathStyle": storage.PathStyle, "credentialsRef": "borq/platform/rustfs"}
	body, _ := json.Marshal(updated)
	writes := []sqlite.Write{{Collection: "management-s3", Key: "shared", ExpectedRevision: previous.Revision, Value: body}}
	spaceRecords, err := database.ListBounded(ctx, "space-storage", 10000, 32<<20)
	if err != nil {
		return err
	}
	spaceCredentials := map[string]bool{}
	for _, record := range spaceRecords {
		var space struct {
			Storage recoveryStorage `json:"storage"`
		}
		if json.Unmarshal(record.Value, &space) != nil {
			return errors.New("invalid retained space storage")
		}
		if space.Storage.Bucket == storage.Bucket {
			return errors.New("management recovery cannot share a space bucket")
		}
		spaceCredentials[space.Storage.CredentialsRef] = true
	}
	records, err := database.ListBounded(ctx, "postgres-archive", 10000, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range records {
		var archive map[string]any
		if json.Unmarshal(record.Value, &archive) != nil {
			return errors.New("invalid preserved PostgreSQL archive location")
		}
		if reference, _ := archive["credentialsRef"].(string); spaceCredentials[reference] {
			continue
		}
		if old.Bucket != "" && (archive["bucket"] != old.Bucket || archive["endpoint"] != old.Endpoint) {
			continue
		}
		prefix, _ := archive["prefix"].(string)
		relative := strings.TrimPrefix(prefix, strings.Trim(old.Prefix, "/"))
		relative = strings.TrimPrefix(relative, "/")
		if !strings.HasPrefix(relative, "organizations/") {
			return errors.New("preserved archive needs explicit location review before automatic rebinding")
		}
		archive["endpoint"], archive["region"], archive["bucket"], archive["prefix"], archive["pathStyle"], archive["credentialsRef"] = storage.Endpoint, storage.Region, storage.Bucket, path.Join(storage.Prefix, relative), storage.PathStyle, "borq/platform/rustfs"
		data, _ := json.Marshal(archive)
		writes = append(writes, sqlite.Write{Collection: record.Collection, Key: record.Key, ExpectedRevision: record.Revision, Value: data})
	}
	if err := replaceRecoveryCredentials(ctx, *vault, "borq/platform/rustfs", map[string]string{"RUSTFS_ACCESS_KEY": storage.Credentials.AccessKey, "RUSTFS_SECRET_KEY": storage.Credentials.SecretKey}); err != nil {
		return errors.New("rebind fresh RustFS credentials in the restored vault")
	}
	return database.Commit(ctx, writes)
}

// Resume removes only the explicit replay barrier after the operator has reviewed restored operations and connections.
func Resume(directory string) error {
	if err := os.Remove(filepath.Join(directory, "RECOVERY_REQUIRED")); err != nil {
		return errors.New("remove reviewed management recovery barrier")
	}
	return nil
}

// Recovery keeps workers paused while replacing existing vault documents with a checked version.
func replaceRecoveryCredentials(ctx context.Context, vault openbao.Client, reference string, values map[string]string) error {
	metadata, err := vault.ReadKvMetadata(ctx, "secret", reference)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	_, err = vault.WriteKv(ctx, "secret", reference, openbao.WriteKvInput{Values: values, CheckVersion: metadata.CurrentVersion})
	return err
}
