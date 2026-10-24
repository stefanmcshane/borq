package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	openbaoapi "github.com/openbao/openbao/api/v2"
)

// ErrNotFound identifies a missing, deleted, or destroyed KV entry. A package
// sentinel lets callers classify failures without seeing backend responses.
var ErrNotFound = errors.New("openbao kv entry not found")

// ErrConflict identifies a write whose check-and-set version no longer matches.
// A package sentinel lets callers retry safely without parsing backend errors.
var ErrConflict = errors.New("openbao kv version conflict")

// KvEntry contains one version of a string-valued KV v2 document.
type KvEntry struct {
	// Version is the immutable KV v2 version assigned by OpenBao.
	Version int
	// Values contains the document's string values, including empty strings.
	Values map[string]string
	// PlanID is an optional caller-supplied tag stored alongside Values.
	PlanID string
}

// KvMetadata describes the version history and retention of one KV v2 key.
type KvMetadata struct {
	// CurrentVersion is the most recently allocated version, even if deleted.
	CurrentVersion int
	// OldestVersion is the oldest version retained by OpenBao.
	OldestVersion int
	// MaxVersions is this key's retention override; zero uses the mount default.
	MaxVersions int
}

// WriteKvInput describes a conditional replacement of one KV v2 document.
type WriteKvInput struct {
	// Values is the complete replacement document, rather than a partial patch.
	Values map[string]string
	// PlanID is stored alongside Values so callers can identify their own write.
	PlanID string
	// CheckVersion must equal the current version; zero requires a new key.
	CheckVersion int
}

// ReadKv reads a specific KV v2 version, or the latest when version is zero.
// Documents contain a values object or JSON-encoded object string and an
// optional planId string.
func (c *Client) ReadKv(ctx context.Context, mount, path string, version int) (KvEntry, error) {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return KvEntry{}, err
	}
	if version < 0 {
		return KvEntry{}, errors.New("openbao kv version must not be negative")
	}

	// The SDK returns a pointer because a KV response may be absent.
	var secret *openbaoapi.KVSecret
	if version == 0 {
		secret, err = c.cli.KVv2(mount).Get(ctx, path)
	} else {
		secret, err = c.cli.KVv2(mount).GetVersion(ctx, path, version)
	}
	if err != nil {
		return KvEntry{}, kvRequestError(ctx, "read", err)
	}
	if secret.Data == nil {
		return KvEntry{}, ErrNotFound
	}
	if secret.VersionMetadata == nil || secret.VersionMetadata.Version < 1 {
		return KvEntry{}, errors.New("openbao kv read returned invalid version metadata")
	}
	values, ok := secret.Data["values"].(map[string]any)
	if encoded, isEncoded := secret.Data["values"].(string); isEncoded {
		ok = json.Unmarshal([]byte(encoded), &values) == nil
	}
	if !ok || values == nil {
		return KvEntry{}, errors.New("openbao kv document must contain a values object")
	}
	entry := KvEntry{
		Version: secret.VersionMetadata.Version,
		Values:  make(map[string]string, len(values)),
	}
	for key, value := range values {
		text, ok := value.(string)
		if !ok {
			return KvEntry{}, errors.New("openbao kv values must all be strings")
		}
		entry.Values[key] = text
	}
	if plan, exists := secret.Data["planId"]; exists {
		entry.PlanID, ok = plan.(string)
		if !ok {
			return KvEntry{}, errors.New("openbao kv planId must be a string")
		}
	}
	return entry, nil
}

// ReadKvMetadata reads key-level retention and version information.
func (c *Client) ReadKvMetadata(ctx context.Context, mount, path string) (KvMetadata, error) {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return KvMetadata{}, err
	}
	metadata, err := c.cli.KVv2(mount).GetMetadata(ctx, path)
	if err != nil {
		return KvMetadata{}, kvRequestError(ctx, "read metadata", err)
	}
	return KvMetadata{
		CurrentVersion: metadata.CurrentVersion,
		OldestVersion:  metadata.OldestVersion,
		MaxVersions:    metadata.MaxVersions,
	}, nil
}

// SetKvRetention sets a key's retained version count without changing the mount
// configuration or its other metadata. Zero restores the mount's default.
func (c *Client) SetKvRetention(ctx context.Context, mount, path string, maxVersions int) error {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return err
	}
	if maxVersions < 0 {
		return errors.New("openbao kv retention must not be negative")
	}
	_, err = c.cli.Logical().WriteWithContext(ctx, mount+"/metadata/"+path, map[string]any{"max_versions": maxVersions})
	if err != nil {
		return kvRequestError(ctx, "set retention", err)
	}
	return nil
}

// WriteKv conditionally replaces a KV v2 document, always sending check-and-set
// even for a new key. The returned entry contains the newly allocated version.
func (c *Client) WriteKv(ctx context.Context, mount, path string, input WriteKvInput) (KvEntry, error) {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return KvEntry{}, err
	}
	if input.CheckVersion < 0 {
		return KvEntry{}, errors.New("openbao kv check version must not be negative")
	}
	values := maps.Clone(input.Values)
	if values == nil {
		values = map[string]string{}
	}
	secret, err := c.cli.KVv2(mount).Put(ctx, path, map[string]any{
		"values": values,
		"planId": input.PlanID,
	}, openbaoapi.WithCheckAndSet(input.CheckVersion))
	if err != nil {
		return KvEntry{}, kvRequestError(ctx, "write", err)
	}
	if secret.VersionMetadata == nil || secret.VersionMetadata.Version < 1 {
		return KvEntry{}, errors.New("openbao kv write returned invalid version metadata")
	}
	return KvEntry{Version: secret.VersionMetadata.Version, Values: values, PlanID: input.PlanID}, nil
}

func kvLocation(mount, path string) (string, string, error) {
	mount = strings.Trim(mount, "/")
	path = strings.Trim(path, "/")
	for _, location := range []string{mount, path} {
		for _, segment := range strings.Split(location, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return "", "", errors.New("openbao kv mount and path must contain nonempty path segments")
			}
		}
	}
	return mount, path, nil
}

func kvRequestError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("openbao kv %s: %w", operation, ctx.Err())
	}
	if errors.Is(err, openbaoapi.ErrSecretNotFound) {
		return ErrNotFound
	}
	// The SDK error includes the response body and request URL, so only expose
	// the status and recognized classifications to avoid leaking credentials.
	var responseError *openbaoapi.ResponseError
	if errors.As(err, &responseError) {
		if responseError.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		if responseError.StatusCode == http.StatusBadRequest {
			for _, message := range responseError.Errors {
				if strings.Contains(message, "check-and-set parameter did not match the current version") {
					return ErrConflict
				}
			}
		}
		return fmt.Errorf("openbao kv %s failed with status %d", operation, responseError.StatusCode)
	}
	return fmt.Errorf("openbao kv %s request failed", operation)
}
