package kubeapps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (client Client) ingressTlsStatus(ctx context.Context, live, desired map[string]any) (string, string, error) {
	issuer := textAt(desired, "metadata", "annotations", "cert-manager.io/cluster-issuer")
	if textAt(live, "metadata", "annotations", "cert-manager.io/cluster-issuer") != issuer {
		return "running", "ingress certificate issuer does not match the applied revision", nil
	}
	entries, _ := valueAt(desired, "spec", "tls").([]any)
	if len(entries) == 0 {
		return "running", "waiting for the ingress TLS configuration", nil
	}
	namespace := textAt(desired, "metadata", "namespace")
	for _, entry := range entries {
		configuration, _ := entry.(map[string]any)
		secretName := textAt(configuration, "secretName")
		hosts := textList(valueAt(configuration, "hosts"))
		if secretName == "" || len(hosts) == 0 {
			return "running", "waiting for the ingress TLS host and certificate reference", nil
		}
		resource := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
		certificate, err := client.dynamic.Resource(resource).Namespace(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return "running", "waiting for certificate creation", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("read ingress Certificate %s/%s: %w", namespace, secretName, err)
		}
		state, message := certificateStatus(certificate.Object, secretName, issuer, hosts)
		if state != "succeeded" {
			return state, message, nil
		}
		resource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
		secret, err := client.dynamic.Resource(resource).Namespace(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return "running", "waiting for the certificate TLS Secret", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("read ingress TLS Secret %s/%s: %w", namespace, secretName, err)
		}
		state, message = tlsSecretStatus(secret.Object, hosts, time.Now())
		if state != "succeeded" {
			return state, message, nil
		}
	}
	return "succeeded", "ingress configuration is accepted and its certificate is ready", nil
}

func certificateStatus(certificate map[string]any, secretName, issuer string, hosts []string) (string, string) {
	issuerGroup := textAt(certificate, "spec", "issuerRef", "group")
	if textAt(certificate, "spec", "secretName") != secretName ||
		textAt(certificate, "spec", "issuerRef", "name") != issuer ||
		textAt(certificate, "spec", "issuerRef", "kind") != "ClusterIssuer" ||
		issuerGroup != "" && issuerGroup != "cert-manager.io" ||
		!sameTexts(textList(valueAt(certificate, "spec", "dnsNames")), hosts) {
		return "running", "certificate configuration does not match the applied ingress"
	}
	generation := integerAt(certificate, "metadata", "generation")
	conditions, _ := valueAt(certificate, "status", "conditions").([]any)
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if textAt(condition, "type") == "Ready" && textAt(condition, "status") == "True" && integerAt(condition, "observedGeneration") >= generation && generation > 0 {
			return "succeeded", "certificate controller has observed the requested certificate"
		}
	}
	return "running", "waiting for the certificate controller to mark this revision ready"
}

func tlsSecretStatus(secret map[string]any, hosts []string, now time.Time) (string, string) {
	if textAt(secret, "type") != "kubernetes.io/tls" {
		return "running", "waiting for a Kubernetes TLS Secret"
	}
	certificateBytes, certificateError := base64.StdEncoding.DecodeString(textAt(secret, "data", "tls.crt"))
	keyBytes, keyError := base64.StdEncoding.DecodeString(textAt(secret, "data", "tls.key"))
	if certificateError != nil || keyError != nil {
		return "running", "TLS Secret does not contain a valid certificate and private key"
	}
	pair, err := tls.X509KeyPair(certificateBytes, keyBytes)
	if err != nil || len(pair.Certificate) == 0 {
		return "running", "TLS Secret does not contain a matching certificate and private key"
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "running", "TLS Secret does not contain a valid certificate"
	}
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return "running", "TLS certificate is outside its validity period"
	}
	for _, host := range hosts {
		if certificate.VerifyHostname(host) != nil {
			return "running", "TLS certificate does not cover the applied ingress host"
		}
	}
	return "succeeded", "TLS certificate and private key are available"
}

func textList(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil
		}
		result = append(result, text)
	}
	return result
}

func sameTexts(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, text := range left {
		counts[text]++
	}
	for _, text := range right {
		counts[text]--
		if counts[text] < 0 {
			return false
		}
	}
	return true
}

func (client Client) managedCertificateStatus(ctx context.Context, live, desired map[string]any) (string, string, error) {
	name := textAt(desired, "spec", "secretName")
	hosts := textList(valueAt(desired, "spec", "dnsNames"))
	issuer := textAt(desired, "spec", "issuerRef", "name")
	state, message := certificateStatus(live, name, issuer, hosts)
	if state != "succeeded" {
		return state, message, nil
	}
	secret, err := client.dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(textAt(desired, "metadata", "namespace")).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "running", "waiting for the certificate TLS Secret", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read Certificate TLS Secret: %w", err)
	}
	state, message = tlsSecretStatus(secret.Object, hosts, time.Now())
	return state, message, nil
}
