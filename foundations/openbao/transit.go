package openbao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openbaoapi "github.com/openbao/openbao/api/v2"
)

// EnsureTransit creates a dedicated cryptographic engine without replacing an existing mount or key.
func (client *Client) EnsureTransit(ctx context.Context, mount string) error {
	mount = strings.Trim(mount, "/")
	if _, _, err := kvLocation(mount, "check"); err != nil {
		return err
	}
	mounts, err := client.cli.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return kvRequestError(ctx, "list encryption mounts", err)
	}
	if existing, exists := mounts[mount+"/"]; exists {
		if existing.Type != "transit" {
			return fmt.Errorf("openbao encryption mount %s must use Transit", mount)
		}
		return nil
	}
	if err := client.cli.Sys().MountWithContext(ctx, mount, &openbaoapi.MountInput{Type: "transit"}); err != nil {
		return kvRequestError(ctx, "enable encryption engine", err)
	}
	return nil
}

// TransitKey reports safety properties of an existing nonexportable encryption root.
type TransitKey struct {
	// Type identifies the cryptographic algorithm.
	Type string `json:"type"`
	// Exportable and DeletionAllowed must remain false for retained backup roots.
	Exportable      bool `json:"exportable"`
	DeletionAllowed bool `json:"deletion_allowed"`
}

// ReadTransitKey observes the existing encryption root without minting replacement material.
func (client *Client) ReadTransitKey(ctx context.Context, mount, name string) (TransitKey, error) {
	mount, name, err := kvLocation(mount, name)
	if err != nil {
		return TransitKey{}, err
	}
	secret, err := client.cli.Logical().ReadWithContext(ctx, mount+"/keys/"+name)
	if err != nil {
		return TransitKey{}, kvRequestError(ctx, "read encryption root", err)
	}
	if secret == nil {
		return TransitKey{}, ErrNotFound
	}
	data, err := json.Marshal(secret.Data)
	if err != nil {
		return TransitKey{}, errors.New("OpenBao encryption root metadata cannot be parsed")
	}
	var key TransitKey
	if err := json.Unmarshal(data, &key); err != nil || key.Type == "" {
		return TransitKey{}, errors.New("OpenBao encryption root metadata is invalid")
	}
	return key, nil
}

// CreateTransitKey asks OpenBao to generate a retained AES256 root; keys remain nonexportable and deletion-protected.
func (client *Client) CreateTransitKey(ctx context.Context, mount, name string) error {
	mount, name, err := kvLocation(mount, name)
	if err != nil {
		return err
	}
	_, err = client.cli.Logical().WriteWithContext(ctx, mount+"/keys/"+name, map[string]any{"type": "aes256-gcm96", "exportable": false, "allow_plaintext_backup": false})
	if err != nil {
		return kvRequestError(ctx, "create encryption root", err)
	}
	return nil
}

// EncryptTransit wraps a short caller-owned root-binding probe with an existing key.
func (client *Client) EncryptTransit(ctx context.Context, mount, name string, plaintext []byte) (string, error) {
	mount, name, err := kvLocation(mount, name)
	if err != nil {
		return "", err
	}
	secret, err := client.cli.Logical().WriteWithContext(ctx, mount+"/encrypt/"+name, map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plaintext)})
	if err != nil {
		return "", kvRequestError(ctx, "encrypt root-binding probe", err)
	}
	if secret == nil {
		return "", errors.New("OpenBao did not return a root-binding probe")
	}
	ciphertext, ok := secret.Data["ciphertext"].(string)
	if !ok || !strings.HasPrefix(ciphertext, "vault:v") {
		return "", errors.New("OpenBao returned an invalid root-binding probe")
	}
	return ciphertext, nil
}

// DecryptTransit validates retained key continuity without exporting the master key.
func (client *Client) DecryptTransit(ctx context.Context, mount, name, ciphertext string) ([]byte, error) {
	mount, name, err := kvLocation(mount, name)
	if err != nil {
		return nil, err
	}
	secret, err := client.cli.Logical().WriteWithContext(ctx, mount+"/decrypt/"+name, map[string]any{"ciphertext": ciphertext})
	if err != nil {
		return nil, kvRequestError(ctx, "verify encryption root continuity", err)
	}
	if secret == nil {
		return nil, errors.New("OpenBao did not return the root-binding probe")
	}
	plaintext, ok := secret.Data["plaintext"].(string)
	if !ok {
		return nil, errors.New("OpenBao returned an invalid root-binding probe")
	}
	decoded, err := base64.StdEncoding.DecodeString(plaintext)
	if err != nil {
		return nil, errors.New("OpenBao returned a malformed root-binding probe")
	}
	return decoded, nil
}
