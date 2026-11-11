package managementbackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// SpaceObject identifies verified PostgreSQL bytes or a selected SQLite generation.
type SpaceObject struct {
	// Key preserves the captured relative backup object path.
	Key string
	// Size and SHA256 bind the copy to the completed offsite manifest.
	Size   int64
	SHA256 string
}

// SpaceRecovery reconnects one recovered space to its independently rehydrated bucket.
type SpaceRecovery struct {
	// SpaceID selects the immutable retained space, never a friendly name.
	SpaceID string
	// Source identifies the completed snapshot's original S3 connection.
	Source string
	// Storage supplies fresh administrative access to the rehydrated space bucket.
	Storage objectstore.Config
	// Objects contains all captured PostgreSQL objects and one selected SQLite generation per application.
	Objects []SpaceObject
	// OpenBao authenticates to the restored vault using independently retained material.
	OpenBao OpenBaoRecovery
}

type recoveryStorage struct {
	Endpoint       string `json:"endpoint,omitempty"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix,omitempty"`
	PathStyle      bool   `json:"pathStyle"`
	CredentialsRef string `json:"credentialsRef"`
}

// RebindSpace copies PostgreSQL objects and selected SQLite generations to a fresh live prefix and reconnects saved state.
// Original recovered generations remain intact for independent historical restores.
func RebindSpace(ctx context.Context, directory string, input SpaceRecovery) error {
	if _, err := os.Stat(filepath.Join(directory, "RECOVERY_REQUIRED")); err != nil {
		return errors.New("space rebinding requires the management recovery barrier")
	}
	if input.Storage.Prefix == "" || len(input.SpaceID) != 32 || strings.Trim(input.SpaceID, "0123456789abcdef") != "" || input.Storage.Credentials.SessionToken != "" {
		return errors.New("space recovery requires its identity, an unpublished prefix, and persistent RustFS credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	database, err := sqlite.New(ctx, filepath.Join(directory, "borq.sqlite"))
	if err != nil {
		return err
	}
	defer database.Close()
	storageRecord, err := database.Get(ctx, "space-storage", input.SpaceID)
	if err != nil {
		return err
	}
	var stored map[string]json.RawMessage
	if json.Unmarshal(storageRecord.Value, &stored) != nil {
		return errors.New("invalid recovered space storage")
	}
	var previous recoveryStorage
	if json.Unmarshal(stored["storage"], &previous) != nil {
		return errors.New("invalid recovered space location")
	}
	digest := sha256.Sum256([]byte(strings.TrimRight(previous.Endpoint, "/") + "\n" + previous.Region + "\n" + previous.Bucket + "\n" + strings.Trim(previous.Prefix, "/")))
	if hex.EncodeToString(digest[:16]) != input.Source {
		return errors.New("recovery manifest does not belong to this space's original storage")
	}
	spaces, err := database.ListBounded(ctx, "space-storage", 10000, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range spaces {
		var other struct {
			Storage recoveryStorage `json:"storage"`
		}
		if json.Unmarshal(record.Value, &other) != nil {
			return errors.New("invalid retained space storage")
		}
		if record.Key != input.SpaceID && other.Storage.Bucket == input.Storage.Bucket {
			return errors.New("a recovered space must have its own bucket")
		}
	}
	if record, err := database.Get(ctx, "management-s3", "shared"); err == nil {
		var management recoveryStorage
		if json.Unmarshal(record.Value, &management) != nil {
			return errors.New("invalid recovered management storage")
		}
		if management.Bucket == input.Storage.Bucket {
			return errors.New("space recovery cannot share the management bucket")
		}
	} else {
		return errors.New("rebind management storage with local rebind before rebinding a space")
	}
	identity := make([]byte, 16)
	if _, err := rand.Read(identity); err != nil {
		return err
	}
	parentRef := "borq/management/s3/" + hex.EncodeToString(identity)
	live := input.Storage
	live.Prefix = path.Join(input.Storage.Prefix, "live-"+hex.EncodeToString(identity))
	if err := copySpaceObjects(ctx, input, live); err != nil {
		return err
	}
	admin := input.Storage
	admin.Prefix = ""
	credentials, err := objectstore.RustfsCredentials(ctx, admin)
	if err != nil {
		return errors.New("create recovered space's restricted RustFS account")
	}
	vault, err := openbao.NewClient(openbao.NewClientInput{Address: input.OpenBao.Address, Token: input.OpenBao.OriginalToken})
	if err != nil {
		return err
	}
	if err := vault.CheckToken(ctx); err != nil {
		return err
	}
	if _, err := vault.WriteKv(ctx, "secret", parentRef, openbao.WriteKvInput{Values: map[string]string{"accessKey": admin.Credentials.AccessKey, "secretKey": admin.Credentials.SecretKey}}); err != nil {
		return errors.New("retain replacement storage administrator access in the recovered vault")
	}
	if err := replaceRecoveryCredentials(ctx, *vault, previous.CredentialsRef, map[string]string{"accessKey": credentials.AccessKey, "secretKey": credentials.SecretKey}); err != nil {
		return errors.New("retain recovered space credentials in the restored vault")
	}
	connection := recoveryStorage{Endpoint: live.Endpoint, Region: live.Region, Bucket: live.Bucket, Prefix: live.Prefix, PathStyle: live.PathStyle, CredentialsRef: previous.CredentialsRef}
	parent := connection
	parent.Prefix, parent.CredentialsRef = "", parentRef
	stored["storage"], _ = json.Marshal(connection)
	stored["parent"], _ = json.Marshal(parent)
	stored["status"], _ = json.Marshal(map[string]string{"bucket": live.Bucket, "state": "ready", "message": "Recovered space storage is ready; deploy a fresh application plan."})
	value, _ := json.Marshal(stored)
	writes := []sqlite.Write{{Collection: storageRecord.Collection, Key: storageRecord.Key, ExpectedRevision: storageRecord.Revision, Value: value}}
	spaceRecords, err := database.ListBounded(ctx, "space", 10000, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range spaceRecords {
		var space map[string]json.RawMessage
		var target struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(record.Value, &space) != nil || json.Unmarshal(space["space"], &target) != nil {
			return errors.New("invalid recovered space")
		}
		if target.ID != input.SpaceID {
			continue
		}
		space["sqliteStorage"], _ = json.Marshal(connection)
		space["activeOperation"], space["latestOperation"], space["queue"] = json.RawMessage(`""`), json.RawMessage(`""`), json.RawMessage(`[]`)
		value, _ := json.Marshal(space)
		writes = append(writes, sqlite.Write{Collection: record.Collection, Key: record.Key, ExpectedRevision: record.Revision, Value: value})
	}
	archives, err := database.ListBounded(ctx, "postgres-archive", 10000, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range archives {
		var archive map[string]any
		if json.Unmarshal(record.Value, &archive) != nil {
			return errors.New("invalid preserved PostgreSQL archive")
		}
		if archive["credentialsRef"] != previous.CredentialsRef {
			continue
		}
		prefix, _ := archive["prefix"].(string)
		relative := strings.TrimPrefix(prefix, strings.Trim(previous.Prefix, "/")+"/")
		if previous.Prefix == "" {
			relative = prefix
		}
		if !strings.HasPrefix(relative, "organizations/") {
			return errors.New("preserved archive prefix is outside its original space root")
		}
		archive["endpoint"], archive["region"], archive["bucket"], archive["prefix"], archive["pathStyle"] = input.Storage.Endpoint, input.Storage.Region, input.Storage.Bucket, path.Join(live.Prefix, relative), input.Storage.PathStyle
		value, _ := json.Marshal(archive)
		writes = append(writes, sqlite.Write{Collection: record.Collection, Key: record.Key, ExpectedRevision: record.Revision, Value: value})
	}
	operations, err := database.ListBounded(ctx, "operation", 10000, 32<<20)
	if err != nil {
		return err
	}
	for _, record := range operations {
		var operation map[string]any
		if json.Unmarshal(record.Value, &operation) != nil {
			return errors.New("invalid saved application operation")
		}
		status, _ := operation["status"].(string)
		if operation["spaceId"] != input.SpaceID || status == "succeeded" || status == "failed" || status == "canceled" || status == "superseded" {
			continue
		}
		operation["status"], operation["message"], operation["updatedAt"] = "failed", "Storage was rebound during recovery; create a fresh deployment plan.", time.Now().UTC()
		value, _ := json.Marshal(operation)
		writes = append(writes, sqlite.Write{Collection: record.Collection, Key: record.Key, ExpectedRevision: record.Revision, Value: value})
	}
	return database.Commit(ctx, writes)
}

func copySpaceObjects(ctx context.Context, input SpaceRecovery, destination objectstore.Config) error {
	source, err := objectstore.New(input.Storage)
	if err != nil {
		return err
	}
	target, err := objectstore.New(destination)
	if err != nil {
		return err
	}
	objects := map[string]objectstore.Object{}
	for _, prefix := range []string{"organizations/", "sqlite/" + input.SpaceID + "/"} {
		inventory, err := source.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, object := range inventory {
			objects[object.Key] = object
		}
	}
	for _, captured := range input.Objects {
		object, exists := objects[captured.Key]
		if !exists || object.Size != captured.Size || !(strings.HasPrefix(captured.Key, "sqlite/"+input.SpaceID+"/") || strings.HasPrefix(captured.Key, "organizations/")) || len(captured.SHA256) != 64 {
			return errors.New("selected backup object is incomplete or outside the recovered space")
		}
		body, err := source.OpenCaptured(ctx, object)
		if err != nil {
			return err
		}
		digest := sha256.New()
		err = target.PutNew(ctx, captured.Key, io.TeeReader(body, digest), captured.Size)
		closeErr := body.Close()
		if err != nil {
			return err
		}
		if closeErr != nil || hex.EncodeToString(digest.Sum(nil)) != captured.SHA256 {
			return errors.New("recovered backup object does not match the completed manifest")
		}
	}
	return nil
}
