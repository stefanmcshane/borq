package openbao

import (
	"context"
	"errors"
)

// KvDocument carries a native KV v2 document for protocols that own their own schema rather than Borq's values wrapper.
type KvDocument struct {
	// Version is the current compare-and-set version.
	Version int
	// Data contains the external protocol's document fields.
	Data map[string]any
}

// ReadKvDocument reads a native document without assuming Borq's string-valued wrapper.
func (client *Client) ReadKvDocument(ctx context.Context, mount, path string) (KvDocument, error) {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return KvDocument{}, err
	}
	secret, err := client.cli.KVv2(mount).Get(ctx, path)
	if err != nil {
		return KvDocument{}, kvRequestError(ctx, "read native document", err)
	}
	if secret == nil || secret.Data == nil {
		return KvDocument{}, ErrNotFound
	}
	if secret.VersionMetadata == nil || secret.VersionMetadata.Version < 1 {
		return KvDocument{}, errors.New("OpenBao native document has invalid version metadata")
	}
	return KvDocument{Version: secret.VersionMetadata.Version, Data: secret.Data}, nil
}

// WriteKvDocument conditionally creates or replaces the caller-owned native protocol document.
func (client *Client) WriteKvDocument(ctx context.Context, mount, path string, document KvDocument) error {
	mount, path, err := kvLocation(mount, path)
	if err != nil {
		return err
	}
	if document.Version < 0 || document.Data == nil {
		return errors.New("OpenBao native document requires data and a nonnegative version")
	}
	_, err = client.cli.Logical().WriteWithContext(ctx, mount+"/data/"+path, map[string]any{"options": map[string]any{"cas": document.Version}, "data": document.Data})
	if err != nil {
		return kvRequestError(ctx, "write native document", err)
	}
	return nil
}
