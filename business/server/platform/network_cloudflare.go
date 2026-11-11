package platform

import (
	"context"
	"fmt"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// CloudflareCertificateIssuer identifies Let's Encrypt production issuance through native Cloudflare DNS01.
const CloudflareCertificateIssuer = "letsencrypt-cloudflare"

// The management API token already belongs to this physical cluster. Giving
// cert-manager its own Secret avoids forwarding local challenges through Borq.
func reconcileManagementCloudflare(ctx context.Context, client ctrlclient.Client, settings CloudflareSettings, token []byte) error {
	if len(token) == 0 {
		return fmt.Errorf("management Cloudflare API credential is required")
	}
	if err := ensureDns01Account(ctx, client); err != nil {
		return err
	}
	credential := &corev1.Secret{ObjectMeta: networkMetadata(SystemNamespace, "borq-cloudflare-api-token"),
		Data: map[string][]byte{"api-token": token}}
	if err := ensureNetworkResource(ctx, client, credential); err != nil {
		return err
	}
	if err := ensureNetworkResource(ctx, client, managementCloudflareIssuer(settings)); err != nil {
		return err
	}
	// Tunnel routes use HTTP behind cloudflared and must not wait for ACME.
	// Keep the native issuer available for independent direct service routes.
	if settings.ExposureMode() == "tunnel" {
		return nil
	}
	return waitDns01Issuer(ctx, client)
}

// cert-manager's Kubernetes client requires a pointer to the resource it writes.
func managementCloudflareIssuer(settings CloudflareSettings) *certmanagerv1.ClusterIssuer {
	return &certmanagerv1.ClusterIssuer{ObjectMeta: networkMetadata("", CloudflareCertificateIssuer), Spec: certmanagerv1.IssuerSpec{
		IssuerConfig: certmanagerv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{
			Server: "https://acme-v02.api.letsencrypt.org/directory", Email: settings.AcmeEmail,
			PrivateKey: cmmeta.SecretKeySelector{LocalObjectReference: cmmeta.LocalObjectReference{Name: "borq-letsencrypt-account"}},
			Solvers: []acmev1.ACMEChallengeSolver{{
				Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{settings.WildcardDomain}},
				DNS01: &acmev1.ACMEChallengeSolverDNS01{Cloudflare: &acmev1.ACMEIssuerDNS01ProviderCloudflare{
					APIToken: &cmmeta.SecretKeySelector{LocalObjectReference: cmmeta.LocalObjectReference{Name: "borq-cloudflare-api-token"}, Key: "api-token"},
				}},
			}},
		}},
	}}
}

func waitDns01Issuer(ctx context.Context, client ctrlclient.Client) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetAPIVersion("cert-manager.io/v1")
	issuer.SetKind("ClusterIssuer")
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKey{Name: CloudflareCertificateIssuer}, issuer); err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(issuer.Object, "status", "conditions")
		for _, condition := range conditions {
			entry, ok := condition.(map[string]any)
			if ok && entry["type"] == "Ready" && entry["status"] == "True" && entry["observedGeneration"] == issuer.GetGeneration() {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("wait for Let's Encrypt account readiness: %w", err)
	}
	return nil
}
