package platform

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func ensureDns01Account(ctx context.Context, client ctrlclient.Client) error {
	account := &corev1.Secret{ObjectMeta: networkMetadata(SystemNamespace, "borq-letsencrypt-account")}
	current := &corev1.Secret{}
	err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(account), current)
	if err == nil {
		return checkNetworkOwner(current)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read DNS issuer account Secret")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate DNS issuer account key: %w", err)
	}
	account.Data = map[string][]byte{"tls.key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})}
	if err := client.Create(ctx, account, ctrlclient.FieldOwner("borq-network")); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return networkResourceError(account, err)
		}
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(account), current); err != nil {
			return fmt.Errorf("read DNS issuer account Secret")
		}
		return checkNetworkOwner(current)
	}
	return nil
}
