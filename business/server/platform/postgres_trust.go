package platform

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// PostgresTrustInput identifies one database's public certificate in management OpenBao.
type PostgresTrustInput struct {
	// KubeconfigPath selects management Kubernetes, never the database's target cluster.
	KubeconfigPath string
	// Namespace contains the management OpenBao installation.
	Namespace string
	// DatabaseID isolates the certificate by immutable database incarnation.
	DatabaseID string
	// Certificate contains public CA PEM; empty removes this database's trust entry.
	Certificate []byte
}

// ReconcilePostgresTrust publishes a public CA into OpenBao's mounted trust directory.
func ReconcilePostgresTrust(ctx context.Context, input PostgresTrustInput) error {
	if len(input.DatabaseID) != 32 || strings.Trim(input.DatabaseID, "0123456789abcdef") != "" {
		return errors.New("PostgreSQL trust requires an immutable database ID")
	}
	if len(input.Certificate) > 0 && !x509.NewCertPool().AppendCertsFromPEM(input.Certificate) {
		return errors.New("PostgreSQL trust requires a valid public CA certificate")
	}
	if input.Namespace == "" {
		input.Namespace = ManagementNamespace
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return err
	}
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return err
	}
	configMaps := client.CoreV1().ConfigMaps(input.Namespace)
	return retry.OnError(retry.DefaultRetry, func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }, func() error {
		current, err := configMaps.Get(ctx, "borq-postgres-ca", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if len(input.Certificate) == 0 {
				return nil
			}
			_, err = configMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-postgres-ca", Namespace: input.Namespace, Labels: map[string]string{"borq.run/component": "postgres-trust"}}, Data: map[string]string{input.DatabaseID + ".crt": string(input.Certificate)}}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if current.Labels["borq.run/component"] != "postgres-trust" {
			return errors.New("management PostgreSQL trust ConfigMap is not owned by Borq")
		}
		key := input.DatabaseID + ".crt"
		if current.Data[key] == string(input.Certificate) {
			return nil
		}
		if current.Data == nil {
			current.Data = map[string]string{}
		}
		if len(input.Certificate) == 0 {
			delete(current.Data, key)
		} else {
			current.Data[key] = string(input.Certificate)
		}
		_, err = configMaps.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}
