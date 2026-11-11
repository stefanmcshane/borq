package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Only file references enter Helm values. The actual credentials are persisted
// before OpenBao starts, removing the one-time /sys/init response handoff.
const vaultBootstrapConfig = `seal "static" {
  current_key_id = "borq-v1"
  current_key = "file:///openbao/borq-bootstrap/sealKey"
}
initialize "borq" {
  request "administrator" {
    operation = "update"
    path = "auth/token/create-orphan"
    data = {
      id = {
        eval_source = "file"
        eval_type = "string"
        path = "/openbao/borq-bootstrap/token"
      }
      policies = ["root"]
      no_default_policy = true
      ttl = "0"
    }
  }
}
`

func ensureAdminSecret(ctx context.Context, connection kubernetes.Connection, namespace string) error {
	secret, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	if apierrors.IsNotFound(err) {
		_, claimErr := connection.PersistentVolumeClaims().Get(ctx, namespace, "data-openbao-0")
		if claimErr == nil {
			return errors.New("OpenBao storage exists without its administrator Secret; restore the saved recovery material before continuing")
		}
		if !apierrors.IsNotFound(claimErr) {
			return claimErr
		}
		token, tokenErr := randomCredential()
		if tokenErr != nil {
			return tokenErr
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return errors.New("generate management vault auto-unseal key")
		}
		err = connection.Secrets().Create(ctx, kubernetes.Secret{
			Name: "borq-openbao-admin", Namespace: namespace, Labels: managedLabels(),
			Data: map[string]string{"token": token, "sealKey": base64.StdEncoding.EncodeToString(key), "bootstrap.hcl": vaultBootstrapConfig},
		})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		secret, err = connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	}
	if err != nil {
		return err
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "borq" || secret.Labels["app.kubernetes.io/part-of"] != "borq" {
		return errors.New("management vault administrator Secret is outside Borq ownership; refusing to adopt recovery material")
	}
	if len(secret.Data["token"]) != 64 || strings.Trim(secret.Data["token"], "0123456789abcdef") != "" {
		return errors.New("management vault administrator Secret contains an invalid token; refusing to replace recovery material")
	}
	key, err := base64.StdEncoding.DecodeString(secret.Data["sealKey"])
	if err != nil || len(key) != 32 || secret.Data["bootstrap.hcl"] != vaultBootstrapConfig {
		return errors.New("management vault bootstrap Secret is incomplete or invalid; refusing to replace recovery material")
	}
	return ensureVaultKmsTlsSecret(ctx, connection, namespace)
}
