package platform

import (
	"context"
	"errors"

	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

const managementBackupKeyPath = "borq/platform/backup-encryption"

// ManagementBackupRecipient returns the retained public archive key, validating
// that the exported static seal key can reconstruct its private identity offline.
func ManagementBackupRecipient(ctx context.Context, kubeconfigPath, namespace string) (string, error) {
	if namespace == "" {
		namespace = ManagementNamespace
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	if err != nil {
		return "", err
	}
	vault, err := ManagementVault(ctx, kubeconfigPath, namespace)
	if err != nil {
		return "", err
	}
	return prepareManagementBackupKey(ctx, connection, namespace, vault)
}

func prepareManagementBackupKey(ctx context.Context, connection kubernetes.Connection, namespace string, vault *openbao.Client) (string, error) {
	secret, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	if err != nil {
		return "", errors.New("read management backup recovery root")
	}
	identity, err := managementbackup.RecoveryIdentity(secret.Data["sealKey"])
	if err != nil {
		return "", err
	}
	expected := map[string]string{"version": "1", "identity": identity.String(), "recipient": identity.Recipient().String()}
	entry, err := vault.ReadKv(ctx, "secret", managementBackupKeyPath, 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return "", err
	}
	if entry.Version == 0 {
		if _, err := vault.WriteKv(ctx, "secret", managementBackupKeyPath, openbao.WriteKvInput{Values: expected, CheckVersion: 0}); err != nil {
			// A concurrent reconciler may have committed the same derived key.
			entry, err = vault.ReadKv(ctx, "secret", managementBackupKeyPath, 0)
			if err != nil {
				return "", errors.New("retain management backup encryption key")
			}
		} else {
			return expected["recipient"], nil
		}
	}
	for key, value := range expected {
		if entry.Values[key] != value {
			return "", errors.New("management backup encryption key does not match the original recovery root; refusing to rotate or capture")
		}
	}
	return expected["recipient"], nil
}
