package platform

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func ensureVaultKmsTlsSecret(ctx context.Context, connection kubernetes.Connection, namespace string) error {
	secret, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-kms-tls")
	if apierrors.IsNotFound(err) {
		secret, err = newVaultKmsTlsSecret(namespace)
		if err != nil {
			return err
		}
		if err := connection.Secrets().Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		secret, err = connection.Secrets().Get(ctx, namespace, "borq-openbao-kms-tls")
	}
	if err != nil {
		return err
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "borq" {
		return errors.New("OpenBao KMS TLS Secret is not owned by Borq")
	}
	pair, err := tls.X509KeyPair([]byte(secret.Data["tls.crt"]), []byte(secret.Data["tls.key"]))
	if err != nil {
		return errors.New("OpenBao KMS TLS Secret contains an invalid certificate pair")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("OpenBao KMS TLS certificate cannot be parsed")
	}
	if certificate.IsCA {
		return errors.New("OpenBao KMS TLS server certificate must be a non-CA leaf")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(secret.Data["ca.crt"])) {
		return errors.New("OpenBao KMS TLS trust root is missing")
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "openbao-kms." + namespace + ".svc"}); err != nil {
		return errors.New("OpenBao KMS TLS certificate is expired or does not match its service")
	}
	return nil
}

func newVaultKmsTlsSecret(namespace string) (kubernetes.Secret, error) {
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return kubernetes.Secret{}, errors.New("generate OpenBao KMS transport authority")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return kubernetes.Secret{}, errors.New("generate OpenBao KMS transport key")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return kubernetes.Secret{}, errors.New("generate OpenBao KMS certificate identity")
	}
	now := time.Now()
	issuer := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Borq internal KMS transport authority"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}
	authority, err := x509.CreateCertificate(rand.Reader, &issuer, &issuer, &issuerKey.PublicKey, issuerKey)
	if err != nil {
		return kubernetes.Secret{}, errors.New("create OpenBao KMS transport authority")
	}
	leafSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return kubernetes.Secret{}, errors.New("generate OpenBao KMS server identity")
	}
	certificate := x509.Certificate{SerialNumber: leafSerial, Subject: pkix.Name{CommonName: "Borq internal OpenBao KMS"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		DNSNames:              []string{"openbao", "openbao." + namespace, "openbao." + namespace + ".svc", "openbao." + namespace + ".svc.cluster.local", "openbao-kms", "openbao-kms." + namespace, "openbao-kms." + namespace + ".svc", "openbao-kms." + namespace + ".svc.cluster.local", "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	encoded, err := x509.CreateCertificate(rand.Reader, &certificate, &issuer, &key.PublicKey, issuerKey)
	if err != nil {
		return kubernetes.Secret{}, errors.New("create OpenBao KMS certificate")
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return kubernetes.Secret{}, errors.New("encode OpenBao KMS transport key")
	}
	public := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}))
	trust := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority}))
	return kubernetes.Secret{Name: "borq-openbao-kms-tls", Namespace: namespace, Labels: managedLabels(), Data: map[string]string{"ca.crt": trust, "tls.crt": public + trust, "tls.key": string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}))}}, nil
}
