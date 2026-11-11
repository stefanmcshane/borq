package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/openbao"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ReconcileRustfsEncryption prepares the retained OpenBao KMS engine and renewable scoped credential before ESO projects RustFS's Secret.
// Borq provisions one nonexportable Transit root and native RustFS metadata; the runtime token cannot recreate either after loss.
func ReconcileRustfsEncryption(ctx context.Context, vault *openbao.Client) error {
	if err := vault.EnsureTransit(ctx, "borq-rustfs-kms"); err != nil {
		return err
	}
	if err := vault.PutPolicy(ctx, "borq-rustfs-kms", rustfsEncryptionPolicy()); err != nil {
		return err
	}
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil {
		return err
	}
	if entry.Values["RUSTFS_ACCESS_KEY"] == "" || entry.Values["RUSTFS_SECRET_KEY"] == "" {
		return errors.New("RustFS credentials must be prepared before encryption")
	}
	if err := ensureRustfsEncryptionRoot(ctx, vault, entry); err != nil {
		return err
	}
	entry, err = vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil {
		return err
	}
	if token := entry.Values["RUSTFS_KMS_VAULT_TOKEN"]; token != "" && vault.RenewToken(ctx, token) == nil {
		return nil
	}
	token, err := vault.CreatePeriodicToken(ctx, "borq-rustfs-kms")
	if err != nil {
		return err
	}
	entry.Values["RUSTFS_KMS_VAULT_TOKEN"] = token
	if _, err := vault.WriteKv(ctx, "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: entry.Values, CheckVersion: entry.Version}); err != nil {
		return fmt.Errorf("retain RustFS encryption credential: %w", err)
	}
	return nil
}

func rustfsEncryptionPolicy() string {
	// RustFS 1.0.1 renews lease-bound static tokens itself after lookup-self discovers the period.
	// Its synthetic health check creates a separate reserved key; permissions on
	// that key must not permit recreation or metadata changes of the backup root.
	return `path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "borq-rustfs-kms/keys" { capabilities = ["list"] }
path "borq-rustfs-kms/keys/borq-managed-backups-v1" { capabilities = ["read"] }
path "borq-rustfs-kms/encrypt/borq-managed-backups-v1" { capabilities = ["update"] }
path "borq-rustfs-kms/decrypt/borq-managed-backups-v1" { capabilities = ["update"] }
path "secret/data/borq/platform/rustfs-kms/metadata/borq-managed-backups-v1" { capabilities = ["read"] }
path "secret/metadata/borq/platform/rustfs-kms/metadata" { capabilities = ["list"] }
path "secret/metadata/borq/platform/rustfs-kms/metadata/borq-managed-backups-v1" { capabilities = ["read"] }
path "borq-rustfs-kms/keys/rustfs-internal-kms-probe" { capabilities = ["create", "read", "update"] }
path "borq-rustfs-kms/encrypt/rustfs-internal-kms-probe" { capabilities = ["update"] }
path "borq-rustfs-kms/decrypt/rustfs-internal-kms-probe" { capabilities = ["update"] }
path "secret/data/borq/platform/rustfs-kms/metadata/rustfs-internal-kms-probe" { capabilities = ["create", "read", "update"] }
path "secret/metadata/borq/platform/rustfs-kms/metadata/rustfs-internal-kms-probe" { capabilities = ["read"] }
`
}

// ReconcileRustfsEncryptionProjection rolls an existing owned RustFS Deployment after ESO has projected the retained KMS token.
// A token digest detects replacement without putting the credential in annotations or Helm history.
func ReconcileRustfsEncryptionProjection(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	vault, err := OpenManagementVault(ctx, connection, settings.Namespace)
	if err != nil {
		return err
	}
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil {
		return err
	}
	expected := []byte(entry.Values["RUSTFS_KMS_VAULT_TOKEN"])
	if len(expected) == 0 {
		return errors.New("retained RustFS encryption credential is missing")
	}
	wait, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	secret := corev1.Secret{}
	for {
		err := client.Get(wait, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "borq-rustfs"}, &secret)
		if err == nil && bytes.Equal(secret.Data["RUSTFS_KMS_VAULT_TOKEN"], expected) {
			break
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("read projected RustFS encryption credentials")
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("wait for the retained RustFS encryption credential projection: %w", wait.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "borq" {
		return errors.New("RustFS encryption credentials are not owned by Borq")
	}
	deployment := appsv1.Deployment{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "rustfs"}, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read RustFS Deployment")
	}
	if deployment.Annotations["meta.helm.sh/release-name"] != "rustfs" || deployment.Annotations["meta.helm.sh/release-namespace"] != settings.Namespace {
		return errors.New("RustFS Deployment is outside the managed release")
	}
	trust := corev1.Secret{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "borq-openbao-kms-tls"}, &trust); err != nil {
		return errors.New("read OpenBao KMS transport trust root")
	}
	if trust.Labels["app.kubernetes.io/managed-by"] != "borq" || len(trust.Data["ca.crt"]) == 0 {
		return errors.New("OpenBao KMS transport trust root is missing or not owned by Borq")
	}
	const annotation = "borq.run/rustfs-kms-credentials-sha256"
	material := append(bytes.Clone(secret.Data["RUSTFS_KMS_VAULT_TOKEN"]), trust.Data["ca.crt"]...)
	digest := sha256.Sum256(material)
	version := hex.EncodeToString(digest[:])
	if deployment.Spec.Template.Annotations[annotation] != version {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations[annotation] = version
		if err := client.Update(ctx, &deployment); err != nil {
			return fmt.Errorf("roll out RustFS encryption credential replacement: %w", err)
		}
	}
	return connection.Deployments().WaitForRollout(ctx, settings.Namespace, "rustfs")
}

func ensureRustfsEncryptionRoot(ctx context.Context, vault *openbao.Client, entry openbao.KvEntry) error {
	const mount = "borq-rustfs-kms"
	const name = "borq-managed-backups-v1"
	const metadataPath = "borq/platform/rustfs-kms/metadata/borq-managed-backups-v1"
	const probe = "borq:rustfs:managed-backups:v1"
	if entry.Values["RUSTFS_KMS_ROOT_PROBE"] != "" {
		return CheckRetainedRustfsEncryptionRoot(ctx, vault)
	}
	key, keyError := vault.ReadTransitKey(ctx, mount, name)
	metadata, metadataError := vault.ReadKvDocument(ctx, "secret", metadataPath)
	if keyError != nil && !errors.Is(keyError, openbao.ErrNotFound) {
		return keyError
	}
	if metadataError != nil && !errors.Is(metadataError, openbao.ErrNotFound) {
		return metadataError
	}
	if errors.Is(keyError, openbao.ErrNotFound) {
		if metadataError == nil || entry.Values["RUSTFS_KMS_VAULT_TOKEN"] != "" {
			return errors.New("RustFS encryption key is missing but prior KMS state exists; restore the original OpenBao key")
		}
		if err := vault.CreateTransitKey(ctx, mount, name); err != nil {
			return err
		}
		key, keyError = vault.ReadTransitKey(ctx, mount, name)
	}
	if keyError != nil {
		return keyError
	}
	if errors.Is(metadataError, openbao.ErrNotFound) {
		if entry.Values["RUSTFS_KMS_VAULT_TOKEN"] != "" {
			return errors.New("retained RustFS key metadata is missing; restore OpenBao before accepting archives")
		}
		// Native RustFS 1.0.1 TransitKeyMetadataPersisted protocol; no encryption material is stored in this document.
		metadata = openbao.KvDocument{Data: map[string]any{"key_usage": "EncryptDecrypt", "key_state": "Enabled", "description": "Borq managed backup encryption root", "tags": map[string]string{}, "created_at": time.Now().UTC().Format(time.RFC3339Nano) + "[UTC]", "deletion_date": nil, "origin": "VAULT_TRANSIT", "created_by": "borq", "current_version": 1}}
		if err := vault.WriteKvDocument(ctx, "secret", metadataPath, metadata); err != nil {
			return err
		}
	}
	ciphertext, err := vault.EncryptTransit(ctx, mount, name, []byte(probe))
	if err != nil {
		return err
	}
	entry.Values["RUSTFS_KMS_ROOT_PROBE"] = ciphertext
	if _, err := vault.WriteKv(ctx, "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: entry.Values, CheckVersion: entry.Version}); err != nil {
		return err
	}
	if key.Exportable || key.DeletionAllowed || key.Type != "aes256-gcm96" || metadata.Data["key_usage"] != "EncryptDecrypt" || metadata.Data["key_state"] != "Enabled" {
		return errors.New("RustFS encryption root must remain enabled, nonexportable and deletion-protected")
	}
	return nil
}

// CheckRustfsEncryptionContinuity fences empty or unrelated vault state before RustFS credentials or a replacement root can be projected over surviving archive resources.
func CheckRustfsEncryptionContinuity(ctx context.Context, connection kubernetes.Connection, settings Settings, vault *openbao.Client) error {
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	prior := corev1.Secret{}
	err = client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "borq-rustfs"}, &prior)
	if apierrors.IsNotFound(err) {
		claim := corev1.PersistentVolumeClaim{}
		err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "rustfs-data"}, &claim)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect retained RustFS archive storage")
		}
		if claim.Annotations["meta.helm.sh/release-name"] != "rustfs" || claim.Annotations["meta.helm.sh/release-namespace"] != settings.Namespace {
			return errors.New("RustFS archive volume is outside the managed release")
		}
		if entry.Values["RUSTFS_KMS_ROOT_PROBE"] == "" {
			return errors.New("retained RustFS storage exists without encryption continuity evidence; restore the original OpenBao state")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect prior RustFS encryption credentials")
	}
	owned := prior.Labels["app.kubernetes.io/managed-by"] == "borq"
	for _, owner := range prior.OwnerReferences {
		owned = owned || owner.APIVersion == "external-secrets.io/v1" && owner.Kind == "ExternalSecret" && owner.Name == "borq-rustfs"
	}
	if !owned {
		return errors.New("prior RustFS credentials are outside the managed release")
	}
	if string(prior.Data["RUSTFS_ACCESS_KEY"]) != entry.Values["RUSTFS_ACCESS_KEY"] || string(prior.Data["RUSTFS_SECRET_KEY"]) != entry.Values["RUSTFS_SECRET_KEY"] {
		return errors.New("retained RustFS credentials no longer match OpenBao; restore the original vault state")
	}
	if probe := string(prior.Data["RUSTFS_KMS_ROOT_PROBE"]); probe != "" && probe != entry.Values["RUSTFS_KMS_ROOT_PROBE"] {
		return errors.New("retained RustFS encryption root no longer matches OpenBao; restore the original vault state")
	}
	if len(prior.Data["RUSTFS_KMS_VAULT_TOKEN"]) != 0 && entry.Values["RUSTFS_KMS_ROOT_PROBE"] == "" {
		return errors.New("prior RustFS encryption state exists without its root; restore OpenBao before accepting archives")
	}
	return nil
}

// CheckRetainedRustfsEncryptionRoot verifies original key continuity and native metadata before a backup checkpoint, without creating or repairing encryption state.
// Unprovisioned installations and external primary storage have no retained KMS root to validate.
func CheckRetainedRustfsEncryptionRoot(ctx context.Context, vault *openbao.Client) error {
	entry, err := vault.ReadKv(ctx, "secret", "borq/platform/rustfs", 0)
	if errors.Is(err, openbao.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	probe := entry.Values["RUSTFS_KMS_ROOT_PROBE"]
	if probe == "" {
		if entry.Values["RUSTFS_KMS_VAULT_TOKEN"] == "" {
			return nil
		}
		return errors.New("retained RustFS encryption credential has no root continuity probe")
	}
	key, err := vault.ReadTransitKey(ctx, "borq-rustfs-kms", "borq-managed-backups-v1")
	if err != nil {
		return errors.New("retained RustFS encryption root is missing; restore the original OpenBao state")
	}
	metadata, err := vault.ReadKvDocument(ctx, "secret", "borq/platform/rustfs-kms/metadata/borq-managed-backups-v1")
	if err != nil {
		return errors.New("retained RustFS native key metadata is missing; restore the original OpenBao state")
	}
	if key.Exportable || key.DeletionAllowed || key.Type != "aes256-gcm96" || metadata.Data["key_usage"] != "EncryptDecrypt" || metadata.Data["key_state"] != "Enabled" {
		return errors.New("RustFS encryption root must remain enabled, nonexportable and deletion-protected")
	}
	plaintext, err := vault.DecryptTransit(ctx, "borq-rustfs-kms", "borq-managed-backups-v1", probe)
	if err != nil || string(plaintext) != "borq:rustfs:managed-backups:v1" {
		return errors.New("RustFS encryption root no longer matches retained archives; restore the original OpenBao key")
	}
	return nil
}
